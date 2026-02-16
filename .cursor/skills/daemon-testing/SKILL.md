---
name: daemon-testing
description: End-to-end testing of auxot-router and auxot-worker daemons with real llama.cpp inference
---

# Daemon Testing — auxot-router + auxot-worker

Test the full auxot inference pipeline without human involvement: router, worker, llama.cpp, and embedded Redis.

## When to Use

- After making changes to `cmd/auxot-router`, `cmd/auxot-worker`, or `internal/` packages
- User asks to "test the router", "test the worker", "test end-to-end"
- After changing the WebSocket protocol, job dispatch, or API handlers
- After changing model registry, config loading, or authentication

## Prerequisites

| Dependency | How to check | Notes |
|------------|-------------|-------|
| Go toolchain | `go version` | Required to build binaries |
| .env file | `bin/auxot-router setup --write-env` | Generates keys and writes `.env` file |

No external Redis needed — the router uses embedded miniredis by default.

### First-Time Setup

```bash
# 1. Build binaries
make build

# 2. Generate keys and write .env (only needed once)
bin/auxot-router setup --write-env
# Prints plaintext keys to stdout (save them!) and writes hashes to .env

# 3. Add a model to .env — pick one from:
bin/auxot-router models
# Then add to .env:  AUXOT_MODEL=<model-name>
```

### Verify Prerequisites

```bash
# Binaries built?
ls bin/auxot-router bin/auxot-worker

# .env has keys?
grep AUXOT_API_KEY_HASH .env
```

## Architecture

```
┌──────────┐    HTTP/WS     ┌──────────────┐    WS      ┌─────────────┐    HTTP    ┌─────────────┐
│ curl/API │ ──────────────→│ auxot-router │←──────────→│ auxot-worker│──────────→│ llama-server│
│  caller  │    :8080       │  (Go binary) │   :8080/ws │  (Go binary)│  :random  │ (subprocess)│
└──────────┘                └──────┬───────┘            └─────────────┘           └─────────────┘
                                   │
                            miniredis (embedded)
                          (job queue + tokens)
```

**Worker lifecycle:**
1. Worker connects to router → receives Policy (model name, quant, ctx, parallelism)
2. Worker disconnects, downloads model + llama.cpp binary if not cached
3. Worker spawns `llama-server` as subprocess on a random port
4. Worker waits for `llama-server` to be ready, discovers capabilities
5. Worker reconnects to router permanently, sends capabilities
6. Worker processes jobs: receives from router → forwards to llama.cpp → streams tokens back

**Key detail:** The worker downloads and manages `llama-server` itself (from GitHub releases into `~/.auxot/llama-server/`). It is NOT installed via Homebrew or any system package manager.

## Testing Workflow

### Step 0: Build Both Binaries

```bash
make build
```

This produces `bin/auxot-router` and `bin/auxot-worker`.

### Step 1: Start the Router (Background)

The router uses embedded miniredis — no external Redis needed.

```bash
bin/auxot-router &
ROUTER_PID=$!
echo "Router PID: $ROUTER_PID"
```

Wait ~1 second, then verify it's listening:

```bash
curl -s http://localhost:8080/health || echo "Router not ready"
```

**Expected:** Router logs show model validated against registry, embedded redis started, listening on :8080.

### Step 2: Start the Worker (Background)

The worker reads `.env` for `AUXOT_GPU_KEY` and connects to the router.

```bash
bin/auxot-worker &
WORKER_PID=$!
echo "Worker PID: $WORKER_PID"
```

**Expected output (JSON log lines, pretty-printed in TTY):**

All output is structured JSON via `slog`. When running in a terminal, lines are pretty-printed (indented). The key messages to watch for, in order:

