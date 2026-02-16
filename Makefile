.PHONY: build test test-integration test-race vet lint check clean sync-registry

# Build all binaries to ./bin/
build: bin/auxot-router bin/auxot-worker

bin/auxot-router: $(shell find cmd/auxot-router internal pkg -name '*.go' 2>/dev/null)
	go build -o bin/auxot-router ./cmd/auxot-router

bin/auxot-worker: $(shell find cmd/auxot-worker internal pkg -name '*.go' 2>/dev/null)
	go build -o bin/auxot-worker ./cmd/auxot-worker

# Run all tests
test:
	go test ./...

# Run integration tests (uses embedded miniredis by default — no external deps)
test-integration:
	go test -tags integration -v -timeout 60s ./tests/integration/

# Run tests with race detector
test-race:
	go test -race ./...

# Run go vet
vet:
	go vet ./...

# Run golangci-lint
lint:
	golangci-lint run

# Run ALL checks (this is what CI runs)
check: vet lint test-race build

# Remove build artifacts
clean:
	rm -rf bin/
	go clean ./...

# Fetch latest registry.json from npm (@auxot/model-registry)
# This downloads the npm tarball and extracts just registry.json — no npm install needed.
# Usage:
#   make sync-registry              # fetch latest version
#   make sync-registry VERSION=1.2.0 # fetch specific version
VERSION ?= latest
sync-registry:
	@echo "Fetching @auxot/model-registry@$(VERSION) from npm..."
	@TARBALL_URL=$$(curl -sL "https://registry.npmjs.org/@auxot/model-registry/$(VERSION)" | \
		python3 -c "import json,sys; print(json.load(sys.stdin)['dist']['tarball'])") && \
	curl -sL "$$TARBALL_URL" | tar xzO package/registry.json > pkg/registry/registry.json && \
	MODEL_COUNT=$$(python3 -c "import json; print(len(json.load(open('pkg/registry/registry.json'))['models']))") && \
	echo "✓ Updated pkg/registry/registry.json ($$MODEL_COUNT models)"

# Cross-compile for all platforms
PLATFORMS := linux/amd64 linux/arm64 darwin/amd64 darwin/arm64

release:
	@for platform in $(PLATFORMS); do \
		os=$$(echo $$platform | cut -d/ -f1); \
		arch=$$(echo $$platform | cut -d/ -f2); \
		echo "Building $$os/$$arch..."; \
		GOOS=$$os GOARCH=$$arch go build -o bin/auxot-router-$$os-$$arch ./cmd/auxot-router; \
		GOOS=$$os GOARCH=$$arch go build -o bin/auxot-worker-$$os-$$arch ./cmd/auxot-worker; \
	done
