# @auxot/model-registry

Shared model registry package for worker-cli and web applications.

This package contains curated information about LLM models available for use with worker-cli, including model metadata, Hugging Face IDs, VRAM requirements, and capabilities.

## Structure

- `registry.json` - Generated JSON file containing model definitions (committed to repo)
- `src/types.ts` - TypeScript type definitions
- `src/schemas.ts` - Zod validation schemas
- `src/loader.ts` - Runtime loader for registry.json
- `src/query.ts` - Query functions for filtering and searching models
- `scripts/build-registry.ts` - Build script to generate registry.json from Hugging Face

## Usage

```typescript
import { loadRegistry, getModels, getModelById, suggestModelsForVRAM } from '@auxot/model-registry';

// Load the registry
const registry = loadRegistry();

// Get all models
const allModels = getModels(registry);

// Filter models
const chatModels = getModels(registry, { capabilities: ['chat'] });
const largeModels = getModels(registry, { min_vram_gb: 20 });

// Get a specific model
const model = getModelById(registry, 'qwen2.5-coder-30b-q4_k_m');

// Suggest models for VRAM budget
const suggestions = suggestModelsForVRAM(registry, 24, 1); // 24GB VRAM, parallelism 1
```

## Building the Registry

To generate or update `registry.json`, run:

```bash
pnpm --filter @auxot/model-registry run build:registry
```

This will scan Hugging Face for compatible models and generate the registry JSON file.

The registry is committed to the repository, so it's available at runtime without needing to run the build script.

## Registry JSON Structure

See `src/types.ts` for the complete TypeScript definitions. The registry contains:

- `version` - Registry version (semver)
- `generated_at` - ISO timestamp when registry was generated
- `models` - Array of model entries with:
  - `id` - Unique identifier
  - `model_name` - Normalized model name
  - `huggingface_id` - Full Hugging Face repo ID
  - `quantization` - Quantization level (e.g. "Q4_K_M")
  - `family` - Model family (MoE or Dense)
  - `parameters` - Parameter count (e.g. "30B")
  - `default_context_size` - Default context window size
  - `vram_requirements_gb` - Estimated VRAM needed
  - `capabilities` - Model capabilities (chat, vision, embedding, code)
  - `file_name` - GGUF filename
  - `file_size_bytes` - File size (optional)