```json
{"level":"INFO","msg":"auxot-worker starting","version":"0.1.0"}
{"level":"INFO","msg":"authenticated","gpu_id":"..."}
{"level":"INFO","msg":"policy","model":"...","quantization":"...","context_size":...,"max_parallelism":...}
{"level":"INFO","msg":"model ready","path":"~/.auxot/models/.../model.gguf","shards":1}
{"level":"INFO","msg":"llama.cpp binary","path":"~/.auxot/llama-server/..."}
{"level":"INFO","msg":"gpu detected","backend":"metal|cuda|vulkan|cpu","detected":true}
{"level":"INFO","msg":"llama.cpp started","port":...}
{"level":"INFO","msg":"waiting for llama.cpp"}
{"level":"INFO","msg":"llama.cpp ready"}
{"level":"INFO","msg":"model warmed up"}
{"level":"INFO","msg":"capabilities","model":"...","backend":"llama.cpp","ctx_size":...,"total_slots":...,"vram_gb":...,"parameters":"..."}
{"level":"INFO","msg":"worker ready","gpu_id":"..."}
```

**Wait for `"msg":"worker ready"` before proceeding.** The llama.cpp startup can take 10-60+ seconds depending on model size and hardware.

### Step 3: Send a Test Request

```bash
# Source .env to get API key
set -a && source .env && set +a

# Non-streaming request
curl -s http://localhost:8080/api/openai/v1/chat/completions \
  -H "Authorization: Bearer $AUXOT_API_KEY" \
  -H "Content-Type: application/json" \
  -d '{
    "model": "anything",
    "messages": [{"role": "user", "content": "Say hello in exactly 5 words."}],
    "stream": false,
    "max_tokens": 50
  }' | jq .
```

**Expected:** JSON response with `choices[0].message.content`.

### Step 4: Test Streaming

```bash
set -a && source .env && set +a

curl -N http://localhost:8080/api/openai/v1/chat/completions \
  -H "Authorization: Bearer $AUXOT_API_KEY" \
  -H "Content-Type: application/json" \
  -d '{
    "model": "anything",
    "messages": [{"role": "user", "content": "Count from 1 to 5."}],
    "stream": true,
    "max_tokens": 100
  }'
```

**Expected:** SSE stream of `data: {...}` lines, ending with `data: [DONE]`.

### Step 5: Cleanup

```bash
# Kill worker first (stops its llama.cpp child process)
kill $WORKER_PID 2>/dev/null

# Then kill router
kill $ROUTER_PID 2>/dev/null

# Verify nothing is left
sleep 2
pgrep -f "auxot-router|auxot-worker|llama-server" && echo "STALE PROCESSES — run: pkill -f 'auxot-router|auxot-worker|llama-server'" || echo "Clean"
```

## Quick Iteration Tips

### Use a Small Model

Large models (30B+) take a long time to load. For quick iteration, set a smaller model in `.env`:

```
AUXOT_MODEL=Qwen2.5-0.5B-Instruct
```

Or check what's available in the registry:

```bash
bin/auxot-router models
```

### Air-Gapped Deployment (Skip Downloads)

If you already have the GGUF model file and/or the llama-server binary:

```bash
# Skip model download
bin/auxot-worker --model-path /path/to/model.gguf

# Skip llama-server binary download
bin/auxot-worker --llama-server-path /path/to/llama-server

# Both (fully air-gapped)
bin/auxot-worker \
  --model-path /path/to/model.gguf \
  --llama-server-path /path/to/llama-server
```

For split/sharded models (e.g., 480B), point to the first shard — all sibling shards must be in the same directory:

```bash
bin/auxot-worker --model-path /opt/models/Model-Q4_K_S-00001-of-00006.gguf
```

These flags can also be set via environment variables: `AUXOT_MODEL_FILE` and `AUXOT_LLAMA_BINARY`.

### Debug Logging

```bash
# Verbose log level (more slog output)
AUXOT_LOG_LEVEL=debug bin/auxot-router
AUXOT_LOG_LEVEL=debug bin/auxot-worker

# Worker debug flags (WebSocket messages, llama.cpp I/O)
bin/auxot-worker --debug       # Level 1: WebSocket messages
bin/auxot-worker --debug 2     # Level 2: + llama.cpp requests/responses + process output
```

