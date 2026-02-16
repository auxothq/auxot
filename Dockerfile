# Auxot Router — FROM scratch production image
#
# Supports multi-arch builds via docker buildx:
#   docker buildx build --platform linux/amd64,linux/arm64 .

# ============================================================================
# Builder
# ============================================================================
FROM --platform=$BUILDPLATFORM golang:1.25-alpine AS builder

ARG TARGETOS TARGETARCH

RUN apk add --no-cache git make curl tar python3

WORKDIR /build

COPY go.mod go.sum ./
RUN go mod download

COPY . .

# Fetch model registry from npm (embedded in the binary at compile time)
RUN make sync-registry

# Static binary — no libc, no CGO, works with FROM scratch
RUN CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} go build \
    -ldflags="-s -w" \
    -o /build/auxot-router \
    ./cmd/auxot-router

# ============================================================================
# Production (FROM scratch — just the binary, nothing else)
# ============================================================================
FROM scratch

COPY --from=builder /build/auxot-router /auxot-router

EXPOSE 8080

ENTRYPOINT ["/auxot-router"]