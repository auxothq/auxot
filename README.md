<p align="center">
  <h1 align="center">Auxot</h1>
  <p align="center">
    Open-source GPU inference router for local LLMs
    <br />
    <a href="https://auxot.com">auxot.com</a> · <a href="#quickstart">Quickstart</a> · <a href="#deploy-to-flyio">Deploy</a> · <a href="#api-reference">API</a>
  </p>
</p>

---

Run any open-source LLM on your own GPU and serve it as an API — OpenAI-compatible and Anthropic-compatible out of the box. Point Claude Code, Cursor, Open WebUI, or any tool at it and start using your own hardware for inference. No vendor lock-in. No tokens burned. Your GPU, your data, your rules. Runs air-gapped if needed.

- **One command to deploy, one command to connect your GPU** — no cluster, no orchestrator, no YAML
- **Works with every tool that speaks OpenAI or Anthropic** — Claude Code, Cursor, Open WebUI, LangChain, anything
- **700+ models** — automatic quantization selection, just pick a name
- **Streaming tool calls** — full agentic workflow support for coding assistants
- **Dead simple** — single binary, no dependencies, embedded Redis, `FROM scratch` Docker image (~10MB)

## Quickstart

### Deploy to Fly.io (no clone required)

```bash
npx @auxot/cli setup --fly
```

Follow the printed steps — create app, set secrets, deploy. Then connect your GPU:

```bash
# Run this on your GPU machine. Keep it running.
npx @auxot/worker-cli --gpu-key adm_xxx --router-url wss://your-app.fly.dev/ws
```

### Configure Your Tools

Point any OpenAI or Anthropic-compatible tool at your router:

| Protocol | Base URL |
|---|---|
| OpenAI-compatible | `https://your-app.fly.dev/api/openai` |
| Anthropic-compatible | `https://your-app.fly.dev/api/anthropic` |

**API Key:** the `rtr_...` key from setup.

Works with Claude Code, Cursor, Open WebUI, LangChain, and anything that speaks these protocols.

### Run Locally

```bash
# Generate keys
npx @auxot/cli setup

# Set environment variables (copy from setup output)
export AUXOT_ADMIN_KEY_HASH='$argon2id$...'
export AUXOT_API_KEY_HASH='$argon2id$...'

# Start the router
go install github.com/auxothq/auxot/cmd/auxot-router@latest
auxot-router
```

