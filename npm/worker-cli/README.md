# Auxot GPU Worker CLI

Connect your local GPU resources to the Auxot platform. The worker CLI automatically downloads and manages llama.cpp binaries and models based on your GPU key policy.

## Requirements

- Node.js 20+
- Valid Auxot GPU key with configured policy
- GPU hardware (NVIDIA CUDA, AMD Vulkan, or Apple Metal) - CPU fallback available with limitations

## Installation

```bash
# Run directly with npx (recommended)
npx @auxot/worker-cli --gpu-key YOUR_GPU_KEY

# Or install globally
npm install -g @auxot/worker-cli
worker-cli --gpu-key YOUR_GPU_KEY
```

## Quick Start

### 1. Get Your GPU Key

1. Log in to [Auxot](https://auxot.com)
2. Navigate to **Organization Settings → GPU Keys**
3. Create a new GPU key
4. **Configure the policy** (model, quantization, context size, capabilities)
5. Copy the key (format: `gpu.xxxxx.yyyyy`)

### 2. Run the Worker

```bash
npx @auxot/worker-cli --gpu-key gpu.xxxxx.yyyyy
```

The worker CLI will:
- ✅ Automatically download the llama.cpp binary (first run only)
- ✅ Automatically download the model specified in your GPU key policy
- ✅ Spawn and manage the llama.cpp process
- ✅ Connect to the Auxot platform
- ✅ Start processing jobs

## CLI Options

```
--gpu-key <key>        GPU authentication key (required)
--auxot-url <url>      Auxot platform URL (default: https://auxot.com)
--debug [level]        Enable debug logging (level 1 or 2, default: 1)
--help, -h             Show help message
```

**Note:** The `--llama-url` option is no longer needed. The worker CLI manages its own llama.cpp instance on `http://127.0.0.1:9002`.

## How It Works

1. **Policy Reception**: Worker CLI connects to Auxot and receives the GPU key policy (model, quantization, context size, etc.)
2. **Model Download**: Automatically downloads the required GGUF model from Hugging Face if not already cached
3. **Binary Download**: Downloads the appropriate llama.cpp binary for your platform (first run only)
4. **Process Management**: Spawns llama.cpp with policy parameters and manages its lifecycle
5. **Capability Discovery**: Queries llama.cpp to discover actual model capabilities
6. **Validation**: Validates discovered capabilities against the policy (both client and server-side)
7. **Job Processing**: Listens for agent execution jobs and forwards them to llama.cpp
8. **Streaming**: Streams response tokens back to the platform in real-time
9. **Crash Recovery**: Automatically restarts llama.cpp if it crashes

## GPU Key Policy

The GPU key policy defines what model and configuration your worker must use:

- **Model Name**: Which model to load (e.g., "Qwen3-VL-30B-A3B")
- **Quantization**: Model quantization level (e.g., "Q4_K_S", "F16")
- **Context Size**: Maximum context window (e.g., 128000)
- **Max Parallelism**: Maximum parallel jobs (e.g., 2)
- **Capabilities**: Required capabilities (e.g., ["chat", "vision"])

The worker CLI validates that its discovered capabilities match the policy before accepting jobs.

## Model Storage

Models are cached in `~/.auxot/models/` (platform-specific). You can override this with the `AUXOT_MODELS_DIR` environment variable.

## Binary Storage

The llama.cpp binary is cached in `~/.auxot/llama-server/{platform}-{arch}/`. The binary is downloaded once and reused on subsequent runs.

## GPU Detection

The worker CLI automatically detects your GPU hardware:

- **macOS**: Metal GPU acceleration (built into binaries)
- **Linux**: Vulkan GPU acceleration (AMD/NVIDIA) or CPU fallback
- **Windows**: CUDA 12.4 GPU acceleration (NVIDIA) or CPU fallback

If no GPU is detected, the worker will:
- Download a CPU-only binary with a warning
- Limit model size to 7B or less (if policy specifies larger model, validation will fail)

## GPU ID

The worker CLI generates a stable UUID on first run and stores it in `~/.auxot/gpu-id`. This allows Auxot to track individual GPUs across restarts.

## Troubleshooting

### Connection Failed

- Verify `--auxot-url` is correct
- Check network connectivity
- Ensure GPU key is valid and has a configured policy

### Policy Validation Failed

- Ensure your GPU key policy is configured in the web UI
- Check that the model specified in the policy exists in the model registry
- Verify your GPU hardware meets the policy requirements (context size, capabilities)

### Model Download Failed

- Check internet connectivity (models are downloaded from Hugging Face)
- Verify sufficient disk space (models can be 10GB+)
- Check Hugging Face API rate limits (downloads are resumable)

### llama.cpp Crashes

- Check GPU memory (VRAM) - models may not fit in available memory
- Review llama.cpp logs in worker CLI output
- Worker CLI will attempt to auto-restart crashed processes

### No Jobs Received

- Verify GPU key belongs to the correct organization
- Check that agents exist in your organization
- Ensure GPU meets minimum context size requirements
- Verify worker is online (check dashboard in web UI)

## Support

For issues, questions, or feature requests, please visit:
- [Auxot Documentation](https://auxot.com/docs)
- [GitHub Issues](https://github.com/keith301/auxot/issues)
- Email: support@auxot.com

## License

Copyright © 2026 Auxot. All rights reserved.
