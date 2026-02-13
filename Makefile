.PHONY: build test test-race vet lint check clean

# Build all binaries
build:
	go build ./...

# Build specific binaries to ./bin/
build-router:
	go build -o bin/auxot-router ./cmd/auxot-router

build-worker:
	go build -o bin/auxot-worker ./cmd/auxot-worker

# Run all tests
test:
	go test ./...

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
