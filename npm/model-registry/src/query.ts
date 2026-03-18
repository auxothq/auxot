import type {
  ModelRegistry,
  ModelRegistryEntry,
  ModelRegistryFilters,
} from "./types.js";

/**
 * Get all models from the registry (optionally filtered)
 */
export function getModels(
  registry: ModelRegistry,
  filters?: ModelRegistryFilters
): ModelRegistryEntry[] {
  let models = registry.models;

  if (filters) {
    if (filters.family) {
      models = models.filter((m) => m.family === filters.family);
    }
    if (filters.capabilities && filters.capabilities.length > 0) {
      models = models.filter((m) =>
        filters.capabilities!.every((cap) => m.capabilities.includes(cap))
      );
    }
    if (filters.parameters) {
      models = models.filter((m) => m.parameters === filters.parameters);
    }
    if (filters.min_vram_gb !== undefined) {
      models = models.filter(
        (m) => m.vram_requirements_gb >= filters.min_vram_gb!
      );
    }
    if (filters.max_vram_gb !== undefined) {
      models = models.filter(
        (m) => m.vram_requirements_gb <= filters.max_vram_gb!
      );
    }
    if (filters.model_name) {
      const searchTerm = filters.model_name.toLowerCase();
      models = models.filter((m) => {
        const regModel = m.model_name.toLowerCase();
        const regHf = m.huggingface_id.toLowerCase();
        return (
          regModel.includes(searchTerm) ||
          searchTerm.includes(regModel) ||
          regHf.includes(searchTerm) ||
          searchTerm.includes(regHf)
        );
      });
    }
  }

  return models;
}

/**
 * Get a single model by ID
 */
export function getModelById(
  registry: ModelRegistry,
  id: string
): ModelRegistryEntry | null {
  return registry.models.find((m) => m.id === id) ?? null;
}

/**
 * Suggest models that fit within the specified VRAM budget.
 * Returns models sorted by parameter count (larger first).
 */
export function suggestModelsForVRAM(
  registry: ModelRegistry,
  vramGb: number,
  parallelism = 1
): ModelRegistryEntry[] {
  const effectiveVramPerModel = vramGb / parallelism;

  const fittingModels = registry.models.filter(
    (m) => m.vram_requirements_gb <= effectiveVramPerModel
  );

  const sorted = fittingModels.sort((a, b) => {
    const parseParams = (params: string): number => {
      const match = params.match(/^(\d+(?:\.\d+)?)([BMK])?$/i);
      if (!match) return 0;
      const value = parseFloat(match[1]);
      const unit = match[2]?.toUpperCase();
      if (unit === "B") return value * 1e9;
      if (unit === "M") return value * 1e6;
      if (unit === "K") return value * 1e3;
      return value;
    };

    const paramsA = parseParams(a.parameters);
    const paramsB = parseParams(b.parameters);
    if (paramsA !== paramsB) {
      return paramsB - paramsA;
    }
    return b.vram_requirements_gb - a.vram_requirements_gb;
  });

  return sorted;
}