Or download a binary from [Releases](https://github.com/auxothq/auxot/releases), or run the Docker image:

```bash
docker run -p 8080:8080 \
  -e AUXOT_ADMIN_KEY_HASH='$argon2id$...' \
  -e AUXOT_API_KEY_HASH='$argon2id$...' \
  ghcr.io/auxothq/auxot-router:latest
```

Connect a GPU worker (on any machine with a GPU):

```bash
npx @auxot/worker-cli --gpu-key adm_xxx --router-url ws://localhost:8080/ws
```

Then configure your tools with `http://localhost:8080/api/openai` or `http://localhost:8080/api/anthropic` as the base URL.

### Send Requests

```bash
curl http://localhost:8080/api/openai/chat/completions \
  -H "Authorization: Bearer rtr_xxxxxxxxx" \
  -H "Content-Type: application/json" \
  -d '{
    "model": "any - required but ignored",
    "messages": [{"role": "user", "content": "Hello!"}],
    "stream": true
  }'
```

## Defaults

Out of the box, Auxot is configured to be useful for chat and agentic tools (Claude Code, OpenClaw, Cursor, etc.):

| Setting | Default | Why |
|---|---|---|
| Model | `Qwen3-Coder-30B-A3B` | Fast MoE coding model, fits in ~16GB VRAM |
| Quantization | `Q4_K_S` | Best speed/quality tradeoff |
| Context | 128K | Large enough for agentic tool use |
| Parallel | 2 | Handle concurrent requests without queueing |

The worker automatically dials down parallelism if your GPU doesn't have enough memory.

### Raising the Defaults

This is configured on the server side, the worker honors the server side model policy.

If you have more GPU headroom:

```bash
# Bigger model (needs ~74GB VRAM)
AUXOT_MODEL=Qwen3-235B-A22B-128K

# Higher quality quantization (needs more VRAM)
AUXOT_QUANTIZATION=Q6_K

# Larger context window (needs more VRAM)
AUXOT_CTX_SIZE=262144    # 256K

# More concurrent requests (needs more VRAM per slot)
AUXOT_MAX_PARALLEL=4
```

### Choosing a Different Model

```bash
auxot-router models
```

Lists all 700+ available models with context sizes and VRAM requirements.

## How It Works

```
┌──────────────┐      ┌──────────────┐      ┌──────────────┐
│   Your App   │─────▶│ auxot-router │◀────▶│ auxot-worker │
│  (any client)│ API  │   (server)   │  WS  │  (your GPU)  │
└──────────────┘      └──────────────┘      └──────────────┘
                             │
                       ┌─────┴─────┐
                       │   Redis   │
                       │(embedded) │
                       └───────────┘
```

Auxot routes OpenAI-compatible and Anthropic-compatible API requests to GPU workers running [llama.cpp](https://github.com/ggerganov/llama.cpp). The router accepts API requests, queues them via Redis Streams, and distributes them to connected GPU workers. Workers run llama.cpp and stream tokens back through the router to your application.

Redis is **embedded by default** (in-memory, ephemeral). No external services needed. Set `AUXOT_REDIS_URL` if you want persistence or multi-instance routing.

The worker automatically:
1. Connects to the router and receives the model policy
2. Downloads the model from HuggingFace (cached in `~/.auxot/models/`)
3. Downloads llama.cpp (cached in `~/.auxot/llama-server/`)
4. Detects GPU hardware (Metal, CUDA, Vulkan, or CPU fallback)
5. Launches llama.cpp and starts serving requests

## Deploy to Fly.io

The router deploys to [Fly.io](https://fly.io) as a single `FROM scratch` container (~10MB) with no external dependencies.

### Quick deploy

```bash
# Install Fly CLI (if you haven't)
curl -L https://fly.io/install.sh | sh
fly auth login

# Generate keys + get step-by-step commands
npx @auxot/cli setup --fly
```

### Interactive deploy

```bash
npx @auxot/cli deploy
```

The `deploy` command is interactive — it creates the app, generates keys, sets secrets, and deploys the pre-built Docker image.

### Scaling

For single-GPU hobby use, the free tier (256MB, shared CPU) is enough. The embedded Redis handles everything in-process.

For multi-GPU production deployments, add an external Redis and scale horizontally:

```bash
fly secrets set AUXOT_REDIS_URL=redis://your-redis-url
fly scale count 2
```

## Docker

Pre-built multi-arch images (linux/amd64 + linux/arm64) are published on every release:

```bash
docker pull ghcr.io/auxothq/auxot-router:latest
docker run -p 8080:8080 \
  -e AUXOT_ADMIN_KEY_HASH='...' \
  -e AUXOT_API_KEY_HASH='...' \
  ghcr.io/auxothq/auxot-router:latest
```

## CLI Commands

### Router

```bash
auxot-router              # Start the router
auxot-router setup        # Generate keys and print configuration
auxot-router setup --fly  # Generate keys with Fly.io secrets format
auxot-router models       # List all available models with context sizes
auxot-router help         # Print help
```

### Worker

```bash
auxot-worker                          # Connect to router and start processing
auxot-worker --gpu-key adm_xxx        # Authenticate with GPU key
auxot-worker --router-url wss://...   # Connect to remote router
auxot-worker --debug                  # Debug logging (level 1)
auxot-worker --debug 2                # Verbose logging (llama.cpp output)
auxot-worker help                     # Print help
```

## API Reference

### OpenAI-Compatible

| Endpoint | Description |
|---|---|
| `POST /api/openai/chat/completions` | Chat completions (streaming + blocking) |
| `GET /api/openai/models` | List available models |

### Anthropic-Compatible

| Endpoint | Description |
|---|---|
| `POST /api/anthropic/v1/messages` | Messages API (streaming + blocking) |
| `POST /api/anthropic/v1/messages/count_tokens` | Token counting |
| `GET /api/anthropic/v1/models` | List available models |

### Infrastructure

| Endpoint | Description |
|---|---|
| `GET /health` | Health check (no auth) |
| `WS /ws` | GPU worker WebSocket connection |

## Configuration Reference

All configuration is via environment variables (or `.env` file).

| Variable | Required | Default | Description |
|---|---|---|---|
| `AUXOT_MODEL` | No | `Qwen3-Coder-30B-A3B` | Model to serve (validated against registry) |
| `AUXOT_ADMIN_KEY_HASH` | Yes | — | Argon2id hash of GPU key |
| `AUXOT_API_KEY_HASH` | Yes | — | Argon2id hash of API key |
| `AUXOT_QUANTIZATION` | No | `Q4_K_S` | Quantization (auto-selects if omitted) |
| `AUXOT_CTX_SIZE` | No | 131072 (128K) | Context window size |
| `AUXOT_MAX_PARALLEL` | No | 2 | Concurrent jobs per GPU |
| `AUXOT_PORT` | No | 8080 | HTTP listen port |
| `AUXOT_HOST` | No | 0.0.0.0 | Bind address |
| `AUXOT_LOG_LEVEL` | No | info | debug, info, warn, error |
| `AUXOT_JOB_TIMEOUT` | No | 5m | Max job duration |
| `AUXOT_REDIS_URL` | No | embedded | Redis URL (embedded in-memory if not set) |

## Worker Configuration

| Variable | Default | Description |
|---|---|---|
| `AUXOT_GPU_KEY` | — | GPU key from setup (required) |
| `AUXOT_ROUTER_URL` | ws://localhost:8080/ws | Router WebSocket URL |
| `AUXOT_GPU_LAYERS` | 9999 | GPU layers to offload (9999 = all) |
| `AUXOT_THREADS` | auto | CPU threads for llama.cpp |
| `AUXOT_MODEL_FILE` | — | Path to local GGUF file (skip download) |
| `AUXOT_LLAMA_BINARY` | — | Path to local llama-server binary (skip download) |

### Air-Gapped Deployment

For environments without internet access:

```bash
auxot-worker \
  --gpu-key adm_xxx \
  --router-url ws://router.internal:8080/ws \
  --model-path /opt/models/qwen3-8b-Q4_K_M.gguf \
  --llama-server-path /opt/bin/llama-server
```

## Architecture

```
auxot/
├── cmd/
│   ├── auxot-router/     # Router binary entry point
│   └── auxot-worker/     # Worker binary entry point
├── internal/
│   ├── router/           # HTTP server, WebSocket handler, API handlers
│   └── worker/           # llama.cpp process manager, job executor
├── pkg/
│   ├── anthropic/        # Anthropic API types
│   ├── auth/             # Key generation and verification (Argon2id)
│   ├── gpu/              # GPU worker pool tracking
│   ├── gpudetect/        # GPU hardware detection (Metal/CUDA/Vulkan)
│   ├── llamabin/         # llama.cpp binary downloader
│   ├── llamacpp/         # llama.cpp HTTP client
│   ├── modeldown/        # Model file downloader (HuggingFace)
│   ├── openai/           # OpenAI API types
│   ├── protocol/         # WebSocket message protocol
│   ├── queue/            # Redis Streams job queue, sweeper, heartbeat
│   └── registry/         # Embedded model registry (700+ models)
├── tests/integration/    # Integration tests
├── Dockerfile            # FROM scratch production image
├── fly.toml              # Fly.io deployment config
└── Makefile
```

### Key Design Decisions

- **No database** — the router is stateless. All state lives in Redis (or embedded miniredis).
- **Redis Streams** — job queue with consumer groups for fair distribution across GPU workers.
- **Embedded Redis** — `AUXOT_REDIS_URL` not set? The router starts miniredis in-process. Zero config.
- **FROM scratch** — the Docker image is a single Go binary. Nothing else.
- **Embedded model registry** — compiled into the binary at build time. No API calls to discover models.
- **Argon2id authentication** — only key hashes are stored. Plaintext keys are shown once during setup.

## Development

```bash
# Build both binaries
make build

# Run tests
make test

# Run integration tests (uses embedded Redis — no external deps)
make test-integration

# Run all checks (vet, lint, race, build)
make check

# Cross-compile for all platforms
make release
```

## Auxot Cloud

**Need more than the open-source router?**

[Auxot Cloud](https://auxot.com) provides a managed platform with:

- **Multi-tenant GPU sharing** — token-bucket rate limiting, org/team management
- **Encryption at rest** — envelope encryption with AWS KMS for all inference data
- **Web dashboard** — model management, GPU monitoring, usage analytics
- **Billing integration** — Stripe-powered usage-based billing
- **SLA & support** — guaranteed uptime and priority support

The open-source router is perfect for personal use, small teams, and self-hosted deployments. [Auxot Cloud](https://auxot.com) is for organizations that need managed infrastructure with enterprise features.

## Contributing

Contributions are welcome. Please open an issue to discuss significant changes before submitting a PR.

```bash
# Before submitting:
make check
```

## License

Apache License 2.0 — see [LICENSE](LICENSE).

Copyright 2024 Auxot.
