# Local Setup — auxot-router, auxot-worker, auxot-tools

Run the full auxot stack locally: router, GPU worker, and tools worker connected together.

## Architecture

```
┌──────────────┐    HTTP/WS     ┌──────────────┐    WS      ┌─────────────┐    HTTP   ┌─────────────┐
│ curl/API     │ ──────────────→│ auxot-router │←──────────→│ auxot-worker│──────────→│ llama-server│
│  caller      │    :8080       │  (Go binary) │   :8080/ws │  (Go binary)│  :random  │ (subprocess)│
└──────────────┘                └──────┬───────┘            └─────────────┘           └─────────────┘
                                       │
                                ┌──────┴───────┐
                                │ auxot-tools  │  WS (tools key)
                                │ (Go binary)  │  Executes tool calls (web_fetch, code_executor, etc.)
                                └──────────────┘
```

## Prerequisites

- **Go 1.23+** — `go version`
- **GPU** (optional for tools-only) — auxot-worker needs a GPU for inference; auxot-tools does not

## Step 1: Build All Binaries

```bash
cd /Users/kminkler/src/auxothq/auxot
make build
```

This produces `bin/auxot-router`, `bin/auxot-worker`, and `bin/auxot-tools`.

## Step 2: Generate Keys and Configure .env

### Option A: Fresh Setup (recommended)

```bash
# Backup existing .env if you have one
mv .env .env.bak   # or remove it

# Generate keys and write .env
bin/auxot-router setup --write-env
```

The setup prints **plaintext keys** to stdout. **Save them.** Add these to `.env` for local use:

```bash
# Append to .env (copy the values from setup output):
AUXOT_API_KEY=rtr_xxxxx      # For API calls (curl, etc.)
AUXOT_GPU_KEY=adm_xxxxx      # For auxot-worker
AUXOT_TOOLS_KEY=tls_xxxxx    # For auxot-tools
```

The setup already wrote the **hashes** (`AUXOT_ADMIN_KEY_HASH`, `AUXOT_API_KEY_HASH`, `AUXOT_TOOLS_KEY_HASH`) to `.env`.

### Option B: Existing .env — Add Tools Support

If your `.env` has `AUXOT_GPU_KEY` and `AUXOT_API_KEY` but **no** `AUXOT_TOOLS_KEY_HASH`, tools workers cannot connect. Add a tools key:

```bash
bin/auxot-router setup --new-tools-key
```

Copy the output:
1. Add `AUXOT_TOOLS_KEY_HASH='...'` to `.env` (router needs this)
2. Add `AUXOT_TOOLS_KEY=tls_xxxxx` to `.env` (auxot-tools needs the plaintext)

### Use Embedded Redis (simplest)

For local testing, the router can use **embedded miniredis** — no external Redis needed. Comment out or remove `AUXOT_REDIS_URL` in `.env`:

```bash
# AUXOT_REDIS_URL=redis://localhost:6381   # Comment out for embedded Redis
```

### Use a Small Model (faster startup)

Large models take a long time to load. For quick iteration:

```bash
# Add to .env:
AUXOT_MODEL=Qwen2.5-0.5B-Instruct
```

Or list available models: `bin/auxot-router models`

## Step 3: Start the Router

```bash
cd /Users/kminkler/src/auxothq/auxot
source .env   # or: set -a && source .env && set +a

bin/auxot-router &
ROUTER_PID=$!
```

Wait ~2 seconds, then verify:

```bash
curl -s http://localhost:8080/health
```

Expected: `{"status":"ok"}` or similar.

## Step 4: Start the Worker (GPU)

The worker reads `AUXOT_GPU_KEY` and `AUXOT_ROUTER_URL` from the environment. For local router, set:

```bash
export AUXOT_ROUTER_URL=ws://localhost:8080/ws   # Default is wss://auxot.com
bin/auxot-worker &
WORKER_PID=$!
```

**Wait for `"msg":"worker ready"`** in the logs. Model download + llama.cpp startup can take 10–60+ seconds.

## Step 5: Start the Tools Worker

The tools worker reads `AUXOT_TOOLS_KEY` and `AUXOT_ROUTER_URL`:

```bash
export AUXOT_ROUTER_URL=ws://localhost:8080/ws   # Point to local router
bin/auxot-tools &
TOOLS_PID=$!
```

Expected log: `"msg":"auxot-tools starting"` then connection to router.

**Note:** The router must have `AUXOT_TOOLS_KEY_HASH` in its `.env` or tools connections will be refused.

## Step 6: Test the Stack

### Test inference (worker)

```bash
curl -s http://localhost:8080/api/openai/v1/chat/completions \
  -H "Authorization: Bearer $AUXOT_API_KEY" \
  -H "Content-Type: application/json" \
  -d '{
    "model": "anything",
    "messages": [{"role": "user", "content": "Say hello in 5 words."}],
    "stream": false,
    "max_tokens": 50
  }' | jq .
```

### Test tools (if AUXOT_ALLOWED_TOOLS is set)

If the router has `AUXOT_ALLOWED_TOOLS=code_executor,web_fetch` (or similar) in `.env`, tool calls will be injected into LLM jobs. A request that triggers a tool call will be routed to auxot-tools.

## Step 7: Cleanup

```bash
kill $TOOLS_PID 2>/dev/null
kill $WORKER_PID 2>/dev/null   # Stops llama.cpp child process
kill $ROUTER_PID 2>/dev/null
```

Or: `pkill -f "auxot-router|auxot-worker|auxot-tools|llama-server"`

## Quick Reference: Environment Variables

| Component    | Variable               | Purpose                                      |
|-------------|------------------------|----------------------------------------------|
| Router      | `AUXOT_ADMIN_KEY_HASH` | GPU worker auth (required)                   |
| Router      | `AUXOT_API_KEY_HASH`  | API caller auth (required)                   |
| Router      | `AUXOT_TOOLS_KEY_HASH`| Tools worker auth (optional, needed for tools)|
| Router      | `AUXOT_MODEL`         | Model name (required)                         |
| Router      | `AUXOT_REDIS_URL`     | Omit for embedded Redis                      |
| Router      | `AUXOT_ALLOWED_TOOLS` | e.g. `code_executor,web_fetch` to enable tools|
| Worker      | `AUXOT_GPU_KEY`       | Plaintext GPU key (required)                 |
| Worker      | `AUXOT_ROUTER_URL`    | `ws://localhost:8080/ws` for local           |
| Tools       | `AUXOT_TOOLS_KEY`     | Plaintext tools key (required)               |
| Tools       | `AUXOT_ROUTER_URL`    | `ws://localhost:8080/ws` for local           |
| API calls   | `AUXOT_API_KEY`       | Plaintext API key for Bearer token           |

## Troubleshooting

- **Router won't start:** Run `bin/auxot-router models` to verify `AUXOT_MODEL` exists.
- **Worker won't connect:** Ensure `AUXOT_ROUTER_URL=ws://localhost:8080/ws` and `AUXOT_GPU_KEY` is correct.
- **Tools refused:** Router needs `AUXOT_TOOLS_KEY_HASH` in `.env`. Run `auxot-router setup --new-tools-key` and add the hash.
- **Tools connects to auxot.com:** Set `AUXOT_ROUTER_URL=ws://localhost:8080/ws` — tools defaults to `wss://auxot.com`.
- **`--router-url` ignored / still see port 8080:** Keep the full command on ONE line. If you split across lines without a backslash (`\`), the shell runs only the first line (no `--router-url`) and the second line fails with "command not found". Use: `auxot-tools --tools-key KEY --router-url ws://localhost:9001/ws` (all on one line).
