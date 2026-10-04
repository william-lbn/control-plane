.PHONY: fmt vet test web helm
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
