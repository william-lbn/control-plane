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
if grep -q '/opt/neon-control/control-worker' artifacts/helm/combined-profile.yaml; then exit 1; fi
helm package charts/neon-control-plane -d artifacts/helm
helm package charts/compute-management-gateway -d artifacts/helm
sha256sum artifacts/helm/*.tgz > artifacts/helm/SHA256SUMS
