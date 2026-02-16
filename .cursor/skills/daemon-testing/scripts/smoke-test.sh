#!/usr/bin/env bash
#
# smoke-test.sh — End-to-end smoke test for auxot-router + auxot-worker.
#
# Prerequisites:
#   1. make build              (builds bin/auxot-router and bin/auxot-worker)
#   2. .env file with keys     (bin/auxot-router setup)
#
# No external Redis needed — the router uses embedded miniredis.
#
# Usage:
#   ./scripts/smoke-test.sh              # from repo root
#   AUXOT_LOG_LEVEL=debug ./scripts/smoke-test.sh   # verbose
#
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/../../.." && pwd)"
cd "$REPO_ROOT"

# Colors (if terminal supports them)
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
NC='\033[0m' # No Color

info()  { echo -e "${GREEN}[INFO]${NC}  $*"; }
warn()  { echo -e "${YELLOW}[WARN]${NC}  $*"; }
fail()  { echo -e "${RED}[FAIL]${NC}  $*"; exit 1; }

cleanup() {
    info "Cleaning up..."
    [ -n "${WORKER_PID:-}" ] && kill "$WORKER_PID" 2>/dev/null && wait "$WORKER_PID" 2>/dev/null || true
    [ -n "${ROUTER_PID:-}" ] && kill "$ROUTER_PID" 2>/dev/null && wait "$ROUTER_PID" 2>/dev/null || true
    # Kill any stale llama-server processes spawned by the worker
    pkill -f "llama-server.*--port" 2>/dev/null || true
    sleep 1
}
trap cleanup EXIT

# --- Preflight ---
info "Preflight checks..."

[ -f bin/auxot-router ] || fail "bin/auxot-router not found. Run: make build"
[ -f bin/auxot-worker ] || fail "bin/auxot-worker not found. Run: make build"
[ -f .env ]             || fail ".env not found. Run: bin/auxot-router setup --write-env"

# Load environment
set -a
source .env
set +a

[ -n "${AUXOT_API_KEY:-}" ]       || fail "AUXOT_API_KEY not set in .env"
[ -n "${AUXOT_GPU_KEY:-}" ]       || fail "AUXOT_GPU_KEY not set in .env"
[ -n "${AUXOT_ADMIN_KEY_HASH:-}" ] || fail "AUXOT_ADMIN_KEY_HASH not set in .env"

info "All preflight checks passed."

# --- Start Router ---
info "Starting router (embedded Redis)..."
bin/auxot-router &
ROUTER_PID=$!
sleep 2

# Verify router is up
if ! kill -0 "$ROUTER_PID" 2>/dev/null; then
    fail "Router exited immediately. Check configuration."
fi
info "Router started (PID $ROUTER_PID)"

# --- Start Worker ---
info "Starting worker..."
info "This may take a while if model/llama.cpp need to be downloaded..."
bin/auxot-worker &
WORKER_PID=$!

# Wait for worker to report ready (poll for up to 120 seconds)
info "Waiting for worker to be ready (up to 120s)..."
READY=false
for i in $(seq 1 120); do
    if ! kill -0 "$WORKER_PID" 2>/dev/null; then
        fail "Worker exited unexpectedly. Check logs above."
    fi
    # Check if any llama-server process is listening (worker spawns it)
    if pgrep -f "llama-server.*--port" >/dev/null 2>&1; then
        sleep 5  # Give it a few more seconds to finish capability discovery
        READY=true
        break
    fi
    sleep 1
done

if [ "$READY" = false ]; then
    fail "Worker did not become ready within 120 seconds."
fi
info "Worker appears ready (PID $WORKER_PID)"

# --- Test: Non-streaming completion ---
info "Testing non-streaming completion..."
RESPONSE=$(curl -s --max-time 60 http://localhost:8080/api/openai/v1/chat/completions \
    -H "Authorization: Bearer $AUXOT_API_KEY" \
    -H "Content-Type: application/json" \
    -d '{
        "model": "test",
        "messages": [{"role": "user", "content": "Say exactly: hello world"}],
        "stream": false,
        "max_tokens": 20
    }')

if echo "$RESPONSE" | jq -e '.choices[0].message.content' >/dev/null 2>&1; then
    CONTENT=$(echo "$RESPONSE" | jq -r '.choices[0].message.content')
    info "Non-streaming OK: $CONTENT"
else
    warn "Non-streaming response: $RESPONSE"
    fail "Non-streaming completion did not return expected format."
fi

# --- Test: Streaming completion ---
info "Testing streaming completion..."
STREAM_OUTPUT=$(curl -s -N --max-time 60 http://localhost:8080/api/openai/v1/chat/completions \
    -H "Authorization: Bearer $AUXOT_API_KEY" \
    -H "Content-Type: application/json" \
    -d '{
        "model": "test",
        "messages": [{"role": "user", "content": "Count: 1 2 3"}],
        "stream": true,
        "max_tokens": 30
    }' 2>&1)

if echo "$STREAM_OUTPUT" | grep -q "data:"; then
    info "Streaming OK: received SSE data events"
else
    warn "Stream output: $STREAM_OUTPUT"
    fail "Streaming did not return SSE data events."
fi

# --- Test: Auth rejection ---
info "Testing auth rejection (bad key)..."
AUTH_RESPONSE=$(curl -s -o /dev/null -w "%{http_code}" http://localhost:8080/api/openai/v1/chat/completions \
    -H "Authorization: Bearer rtr_INVALID_KEY_HERE" \
    -H "Content-Type: application/json" \
    -d '{"model":"x","messages":[{"role":"user","content":"hi"}]}')

if [ "$AUTH_RESPONSE" = "401" ]; then
    info "Auth rejection OK: got 401"
else
    warn "Expected 401, got $AUTH_RESPONSE"
fi

# --- Done ---
echo ""
info "============================================"
info "  All smoke tests passed!"
info "============================================"
echo ""
