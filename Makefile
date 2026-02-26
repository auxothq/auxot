.PHONY: build test test-integration test-race vet lint check clean sync-registry tag-release test-router-url-flag

# Build all binaries to ./bin/
build: bin/auxot-router bin/auxot-worker bin/auxot-tools

bin/auxot-router: $(shell find cmd/auxot-router internal pkg -name '*.go' 2>/dev/null)
	go build -o bin/auxot-router ./cmd/auxot-router

# ensure-registry is phony so it always runs — keeps mmproj_file_name and vision metadata current.
# Prefers monorepo (../../auxot) when present; otherwise fetches from npm.
.PHONY: ensure-registry
ensure-registry:
	@$(MAKE) -s sync-registry

bin/auxot-worker: ensure-registry $(shell find cmd/auxot-worker internal pkg -name '*.go' 2>/dev/null)
	go build -o bin/auxot-worker ./cmd/auxot-worker

bin/auxot-tools: $(shell find cmd/auxot-tools internal/tools pkg/tools pkg/protocol -name '*.go' 2>/dev/null)
	go build -o bin/auxot-tools ./cmd/auxot-tools

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

# Verify that --router-url flag overrides env (catches line-splitting / config bugs)
test-router-url-flag: bin/auxot-tools
	@out=$$(./bin/auxot-tools --tools-key fakekey --router-url ws://localhost:9999/ws 2>&1 | head -1); \
	if echo "$$out" | grep -q 'router_url":"ws://localhost:9999/ws'; then \
		echo "✓ --router-url flag override works"; \
	else \
		echo "✗ FAIL: expected router_url with 9999, got: $$out"; exit 1; \
	fi

# Remove build artifacts
clean:
	rm -rf bin/
	go clean ./...

# Fetch latest registry.json. Prefers local monorepo (../../auxot) when present,
# otherwise fetches from npm (@auxot/model-registry). Ensures mmproj_file_name
# and vision capabilities are included for vision models.
# Usage:
#   make sync-registry              # fetch latest (monorepo or npm)
#   make sync-registry VERSION=1.2.0 # fetch specific version from npm (ignores monorepo)
#   make sync-registry FROM=npm      # force npm even if monorepo exists
FROM ?= auto
VERSION ?= latest
sync-registry:
	@MONOREPO=../../auxot/packages/model-registry/registry.json; \
	if [ "$(FROM)" = "auto" ] && [ -f "$$MONOREPO" ]; then \
		DEST=pkg/registry/registry.json; \
		if [ ! -f "$$DEST" ] || [ "$$MONOREPO" -nt "$$DEST" ]; then \
			echo "Copying registry from monorepo ($$MONOREPO)..."; \
			cp "$$MONOREPO" "$$DEST"; \
		fi; \
	else \
		echo "Fetching @auxot/model-registry@$(VERSION) from npm..."; \
		TARBALL_URL=$$(curl -sL "https://registry.npmjs.org/@auxot/model-registry/$(VERSION)" | \
			python3 -c "import json,sys; print(json.load(sys.stdin)['dist']['tarball'])") && \
		curl -sL "$$TARBALL_URL" | tar xzO package/registry.json > pkg/registry/registry.json; \
	fi && \
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
