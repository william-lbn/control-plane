.PHONY: fmt vet test web helm auth
fmt:
	cd api && gofmt -w cmd internal
vet:
	cd api && go vet ./...
test:
	bash tools/ci-go.sh
web:
	cd web && npm ci && npm run format:check && npm test && npm run build
helm:
	bash tools/ci-helm.sh
auth:
	cd services/auth && npm ci --ignore-scripts --no-audit --fund=false && npm audit --audit-level=high && npm run typecheck && npm test
	node --test tools/tests/*.test.mjs
