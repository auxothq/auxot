<p align="center">
  <h1 align="center">Auxot</h1>
  <p align="center">
    Open-source GPU inference router for local LLMs
    <br />
    <a href="https://auxot.com">auxot.com</a> · <a href="#quickstart">Quickstart</a> · <a href="#deploy-to-flyio">Deploy</a> · <a href="#api-reference">API</a>
  </p>
</p>

---

Auxot routes OpenAI-compatible and Anthropic-compatible API requests to GPU workers running [llama.cpp](https://github.com/ggerganov/llama.cpp). You run the router on a server, connect one or more GPUs, and point your tools at it.

- **Single binary** — no runtime dependencies, no Redis to manage
- **FROM scratch** Docker image — ~10MB, starts in milliseconds
- **OpenAI + Anthropic API compatible** — works with any client that speaks those protocols
- **700+ models** — embedded model registry with automatic quantization selection
- **Resumable downloads** — workers auto-download models from HuggingFace with resume support
- **Tool calling** — full streaming tool call support for agentic workflows
- **Dead worker recovery** — automatic job reclamation when GPU workers disconnect

## How It Works

```
┌──────────────┐       ┌──────────────┐       ┌──────────────┐
│   Your App   │──────▶│  auxot-router │◀─────▶│  auxot-worker │
│  (any client)│  API  │  (server)    │  WS   │  (your GPU)  │
└──────────────┘       └──────────────┘       └──────────────┘
                              │
                        ┌─────┴─────┐
                        │   Redis   │
                        │(embedded) │
                        └───────────┘
```

The router accepts API requests, queues them via Redis Streams, and distributes them to connected GPU workers. Workers run llama.cpp and stream tokens back through the router to your application.

Redis is **embedded by default** (in-memory, ephemeral). No external services needed. Set `AUXOT_REDIS_URL` if you want persistence or multi-instance routing.

## Quickstart

### Deploy to Fly.io (no clone required)

The fastest way to get a production router running:

```bash
# Generate keys and get your fly secrets command
npx @auxot/cli setup --fly

# Create the app and deploy the pre-built image
fly apps create my-router
fly secrets set ...   # paste the output from setup
fly deploy --image ghcr.io/auxothq/auxot-router:latest
```

Then connect a GPU worker from any machine:

```bash
npx @auxot/worker-cli --gpu-key adm_xxx --router wss://my-router.fly.dev/ws
```

That's it. Your router is live and your GPU is serving requests.

### Run Locally

```bash
# Generate keys
npx @auxot/cli setup

# Set environment variables (copy from setup output)
export AUXOT_ADMIN_KEY_HASH='$argon2id$...'
export AUXOT_API_KEY_HASH='$argon2id$...'
export AUXOT_MODEL=Qwen3-Coder-30B-A3B

# Start the router
go install github.com/auxothq/auxot/cmd/auxot-router@latest
auxot-router
```

Or download a binary from [Releases](https://github.com/auxothq/auxot/releases).

### Connect a GPU Worker

On a machine with a GPU:

```bash
npx @auxot/worker-cli --gpu-key adm_xxx --router ws://localhost:8080/ws
```

The worker will:
1. Connect to the router and receive the model policy
2. Download the model from HuggingFace (cached in `~/.auxot/models/`)
3. Download llama.cpp (cached in `~/.auxot/llama-server/`)
4. Detect GPU hardware (Metal, CUDA, Vulkan, or CPU fallback)
5. Launch llama.cpp and start serving requests

### Send Requests

Use any OpenAI-compatible client:

```bash
curl http://localhost:8080/api/openai/v1/chat/completions \
  -H "Authorization: Bearer rtr_xxxxxxxxx" \
  -H "Content-Type: application/json" \
  -d '{
    "model": "any",
    "messages": [{"role": "user", "content": "Hello!"}],
    "stream": true
  }'
```

Or any Anthropic-compatible client:

```bash
curl http://localhost:8080/api/anthropic/v1/messages \
  -H "x-api-key: rtr_xxxxxxxxx" \
  -H "anthropic-version: 2023-06-01" \
  -H "Content-Type: application/json" \
  -d '{
    "model": "any",
    "max_tokens": 1024,
    "messages": [{"role": "user", "content": "Hello!"}]
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

### Raising the Defaults

If you have more GPU headroom, you can increase quality and throughput:

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
# List all available models with context sizes and VRAM requirements
auxot-router models
```

Or browse models with `go run github.com/auxothq/auxot/cmd/auxot-router@latest models`.

## Deploy to Fly.io

The router deploys to [Fly.io](https://fly.io) as a single `FROM scratch` container (~10MB) with no external dependencies.

### Quick deploy

```bash
# Install Fly CLI (if you haven't)
curl -L https://fly.io/install.sh | sh
fly auth login

# Generate keys + deploy
npx @auxot/cli deploy
```

The `deploy` command is interactive — it creates the app, generates keys, sets secrets, and deploys the pre-built Docker image from GHCR.

### Manual deploy

```bash
# Generate keys
npx @auxot/cli setup --fly --model Qwen3-235B-A22B-128K

# Create app and set secrets
fly apps create auxot-router
fly secrets set \
  AUXOT_ADMIN_KEY_HASH='...' \
  AUXOT_API_KEY_HASH='...' \
  AUXOT_MODEL='Qwen3-235B-A22B-128K'

# Deploy the pre-built image
fly deploy --image ghcr.io/auxothq/auxot-router:latest
```

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
  -e AUXOT_MODEL=Qwen3-Coder-30B-A3B \
  ghcr.io/auxothq/auxot-router:latest
```

## API Reference

### OpenAI-Compatible

| Endpoint | Description |
|---|---|
| `POST /api/openai/v1/chat/completions` | Chat completions (streaming + blocking) |
| `GET /api/openai/v1/models` | List available models |

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
