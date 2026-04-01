.PHONY: build test test-integration test-race vet lint check clean sync-registry tag tag-release test-router-url-flag

WEB_REPO      := $(HOME)/src/auxot/auxot
CHANGELOG_DIR := $(WEB_REPO)/apps/web/src/content/changelog/oss

# Build all binaries to ./bin/
build: bin/auxot-router bin/auxot-worker bin/auxot-tools bin/auxot-agent

bin/auxot-router: $(shell find cmd/auxot-router internal pkg -name '*.go' 2>/dev/null)
	go build -o bin/auxot-router ./cmd/auxot-router

# ensure-registry is phony so it always runs — keeps the Go-embedded copy current.
.PHONY: ensure-registry
ensure-registry:
	@$(MAKE) -s sync-registry

bin/auxot-worker: ensure-registry $(shell find cmd/auxot-worker internal pkg -name '*.go' 2>/dev/null)
	go build -o bin/auxot-worker ./cmd/auxot-worker

bin/auxot-tools: $(shell find cmd/auxot-tools internal/tools pkg/tools pkg/protocol -name '*.go' 2>/dev/null)
	go build -o bin/auxot-tools ./cmd/auxot-tools

bin/auxot-agent: $(shell find cmd/auxot-agent internal/agentworker pkg -name '*.go' 2>/dev/null)
	go build -o bin/auxot-agent ./cmd/auxot-agent

# Linux ELF for bind-mounting into Docker (default GOARCH = host `go env GOARCH`).
# Override if the container platform differs, e.g. AUXOT_AGENT_LINUX_GOARCH=amd64 on arm64 Mac.
bin/auxot-agent.linux: $(shell find cmd/auxot-agent internal/agentworker pkg -name '*.go' 2>/dev/null)
	GOOS=linux GOARCH=$(or $(AUXOT_AGENT_LINUX_GOARCH),$(shell go env GOARCH)) CGO_ENABLED=0 go build -o bin/auxot-agent.linux ./cmd/auxot-agent

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

# Sync registry.json from npm/model-registry/ into pkg/registry/ (Go embed source).
# The canonical copy lives at npm/model-registry/registry.json (same repo).
# Usage:
#   make sync-registry              # copy from npm/model-registry/ (default)
#   make sync-registry FROM=npm     # fetch from npmjs.com instead
#   make sync-registry VERSION=1.2.0 # fetch specific version from npmjs.com
FROM ?= auto
VERSION ?= latest
sync-registry:
	@SRC=npm/model-registry/registry.json; \
	DEST=pkg/registry/registry.json; \
	if [ "$(FROM)" = "npm" ] || [ "$(VERSION)" != "latest" ]; then \
		echo "Fetching @auxot/model-registry@$(VERSION) from npm..."; \
		TARBALL_URL=$$(curl -sL "https://registry.npmjs.org/@auxot/model-registry/$(VERSION)" | \
			python3 -c "import json,sys; print(json.load(sys.stdin)['dist']['tarball'])") && \
		curl -sL "$$TARBALL_URL" | tar xzO package/registry.json > "$$DEST" && \
		cp "$$DEST" "$$SRC"; \
	elif [ -f "$$SRC" ]; then \
		if [ ! -f "$$DEST" ] || [ "$$SRC" -nt "$$DEST" ]; then \
			cp "$$SRC" "$$DEST"; \
		fi; \
	else \
		echo "Error: $$SRC not found. Run from repo root."; exit 1; \
	fi && \
	MODEL_COUNT=$$(python3 -c "import json; print(len(json.load(open('$$DEST'))['models']))") && \
	echo "✓ Updated $$DEST ($$MODEL_COUNT models)"

# Bump patch version and tag HEAD — triggers GoReleaser + Docker build via GitHub Actions.
# Finds the latest v* tag and creates vX.Y.(Z+1) on HEAD.
tag:
	@LATEST=$$(git describe --tags --abbrev=0 --match "v*" 2>/dev/null || echo "v0.0.0"); \
	MAJOR=$$(echo $$LATEST | sed 's/v\([0-9]*\)\..*/\1/'); \
	MINOR=$$(echo $$LATEST | sed 's/v[0-9]*\.\([0-9]*\)\..*/\1/'); \
	PATCH=$$(echo $$LATEST | sed 's/v[0-9]*\.[0-9]*\.\([0-9]*\).*/\1/'); \
	NEXT="v$$MAJOR.$$MINOR.$$((PATCH + 1))"; \
	echo "Latest: $$LATEST  →  $$NEXT"; \
	git tag "$$NEXT" && \
	git push origin "$$NEXT" && \
	echo "✓ Tagged: $$NEXT — watch: https://github.com/auxothq/auxot/actions"; \
	echo ""; \
	echo "Generating changelog…"; \
	COMMITS=$$(git log $$LATEST..$$NEXT --pretty=format:"- %s (%h)" --no-merges); \
	DATE=$$(date -u +%Y-%m-%d); \
	mkdir -p $(CHANGELOG_DIR); \
	OUTFILE="$(CHANGELOG_DIR)/$${NEXT#v}.mdx"; \
	claude --print -p "You are writing a changelog entry for Auxot (open-source edition), an AI agent orchestration platform — the OSS router, workers, and CLI. \
Given these git commits from version $$NEXT ($$DATE), produce a complete MDX file with frontmatter and body. \
\
Frontmatter format (YAML between --- delimiters): \
  product: oss \
  version: \"$${NEXT#v}\" \
  date: \"$$DATE\" \
  title: (a short descriptive title for this release) \
  summary: (one sentence summary) \
\
Body rules: \
- Group changes under ## headings: Features, Improvements, Fixes, Internal (only include headings that have entries) \
- Write in past tense, user-facing language \
- Collapse related commits into single bullet points \
- Skip trivial commits (typos, formatting) \
- Keep it concise \
\
Commits: \
$$COMMITS" > "$$OUTFILE"; \
	echo "✓ Changelog: $$OUTFILE (unstaged — review and commit to web repo)"

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
	echo ""; \
	echo "Generating changelog…"; \
	LATEST=$$(git describe --tags --abbrev=0 --match "v*" --exclude "v$(V)" 2>/dev/null || echo "v0.0.0"); \
	COMMITS=$$(git log $$LATEST..v$(V) --pretty=format:"- %s (%h)" --no-merges); \
	DATE=$$(date -u +%Y-%m-%d); \
	mkdir -p $(CHANGELOG_DIR); \
	OUTFILE="$(CHANGELOG_DIR)/$(V).mdx"; \
	claude --print -p "You are writing a changelog entry for Auxot (open-source edition), an AI agent orchestration platform — the OSS router, workers, and CLI. \
Given these git commits from version v$(V) ($$DATE), produce a complete MDX file with frontmatter and body. \
\
Frontmatter format (YAML between --- delimiters): \
  product: oss \
  version: \"$(V)\" \
  date: \"$$DATE\" \
  title: (a short descriptive title for this release) \
  summary: (one sentence summary) \
\
Body rules: \
- Group changes under ## headings: Features, Improvements, Fixes, Internal (only include headings that have entries) \
- Write in past tense, user-facing language \
- Collapse related commits into single bullet points \
- Skip trivial commits (typos, formatting) \
- Keep it concise \
\
Commits: \
$$COMMITS" > "$$OUTFILE"; \
	echo "✓ Changelog: $$OUTFILE (unstaged — review and commit to web repo)"
