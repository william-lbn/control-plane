#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "$0")/.."
mkdir -p artifacts/helm
helm lint charts/neon-control-plane --strict -f charts/neon-control-plane/ci/render-values.yaml
helm template neon-control charts/neon-control-plane --namespace neon -f charts/neon-control-plane/ci/render-values.yaml > artifacts/helm/control-plane.yaml
helm template combined charts/neon-control-plane --namespace neon -f charts/neon-control-plane/ci/render-values.yaml --set worker.enabled=false > artifacts/helm/combined-profile.yaml
helm lint charts/compute-management-gateway --strict -f charts/compute-management-gateway/ci/render-values.yaml
helm template neon-gateway charts/compute-management-gateway --namespace neon -f charts/compute-management-gateway/ci/render-values.yaml > artifacts/helm/gateway.yaml
# These are security/reconciliation gates, not successful deployment evidence.
if helm template bad charts/neon-control-plane -f charts/neon-control-plane/ci/render-values.yaml --set api.replicaCount=2 > artifacts/helm/rejected-replicas.log 2>&1; then exit 1; fi
if helm template bad charts/neon-control-plane -f charts/neon-control-plane/ci/render-values.yaml --set worker.replicaCount=2 > artifacts/helm/rejected-worker-replicas.log 2>&1; then exit 1; fi
if helm template bad charts/neon-control-plane -f charts/neon-control-plane/ci/render-values.yaml --set api.pgCASecret= > artifacts/helm/rejected-ca.log 2>&1; then exit 1; fi
if helm template bad charts/neon-control-plane -f charts/neon-control-plane/ci/render-values.yaml --set api.image.reference=example.invalid/control-api:latest > artifacts/helm/rejected-tag.log 2>&1; then exit 1; fi
if grep -q hostPath artifacts/helm/gateway.yaml; then echo 'Image profile must not mount a host binary'; exit 1; fi
grep -q '/opt/neon-control/control-worker' artifacts/helm/control-plane.yaml
helm template restore charts/neon-control-plane --namespace neon -f charts/neon-control-plane/ci/render-values.yaml --set api.pitrEnabled=true --set api.creationEnabled=true > artifacts/helm/historical-restore.yaml
test "$(grep -c NEON_V2_PITR_ENABLED artifacts/helm/historical-restore.yaml)" -eq 2
if helm template bad charts/neon-control-plane -f charts/neon-control-plane/ci/render-values.yaml --set api.pitrEnabled=true --set api.creationEnabled=false > artifacts/helm/rejected-restore-creation.log 2>&1; then exit 1; fi
if grep -q '/opt/neon-control/control-worker' artifacts/helm/combined-profile.yaml; then exit 1; fi
helm package charts/neon-control-plane -d artifacts/helm
helm package charts/compute-management-gateway -d artifacts/helm
helm lint charts/data-api-gateway --strict -f charts/data-api-gateway/ci/render-values.yaml
helm template neon-data charts/data-api-gateway --namespace neon -f charts/data-api-gateway/ci/render-values.yaml > artifacts/helm/data-api-gateway.yaml
if helm template bad charts/data-api-gateway -f charts/data-api-gateway/ci/render-values.yaml --set image.reference=example.invalid/control-dataapi:latest > artifacts/helm/rejected-data-api-tag.log 2>&1; then exit 1; fi
if helm template bad charts/data-api-gateway -f charts/data-api-gateway/ci/render-values.yaml --set ingress.enabled=false > artifacts/helm/rejected-data-api-http.log 2>&1; then exit 1; fi
if helm template bad charts/data-api-gateway -f charts/data-api-gateway/ci/render-values.yaml --set replicaCount=2 > artifacts/helm/rejected-data-api-replicas.log 2>&1; then exit 1; fi
helm package charts/data-api-gateway -d artifacts/helm
helm template native-data charts/neon-control-plane --namespace neon -f charts/neon-control-plane/ci/render-values.yaml \
  --set dataAPI.enabled=true --set dataAPI.labHTTP=true \
  --set "dataAPI.gatewayImage=example.invalid/gateway@sha256:$(printf 'a%.0s' {1..64})" \
  --set "dataAPI.postgrestImage=example.invalid/postgrest@sha256:$(printf 'b%.0s' {1..64})" > artifacts/helm/native-data-api.yaml
if helm template bad charts/neon-control-plane -f charts/neon-control-plane/ci/render-values.yaml --set dataAPI.enabled=true > artifacts/helm/rejected-native-data-api-default.log 2>&1; then exit 1; fi
if helm template bad charts/neon-control-plane -f charts/neon-control-plane/ci/render-values.yaml --set dataAPI.enabled=true --set dataAPI.labHTTP=true --set dataAPI.gatewayImage=example.invalid/gateway:latest > artifacts/helm/rejected-native-data-api-tag.log 2>&1; then exit 1; fi
helm template credentials charts/neon-control-plane --namespace neon -f charts/neon-control-plane/ci/render-values.yaml \
  --set backendCredentials.enabled=true --set backendCredentials.labHTTP=true --set backendCredentials.existingSecret=backend-keys > artifacts/helm/backend-credentials.yaml
grep -q 'NEON_BACKEND_CREDENTIAL_KEYS_FILE' artifacts/helm/backend-credentials.yaml
if helm template bad charts/neon-control-plane -f charts/neon-control-plane/ci/render-values.yaml --set backendCredentials.enabled=true --set backendCredentials.labHTTP=true > artifacts/helm/rejected-backend-secret.log 2>&1; then exit 1; fi
if helm template bad charts/neon-control-plane -f charts/neon-control-plane/ci/render-values.yaml --set backendCredentials.enabled=true --set backendCredentials.existingSecret=backend-keys --set api.cookieSecure=false > artifacts/helm/rejected-backend-http.log 2>&1; then exit 1; fi
sha256sum artifacts/helm/*.tgz > artifacts/helm/SHA256SUMS
helm template native-auth charts/neon-control-plane --namespace neon -f charts/neon-control-plane/ci/render-values.yaml \
  --set managedAuth.enabled=true --set managedAuth.labHTTP=true \
  --set managedAuth.publicOrigin=http://192.0.2.1:30788 \
  --set "managedAuth.runtimeImage=example.invalid/auth@sha256:$(printf 'c%.0s' {1..64})" > artifacts/helm/native-auth.yaml
grep -q 'NEON_AUTH_RUNTIME_IMAGE' artifacts/helm/native-auth.yaml
if helm template bad charts/neon-control-plane -f charts/neon-control-plane/ci/render-values.yaml --set managedAuth.enabled=true > artifacts/helm/rejected-auth-default.log 2>&1; then exit 1; fi
if helm template bad charts/neon-control-plane -f charts/neon-control-plane/ci/render-values.yaml --set managedAuth.enabled=true --set managedAuth.labHTTP=true --set managedAuth.publicOrigin=http://192.0.2.1:30788 --set managedAuth.runtimeImage=example.invalid/auth:latest > artifacts/helm/rejected-auth-tag.log 2>&1; then exit 1; fi
