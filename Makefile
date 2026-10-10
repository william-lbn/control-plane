.PHONY: fmt vet test web helm auth functions
fmt:
	cd api && gofmt -w cmd internal
vet:
	cd api && go vet ./...
test:
	bash tools/ci-go.sh
web:
	cd web && npm ci && npm run format:check && npm test && npm run build
helm:
	npm ci --prefix tools --ignore-scripts --no-audit --fund=false
	bash tools/ci-helm.sh
auth:
	cd services/auth && npm ci --ignore-scripts --no-audit --fund=false && npm audit --audit-level=high && npm run typecheck && npm test
	node --test tools/tests/*.test.mjs
functions:
	cd api && go test -race ./internal/functions
	cd services/functions && npm run check && npm test
