.PHONY: build test test-integration test-race vet lint check clean sync-registry tag-release

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

# Tag a release — triggers GoReleaser + Docker build via GitHub Actions.
# Usage: make tag-release V=0.1.0
tag-release:
ifndef V
	$(error Usage: make tag-release V=0.1.0)
endif
	@echo "" && \
	echo "  Tagging v$(V)..." && \
	git tag "v$(V)" && \
	git push origin "v$(V)" && \
	echo "" && \
	echo "  ✓ Tagged: v$(V)" && \
	echo "  GoReleaser + Docker build started." && \
	echo "  Watch: https://github.com/auxothq/auxot/actions" && \
	echo ""
