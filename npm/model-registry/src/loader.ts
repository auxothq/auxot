import { readFileSync, statSync } from "node:fs";
import { join, dirname } from "node:path";
import { fileURLToPath } from "node:url";
import { validateModelRegistry } from "./schemas.js";
import type { ModelRegistry } from "./types.js";

const __filename = fileURLToPath(import.meta.url);
const __dirname = dirname(__filename);

let cachedRegistry: ModelRegistry | null = null;
let cachedRegistryPath: string | null = null;
let cachedRegistryMtime: number | null = null;

/**
 * Load the model registry JSON file.
 *
 * Looks for registry.json relative to this module's location.
 * Caches the result and only reloads when the file changes on disk.
 */
export function loadRegistry(): ModelRegistry {
  const registryPaths = [
    join(__dirname, "..", "..", "registry.json"), // From dist/src/ -> package root
    join(__dirname, "..", "registry.json"), // From dist/ -> package root
    join(__dirname, "registry.json"), // Same directory
  ];

  let registryPath: string | null = null;
  let lastError: Error | null = null;

  for (const path of registryPaths) {
    try {
      statSync(path);
      registryPath = path;
      break;
    } catch (error) {
      lastError =
        error instanceof Error ? error : new Error(String(error));
      continue;
    }
  }

  if (!registryPath) {
    throw new Error(
      `Failed to find registry.json. Tried: ${registryPaths.join(", ")}. ` +
        `Last error: ${lastError?.message || "unknown"}`
    );
  }

  try {
    const stats = statSync(registryPath);
    const currentMtime = stats.mtimeMs;

    if (
      cachedRegistry !== null &&
      cachedRegistryPath === registryPath &&
      cachedRegistryMtime !== null &&
      cachedRegistryMtime >= currentMtime
    ) {
      return cachedRegistry;
    }

    const content = readFileSync(registryPath, "utf-8");
    const registryData = JSON.parse(content);
    const registry = validateModelRegistry(registryData);

    cachedRegistry = registry;
    cachedRegistryPath = registryPath;
    cachedRegistryMtime = currentMtime;

    return registry;
  } catch (error) {
    if (error instanceof Error) {
      throw new Error(`Failed to load model registry: ${error.message}`);
    }
    throw error;
  }
}

/**
 * Clear the cached registry (useful for testing or hot reloading)
 */
export function clearRegistryCache(): void {
  cachedRegistry = null;
  cachedRegistryPath = null;
  cachedRegistryMtime = null;
}
