#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "$0")/.."
# helm_template默认模拟Kubernetes 1.27；本项目Chart最低要求1.30。
# 固定使用当前集群/CI验收基线，避免不同Helm版本产生不同默认Capabilities。
KUBE_VERSION="$(printenv NEON_HELM_KUBE_VERSION || true)"
if [[ -z "$KUBE_VERSION" ]]; then KUBE_VERSION=1.31.2; fi
helm_template() { helm template "$@" --kube-version "$KUBE_VERSION"; }
mkdir -p artifacts/helm
helm lint charts/neon-control-plane --strict -f charts/neon-control-plane/ci/render-values.yaml
helm_template neon-control charts/neon-control-plane --namespace neon -f charts/neon-control-plane/ci/render-values.yaml > artifacts/helm/control-plane.yaml
# API-hosted Console is a supported isolated profile: omit nginx/Web Service,
# serve the immutable SPA from the API image, and route optional Ingress to API.
helm_template api-hosted-web charts/neon-control-plane --namespace neon \
  -f charts/neon-control-plane/ci/render-values.yaml \
  --set web.enabled=false --set ingress.enabled=true \
  --set ingress.host=control-r4.example.test > artifacts/helm/api-hosted-web.yaml
node tools/check-web-api-hosting.mjs artifacts/helm/api-hosted-web.yaml
helm_template combined charts/neon-control-plane --namespace neon -f charts/neon-control-plane/ci/render-values.yaml --set worker.enabled=false > artifacts/helm/combined-profile.yaml
node tools/check-helm-rbac.mjs artifacts/helm/control-plane.yaml artifacts/helm/combined-profile.yaml
node --test tools/helm-tests/*.test.mjs
helm lint charts/compute-management-gateway --strict -f charts/compute-management-gateway/ci/render-values.yaml
helm_template neon-gateway charts/compute-management-gateway --namespace neon -f charts/compute-management-gateway/ci/render-values.yaml > artifacts/helm/gateway.yaml
# These are security/reconciliation gates, not successful deployment evidence.
if helm_template bad charts/neon-control-plane -f charts/neon-control-plane/ci/render-values.yaml --set api.replicaCount=2 > artifacts/helm/rejected-replicas.log 2>&1; then exit 1; fi
if helm_template bad charts/neon-control-plane -f charts/neon-control-plane/ci/render-values.yaml --set worker.replicaCount=2 > artifacts/helm/rejected-worker-replicas.log 2>&1; then exit 1; fi
if helm_template bad charts/neon-control-plane -f charts/neon-control-plane/ci/render-values.yaml --set api.pgCASecret= > artifacts/helm/rejected-ca.log 2>&1; then exit 1; fi
if helm_template bad charts/neon-control-plane -f charts/neon-control-plane/ci/render-values.yaml --set api.image.reference=example.invalid/control-api:latest > artifacts/helm/rejected-tag.log 2>&1; then exit 1; fi
if grep -q hostPath artifacts/helm/gateway.yaml; then echo 'Image profile must not mount a host binary'; exit 1; fi
grep -q '/opt/neon-control/control-worker' artifacts/helm/control-plane.yaml
# DSGA PG17/PG18 对外创建必须同时满足平台独立开关与经运维审核的
# 发行目录 Secret；默认部署只输出关闭态，不能把配置缺失误当可创建。
test "$(grep -c 'NEON_DSGA_CREATE_ENABLED.*value: "false"' artifacts/helm/control-plane.yaml)" -eq 1
if grep -q NEON_DSGA_RUNTIME_CATALOG artifacts/helm/control-plane.yaml; then
  echo 'DSGA runtime catalog leaked into default deployment'; exit 1
fi
helm_template dsga-runtime charts/neon-control-plane --namespace neon  \
  -f charts/neon-control-plane/ci/render-values.yaml \
  --set dsga.runtimeCatalogSecret=neon-dsga-runtime-catalog \
  --set dsga.createEnabled=true > artifacts/helm/dsga-runtime.yaml
# 发行目录只注入 API；Worker 只恢复已冻结操作，不应读取当前候选目录。
test "$(grep -c 'NEON_DSGA_CREATE_ENABLED.*value: "true"' artifacts/helm/dsga-runtime.yaml)" -eq 1
test "$(grep -c NEON_DSGA_RUNTIME_CATALOG artifacts/helm/dsga-runtime.yaml)" -eq 1
test "$(grep -c 'key: catalog.json' artifacts/helm/dsga-runtime.yaml)" -eq 1
helm_template dsga-combined charts/neon-control-plane --namespace neon  \
  -f charts/neon-control-plane/ci/render-values.yaml --set worker.enabled=false \
  --set dsga.runtimeCatalogSecret=neon-dsga-runtime-catalog \
  --set dsga.createEnabled=true > artifacts/helm/dsga-combined.yaml
test "$(grep -c NEON_DSGA_RUNTIME_CATALOG artifacts/helm/dsga-combined.yaml)" -eq 1
# DSGA 开启态必须为 API/Worker 分别授予最小跨 namespace 权限；
# 仅允许固定 Node-read 与 Runner-delegation 两个 ClusterRoleBinding。
node tools/check-dsga-rbac.mjs artifacts/helm/dsga-runtime.yaml artifacts/helm/dsga-combined.yaml
if helm_template bad charts/neon-control-plane --namespace default \
  -f charts/neon-control-plane/ci/render-values.yaml \
  --set dsga.runtimeCatalogSecret=neon-dsga-runtime-catalog \
  --set dsga.createEnabled=true > artifacts/helm/rejected-dsga-namespace.log 2>&1; then exit 1; fi
# 反向门禁：缺少可信目录、全局创建关闭或 Secret 名不合法时必须拒绝渲染。
if helm_template bad charts/neon-control-plane  \
  -f charts/neon-control-plane/ci/render-values.yaml \
  --set dsga.createEnabled=true > artifacts/helm/rejected-dsga-no-catalog.log 2>&1; then exit 1; fi
if helm_template bad charts/neon-control-plane  \
  -f charts/neon-control-plane/ci/render-values.yaml \
  --set dsga.runtimeCatalogSecret=neon-dsga-runtime-catalog \
  --set dsga.createEnabled=true --set api.creationEnabled=false \
  > artifacts/helm/rejected-dsga-global-disabled.log 2>&1; then exit 1; fi
if helm_template bad charts/neon-control-plane  \
  -f charts/neon-control-plane/ci/render-values.yaml \
  --set dsga.runtimeCatalogSecret=BAD_SECRET \
  > artifacts/helm/rejected-dsga-bad-secret.log 2>&1; then exit 1; fi
# Kubernetes Secret 名称的每个 DNS 标签最多 63 字符，不能只靠 253 字符总长。
# 64 字符单标签在渲染阶段必须拒绝，避免到集群部署时才因 Secret 名非法失败。
if helm_template bad charts/neon-control-plane  \
  -f charts/neon-control-plane/ci/render-values.yaml \
  --set "dsga.runtimeCatalogSecret=$(printf 'a%.0s' {1..64})" \
  > artifacts/helm/rejected-dsga-overlong-label.log 2>&1; then exit 1; fi
helm_template restore charts/neon-control-plane --namespace neon -f charts/neon-control-plane/ci/render-values.yaml --set api.pitrEnabled=true --set api.creationEnabled=true > artifacts/helm/historical-restore.yaml
test "$(grep -c NEON_V2_PITR_ENABLED artifacts/helm/historical-restore.yaml)" -eq 2
if helm_template bad charts/neon-control-plane -f charts/neon-control-plane/ci/render-values.yaml --set api.pitrEnabled=true --set api.creationEnabled=false > artifacts/helm/rejected-restore-creation.log 2>&1; then exit 1; fi
if grep -q '/opt/neon-control/control-worker' artifacts/helm/combined-profile.yaml; then exit 1; fi
helm package charts/neon-control-plane -d artifacts/helm
helm package charts/compute-management-gateway -d artifacts/helm
helm lint charts/data-api-gateway --strict -f charts/data-api-gateway/ci/render-values.yaml
helm_template neon-data charts/data-api-gateway --namespace neon -f charts/data-api-gateway/ci/render-values.yaml > artifacts/helm/data-api-gateway.yaml
if helm_template bad charts/data-api-gateway -f charts/data-api-gateway/ci/render-values.yaml --set image.reference=example.invalid/control-dataapi:latest > artifacts/helm/rejected-data-api-tag.log 2>&1; then exit 1; fi
if helm_template bad charts/data-api-gateway -f charts/data-api-gateway/ci/render-values.yaml --set ingress.enabled=false > artifacts/helm/rejected-data-api-http.log 2>&1; then exit 1; fi
if helm_template bad charts/data-api-gateway -f charts/data-api-gateway/ci/render-values.yaml --set replicaCount=2 > artifacts/helm/rejected-data-api-replicas.log 2>&1; then exit 1; fi
helm package charts/data-api-gateway -d artifacts/helm
helm_template native-data charts/neon-control-plane --namespace neon -f charts/neon-control-plane/ci/render-values.yaml \
  --set dataAPI.enabled=true --set dataAPI.labHTTP=true \
  --set "dataAPI.gatewayImage=example.invalid/gateway@sha256:$(printf 'a%.0s' {1..64})" \
  --set "dataAPI.postgrestImage=example.invalid/postgrest@sha256:$(printf 'b%.0s' {1..64})" > artifacts/helm/native-data-api.yaml
if helm_template bad charts/neon-control-plane -f charts/neon-control-plane/ci/render-values.yaml --set dataAPI.enabled=true > artifacts/helm/rejected-native-data-api-default.log 2>&1; then exit 1; fi
if helm_template bad charts/neon-control-plane -f charts/neon-control-plane/ci/render-values.yaml --set dataAPI.enabled=true --set dataAPI.labHTTP=true --set dataAPI.gatewayImage=example.invalid/gateway:latest > artifacts/helm/rejected-native-data-api-tag.log 2>&1; then exit 1; fi
helm_template credentials charts/neon-control-plane --namespace neon -f charts/neon-control-plane/ci/render-values.yaml \
  --set backendCredentials.enabled=true --set backendCredentials.labHTTP=true --set backendCredentials.existingSecret=backend-keys > artifacts/helm/backend-credentials.yaml
grep -q 'NEON_BACKEND_CREDENTIAL_KEYS_FILE' artifacts/helm/backend-credentials.yaml
if helm_template bad charts/neon-control-plane -f charts/neon-control-plane/ci/render-values.yaml --set backendCredentials.enabled=true --set backendCredentials.labHTTP=true > artifacts/helm/rejected-backend-secret.log 2>&1; then exit 1; fi
if helm_template bad charts/neon-control-plane -f charts/neon-control-plane/ci/render-values.yaml --set backendCredentials.enabled=true --set backendCredentials.existingSecret=backend-keys --set api.cookieSecure=false > artifacts/helm/rejected-backend-http.log 2>&1; then exit 1; fi
# Independent Storage is distributed with its own digest-pinned image and chart.
helm lint charts/storage-gateway --strict -f charts/storage-gateway/ci/render-values.yaml
helm_template neon-storage charts/storage-gateway --namespace neon -f charts/storage-gateway/ci/render-values.yaml > artifacts/helm/storage-gateway.yaml
helm package charts/storage-gateway -d artifacts/helm
sha256sum artifacts/helm/*.tgz > artifacts/helm/SHA256SUMS
helm_template native-auth charts/neon-control-plane --namespace neon -f charts/neon-control-plane/ci/render-values.yaml \
  --set managedAuth.enabled=true --set managedAuth.labHTTP=true \
  --set managedAuth.publicOrigin=http://192.0.2.1:30788 --set managedAuth.pgCASecret=proxy-public-ca --set managedAuth.pgServerName=proxy.example.test \
  --set "managedAuth.runtimeImage=example.invalid/auth@sha256:$(printf 'c%.0s' {1..64})" > artifacts/helm/native-auth.yaml
grep -q 'NEON_AUTH_RUNTIME_IMAGE' artifacts/helm/native-auth.yaml
if helm_template bad charts/neon-control-plane -f charts/neon-control-plane/ci/render-values.yaml --set managedAuth.enabled=true > artifacts/helm/rejected-auth-default.log 2>&1; then exit 1; fi
if helm_template bad charts/neon-control-plane -f charts/neon-control-plane/ci/render-values.yaml --set managedAuth.enabled=true --set managedAuth.labHTTP=true --set managedAuth.publicOrigin=http://192.0.2.1:30788 --set managedAuth.runtimeImage=example.invalid/auth:latest > artifacts/helm/rejected-auth-tag.log 2>&1; then exit 1; fi

if helm_template bad charts/neon-control-plane -f charts/neon-control-plane/ci/render-values.yaml --set unknownSetting=true > artifacts/helm/rejected-unknown.log 2>&1; then exit 1; fi