## Debugging

### Router won't start

```bash
# Check config validation
bin/auxot-router 2>&1 | head -20

# List available models
bin/auxot-router models

# Common: AUXOT_MODEL not found in registry — check spelling
```

### Worker won't connect

```bash
# Check router is actually listening
curl -s http://localhost:8080/health

# Check auth — wrong GPU key?
AUXOT_LOG_LEVEL=debug bin/auxot-worker 2>&1 | head -30
```

### Worker connects but no jobs arrive

```bash
# Run router with debug logging to see dispatch decisions
AUXOT_LOG_LEVEL=debug bin/auxot-router
```

### llama.cpp crashes

```bash
# Worker logs llama.cpp stderr lines containing "error", "fatal", "crash"
# Test llama.cpp standalone:
~/.auxot/llama-server/*/llama-*/llama-server \
  --model /path/to/model.gguf \
  --ctx-size 4096 \
  --port 8081
```

### Stale processes after ^C

```bash
pkill -f "auxot-router|auxot-worker|llama-server"
pgrep -f "auxot-router|auxot-worker|llama-server" || echo "All clean"
```

## Key Environment Variables

| Variable | Purpose | Default |
|----------|---------|---------|
| `AUXOT_MODEL` | Model name for policy | **Required** |
| `AUXOT_QUANTIZATION` | Quantization (auto-selected if omitted) | Auto |
| `AUXOT_REDIS_URL` | External Redis (optional) | Embedded miniredis |
| `AUXOT_GPU_KEY` | Worker auth key (`adm_...`) | **Required** for worker |
| `AUXOT_API_KEY` | API caller key (`rtr_...`) | **Required** for API calls |
| `AUXOT_ADMIN_KEY_HASH` | Argon2 hash of admin key | **Required** for router |
| `AUXOT_API_KEY_HASH` | Argon2 hash of API key | **Required** for router |
| `AUXOT_LOG_LEVEL` | `debug`, `info`, `warn`, `error` | `info` |
| `AUXOT_CTX_SIZE` | Context window override | Model default |
| `AUXOT_PORT` | Router listen port | `8080` |
| `AUXOT_LLAMA_BINARY` | Override llama.cpp binary path (or `--llama-server-path`) | Auto-download |
| `AUXOT_MODEL_FILE` | Override model file path (or `--model-path`) | Auto-download |

## Lessons Learned

- **No external Redis required.** The router embeds miniredis in-process. Set `AUXOT_REDIS_URL` only if you need external Redis for persistence or multi-instance.
- **llama.cpp binary is downloaded into `~/.auxot/llama-server/`, NOT installed via Homebrew.** The worker handles this via `pkg/llamabin`.
- **Models are downloaded into `~/.auxot/models/`, NOT manually.** The worker handles this via `pkg/modeldown`.
- **Argon2 hashes in `.env` must be single-quoted** because they contain `$` characters that shell/godotenv would otherwise interpolate.
- **The worker spawns llama.cpp as a child process.** The worker is NOT a "dumb pipe" to an external llama.cpp — it owns the entire GPU stack lifecycle.
- **Kill worker first, then router.** The worker needs to cleanly stop its llama.cpp child process.
- **Use `AUXOT_LOG_LEVEL=debug`** on either binary to see detailed message-level logging.
- **`bin/auxot-router setup --write-env`** generates fresh keys and writes `.env` directly (refuses if it already exists). Without `--write-env`, prints to stdout instead.
- **All output is structured JSON** via `slog`. Pretty-printed when stderr is a TTY, compact otherwise.
- **Worker debug flags** (`--debug`, `--debug 2`) control WebSocket and llama.cpp I/O visibility independent of `AUXOT_LOG_LEVEL`.
