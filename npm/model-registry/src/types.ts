/**
 * Model capabilities supported by workers
 */
export type ModelCapability =
  | "chat"
  | "vision"
  | "embedding"
  | "code"
  | "reasoning"
  | "image_generation";

/**
 * Model family type (architecture)
 */
export type ModelFamily = "MoE" | "Dense";

/**
 * Individual model entry in the registry
 */
export interface ModelRegistryEntry {
  /** Unique ID: "model-name-quantization" (e.g. "qwen2.5-coder-30b-q4_k_m") */
  id: string;
  /** Normalized model name (e.g. "Qwen2.5-Coder-30B") */
  model_name: string;
  /** Full Hugging Face repo ID (e.g. "Qwen/Qwen2.5-Coder-30B-A3B-Instruct-GGUF") */
  huggingface_id: string;
  /** Quantization level (e.g. "Q4_K_M", "Q8_0") */
  quantization: string;
  /** Model family (architecture) */
  family: ModelFamily;
  /** Parameter count (e.g. "30B", "7B") */
  parameters: string;
  /** Default context window size */
  default_context_size: number;
  /** Maximum context window size supported by the model */
  max_context_size: number;
  /** Estimated VRAM required in GB */
  vram_requirements_gb: number;
  /** Model capabilities */
  capabilities: ModelCapability[];
  /** GGUF filename pattern or exact name */
  file_name: string;
  /** MMProj file for vision models (e.g. mmproj-F16.gguf). Prefer F16 → BF16 → F32. */
  mmproj_file_name?: string;
  /** File size in bytes (optional) */
  file_size_bytes?: number | null;
}

/**
 * Complete model registry structure
 */
export interface ModelRegistry {
  /** Registry version (semver) */
  version: string;
  /** ISO timestamp when registry was generated */
  generated_at: string;
  /** List of models */
  models: ModelRegistryEntry[];
}

/**
 * Filters for querying the registry
 */
export interface ModelRegistryFilters {
  /** Filter by family */
  family?: ModelFamily;
  /** Filter by capabilities (must have all specified) */
  capabilities?: ModelCapability[];
  /** Filter by parameter count (e.g. "30B") */
  parameters?: string;
  /** Minimum VRAM in GB */
  min_vram_gb?: number;
  /** Maximum VRAM in GB */
  max_vram_gb?: number;
  /** Filter by model name (partial match) */
  model_name?: string;
}
