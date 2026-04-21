import { z } from "zod";
import type { ModelRegistryEntry, ModelRegistry } from "./types.js";

const ModelCapabilitySchema = z.enum([
  "chat",
  "vision",
  "embedding",
  "code",
  "reasoning",
  "image_generation",
]);

const ModelFamilySchema = z.enum(["MoE", "Dense"]);

const ModelFormatTypeSchema = z.enum(["gguf", "mlx"]);

/**
 * Schema for a single weight-format entry in the formats array.
 * Zod must include this or .parse() will strip the formats field.
 */
export const ModelFormatSchema = z.object({
  type: ModelFormatTypeSchema,
  huggingface_id: z.string(),
  file_name: z.string().optional(),
});

export const ModelRegistryEntrySchema = z.object({
  id: z.string(),
  model_name: z.string(),
  huggingface_id: z.string(),
  quantization: z.string(),
  family: ModelFamilySchema,
  parameters: z.string(),
  default_context_size: z.number().int().positive(),
  max_context_size: z.number().int().positive(),
  vram_requirements_gb: z.number().positive(),
  capabilities: z.array(ModelCapabilitySchema).min(1),
  file_name: z.string(),
  mmproj_file_name: z.string().optional(),
  file_size_bytes: z.number().int().positive().nullable().optional(),
  formats: z.array(ModelFormatSchema).optional(),
});

export const ModelRegistrySchema = z.object({
  version: z.string(),
  generated_at: z.string(),
  models: z.array(ModelRegistryEntrySchema),
});

export type ModelRegistryEntryInput = z.input<typeof ModelRegistryEntrySchema>;
export type ModelRegistryInput = z.input<typeof ModelRegistrySchema>;

/**
 * Validate a model registry entry
 */
export function validateModelRegistryEntry(
  data: unknown
): ModelRegistryEntry {
  return ModelRegistryEntrySchema.parse(data);
}

/**
 * Validate a complete model registry
 */
export function validateModelRegistry(data: unknown): ModelRegistry {
  return ModelRegistrySchema.parse(data);
}
