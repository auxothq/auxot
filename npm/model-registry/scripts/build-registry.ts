#!/usr/bin/env tsx

/**
 * Model Registry Build Script
 * 
 * Scans Hugging Face collections for compatible models and generates registry.json.
 * 
 * Flow:
 * 1. For each collection, get list of models
 * 2. For each model, get its files from /api/models/{repoId}
 * 3. Filter files to only GGUF files with allowed quantizations
 * 4. For each allowed quantization, create a registry entry
 * 
 * Usage: pnpm run build:registry
 */

import { writeFileSync, readFileSync, existsSync } from 'node:fs';
import { join, dirname } from 'node:path';
import { fileURLToPath } from 'node:url';
import type { ModelRegistry, ModelRegistryEntry } from '../src/types.js';
import { ModelRegistrySchema } from '../src/schemas.js';

const __filename = fileURLToPath(import.meta.url);
const __dirname = dirname(__filename);
const PACKAGE_ROOT = join(__dirname, '..');
const REGISTRY_PATH = join(PACKAGE_ROOT, 'registry.json');

/**
 * Collections to scan for models
 */
interface Collection {
  owner: string;
  collection: string;
}

const COLLECTIONS: Collection[] = [
  // Unsloth collections
  { owner: 'unsloth', collection: 'Qwen3-VL' },
  { owner: 'unsloth', collection: 'Qwen3' },
  { owner: 'unsloth', collection: 'Qwen3.5' },
  { owner: 'unsloth', collection: 'Ministral-3' },
  { owner: 'unsloth', collection: 'DeepSeek-V3.1' },
  { owner: 'unsloth', collection: 'DeepSeek-R1' },
  { owner: 'unsloth', collection: 'Qwen3-Coder' },
  { owner: 'unsloth', collection: 'Gemma-3' },
  { owner: 'unsloth', collection: 'Gemma-3n' },
  { owner: 'unsloth', collection: 'Llama-4' },
  { owner: 'unsloth', collection: 'Llama-3.3' },
  { owner: 'unsloth', collection: 'gpt-oss' },
  { owner: 'unsloth', collection: 'Granite-4.0' },
  
  // Base provider collections
  { owner: 'Qwen', collection: 'Qwen3' },
  { owner: 'meta-llama', collection: 'Llama-4' },
  { owner: 'meta-llama', collection: 'Llama-3.3' },
  { owner: 'mistralai', collection: 'Ministral-3' },
  { owner: 'mistralai', collection: 'Devstral-2' },
  { owner: 'deepseek-ai', collection: 'DeepSeek-V3' },
  { owner: 'google', collection: 'Gemma-3' },
];

/**
 * Direct model repos (not part of any collection)
 * 
 * Some models exist on HuggingFace but aren't organized into a collection.
 * These are specified by their full repo ID directly.
 */
const DIRECT_MODELS: string[] = [
  'unsloth/MiniMax-M2.1-GGUF',
  'unsloth/Kimi-K2.5-GGUF',
  'unsloth/Kimi-K2-Instruct-GGUF',
];

/**
 * Image generation models (stable-diffusion.cpp, not llama.cpp).
 * These use capability 'image_generation' and require the stable-diffusion.cpp binary.
 */
const IMAGE_GEN_MODELS: ModelRegistryEntry[] = [
  {
    id: 'flux-2-klein-4b-q4-k-s',
    model_name: 'FLUX.2-klein-4B',
    huggingface_id: 'unsloth/FLUX.2-klein-4B-GGUF',
    quantization: 'Q4_K_S',
    family: 'Dense',
    parameters: '4B',
    default_context_size: 4096,
    max_context_size: 4096,
    vram_requirements_gb: 6,
    capabilities: ['image_generation'],
    file_name: 'flux-2-klein-4b-Q4_K_S.gguf',
  },
  {
    id: 'flux-2-klein-9b-q4-k-s',
    model_name: 'FLUX.2-klein-9B',
    huggingface_id: 'unsloth/FLUX.2-klein-9B-GGUF',
    quantization: 'Q4_K_S',
    family: 'Dense',
    parameters: '9B',
    default_context_size: 4096,
    max_context_size: 4096,
    vram_requirements_gb: 8,
    capabilities: ['image_generation'],
    file_name: 'flux-2-klein-9b-Q4_K_S.gguf',
  },
  {
    id: 'flux1-schnell-q4-k-s',
    model_name: 'FLUX.1-schnell',
    huggingface_id: 'unsloth/FLUX.1-schnell-GGUF',
    quantization: 'Q4_K_S',
    family: 'Dense',
    parameters: '12B',
    default_context_size: 4096,
    max_context_size: 4096,
    vram_requirements_gb: 8,
    capabilities: ['image_generation'],
    file_name: 'flux1-schnell-Q4_K_S.gguf',
  },
];

/**
 * Allowed quantization patterns
 */
const QUANTIZATION_PATTERNS = [
  'Q3_K_S',
  'Q4_K_S',
  'Q5_K_S',
  'Q6_K',
  'Q8_0',
  'Q8_K',
  'F16',
  'F32',
  'BF16',
];

/**
 * Allowed model families and their configurations
 */
interface ModelFamilyConfig {
  name: string;
  versions: string[];
  variants: string[];
  providers: string[];
}

const ALLOWED_MODEL_FAMILIES: ModelFamilyConfig[] = [
  {
    name: 'Qwen',
    versions: ['3', '3.5'],
    variants: ['Instruct', 'Coder', 'Omni', 'VL'],
    providers: ['Qwen', 'unsloth'],
  },
  {
    name: 'Llama',
    versions: ['4', '3.3'],
    variants: ['Instruct', 'Maverick', 'Scout'],
    providers: ['meta-llama', 'unsloth'],
  },
  {
    name: 'DeepSeek',
    versions: ['V3.2', 'V3.1', 'V2', 'R1'],
    variants: ['Base', 'Zero', 'Terminus'],
    providers: ['deepseek-ai', 'unsloth'],
  },
  {
    name: 'Ministral',
    versions: ['3', '2'],
    variants: ['Instruct', 'Reasoning'],
    providers: ['mistralai', 'unsloth'],
  },
  {
    name: 'Gemma',
    versions: ['3', '3n'],
    variants: ['Instruct'],
    providers: ['google', 'unsloth'],
  },
  {
    name: 'GPT-OSS',
    versions: ['*'],
    variants: ['*'],
    providers: ['unsloth'],
  },
  {
    name: 'Granite',
    versions: ['4.0'],
    variants: ['*'],
    providers: ['unsloth'],
  },
  {
    name: 'MiniMax',
    versions: ['M2.1'],
    variants: ['*'],
    providers: ['unsloth'],
  },
  {
    name: 'Kimi',
    versions: ['K2.5', 'K2'],
    variants: ['Instruct', '*'],
    providers: ['unsloth'],
  },
];

/**
 * Fetch with retry and exponential backoff
 */
async function fetchWithRetry(
  url: string,
  options: { maxRetries?: number; retryDelay?: number } = {}
): Promise<Response> {
  const { maxRetries = 3, retryDelay = 2000 } = options;
  
  for (let attempt = 0; attempt <= maxRetries; attempt++) {
    const response = await fetch(url, {
      headers: {
        'User-Agent': 'Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0.0.0 Safari/537.36',
      },
    });
    
    // Success or non-retryable error
    if (response.ok || (response.status !== 429 && response.status < 500)) {
      return response;
    }
    
    // Rate limit (429) or server error (5xx) - retry
    if (attempt < maxRetries) {
      const isRateLimit = response.status === 429;
      
      // For rate limits, check Retry-After header if available
      const retryAfter = response.headers.get('Retry-After');
      let waitTime: number;
      
      if (retryAfter) {
        waitTime = parseInt(retryAfter, 10) * 1000;
      } else if (isRateLimit) {
        // No Retry-After header - use exponential backoff with jitter
        const baseDelay = retryDelay * Math.pow(2, attempt);
        const jitter = Math.random() * 1000; // Add up to 1s jitter
        waitTime = baseDelay + jitter;
      } else {
        // Server error - use exponential backoff
        waitTime = retryDelay * Math.pow(2, attempt);
      }
      
      const statusText = isRateLimit ? 'rate limited' : `server error ${response.status}`;
      console.warn(`  Request failed (${statusText}), retrying in ${Math.round(waitTime / 1000)}s... (attempt ${attempt + 1}/${maxRetries})`);
      
      await new Promise(resolve => setTimeout(resolve, waitTime));
    } else {
      return response; // Return the failed response after max retries
    }
  }
  
  throw new Error('Max retries exceeded');
}

/**
 * Fetch models from a Hugging Face collection
 */
async function fetchModelsFromCollection(owner: string, collection: string): Promise<string[]> {
  try {
    // Search for collections
    const searchUrl = `https://huggingface.co/api/collections?owner=${owner}&q=${encodeURIComponent(collection)}`;
    const searchResponse = await fetchWithRetry(searchUrl);
    
    if (!searchResponse.ok) {
      console.warn(`  Failed to search collections: ${searchResponse.status}`);
      return [];
    }
    
    const collections: any[] = (await searchResponse.json()) as any[];
    if (!Array.isArray(collections) || collections.length === 0) {
      console.warn(`  Collection "${collection}" not found`);
      return [];
    }
    
    // Score and sort collections by match quality
    const normalizedQuery = collection.toLowerCase().trim();
    const scoredCollections = collections.map((col: any) => {
      const title = (col.title || '').toLowerCase();
      const slug = (col.slug || '').toLowerCase();
      let score = 0;
      
      if (title === normalizedQuery || slug.includes(normalizedQuery.replace(/\s+/g, '-'))) {
        score = 100;
      } else if (title.startsWith(normalizedQuery) || slug.startsWith(normalizedQuery.replace(/\s+/g, '-'))) {
        score = 50;
      } else if (title.includes(normalizedQuery) || slug.includes(normalizedQuery.replace(/\s+/g, '-'))) {
        score = 25;
      } else {
        score = 10;
      }
      
      if (title !== normalizedQuery && slug !== normalizedQuery.replace(/\s+/g, '-')) {
        const titleParts = title.split(/[-_\s]+/);
        const queryParts = normalizedQuery.split(/[-_\s]+/);
        if (titleParts.length > queryParts.length && !titleParts.every((part: string) => queryParts.includes(part))) {
          score -= 20;
        }
      }
      
      return { collection: col, score };
    });
    
    scoredCollections.sort((a, b) => {
      if (b.score !== a.score) return b.score - a.score;
      return (a.collection.title || '').localeCompare(b.collection.title || '');
    });
    
    const matchedCollection = scoredCollections[0].collection;
    console.log(`  Found collection: "${matchedCollection.title}" (slug: ${matchedCollection.slug})`);
    
    // Fetch full collection details
    const collectionUrl = `https://huggingface.co/api/collections/${matchedCollection.slug}`;
    const collectionResponse = await fetchWithRetry(collectionUrl);
    
    if (!collectionResponse.ok) {
      console.warn(`  Failed to fetch collection: ${collectionResponse.status}`);
      return [];
    }
    
    const collectionData: any = await collectionResponse.json();
    
    // Extract model IDs
    const modelIds: string[] = [];
    if (collectionData.items && Array.isArray(collectionData.items)) {
      for (const item of collectionData.items) {
        if (item.type === 'model' && item.id && typeof item.id === 'string') {
          const repoId = item.id;
          
          // Skip models that won't have GGUF files (save API requests)
          const repoIdLower = repoId.toLowerCase();
          if (
            repoIdLower.includes('bnb') || // Skip bnb-4bit variants
            repoIdLower.includes('fp8') || // Skip FP8 variants
            (!repoIdLower.endsWith('-gguf') && !repoIdLower.includes('/gguf')) // Skip if no -GGUF suffix
          ) {
            continue;
          }
          
          modelIds.push(repoId);
        }
      }
    }
    
    console.log(`  Found ${modelIds.length} models (after filtering)`);
    return modelIds;
  } catch (error) {
    console.warn(`  Error fetching collection ${owner}/${collection}:`, error instanceof Error ? error.message : String(error));
    return [];
  }
}

/**
 * Get model files and extract parameters from Hugging Face API
 */
async function getModelFiles(repoId: string): Promise<{
  files: Array<{ path: string; size?: number }>;
  parameters?: string;
  maxContextSize?: number;
}> {
  try {
    const url = `https://huggingface.co/api/models/${repoId}`;
    const response = await fetchWithRetry(url);
    
    if (!response.ok) {
      return { files: [] };
    }
    
    const modelData: any = await response.json();
    const siblings = modelData.siblings || [];
    
    const files = siblings
      .filter((sibling: any) => sibling.rfilename && sibling.rfilename.endsWith('.gguf'))
      .map((sibling: any) => ({ 
        path: sibling.rfilename, 
        size: sibling.size 
      }));
    
    // Extract parameters from API metadata
    let parameters: string | undefined;
    
    // For GGUF models, check gguf.total field
    if (modelData.gguf?.total && typeof modelData.gguf.total === 'number') {
      const totalParams = modelData.gguf.total;
      if (totalParams >= 1e9) {
        parameters = `${Math.round(totalParams / 1e9)}B`;
      } else if (totalParams >= 1e6) {
        parameters = `${Math.round(totalParams / 1e6)}M`;
      } else if (totalParams >= 1e3) {
        parameters = `${Math.round(totalParams / 1e3)}K`;
      }
    }
    // Fallback: check safetensors.parameters (for non-GGUF models, but we only process GGUF)
    else if (modelData.safetensors?.parameters) {
      // Try BF16 first, then F32, then total
      const params = modelData.safetensors.parameters;
      const paramCount = params.BF16 || params.F32 || modelData.safetensors.total;
      if (typeof paramCount === 'number' && paramCount >= 1e9) {
        parameters = `${Math.round(paramCount / 1e9)}B`;
      } else if (typeof paramCount === 'number' && paramCount >= 1e6) {
        parameters = `${Math.round(paramCount / 1e6)}M`;
      }
    }
    
    // Extract max context size from gguf.context_length if available
    let maxContextSize: number | undefined;
    if (modelData.gguf?.context_length && typeof modelData.gguf.context_length === 'number') {
      maxContextSize = modelData.gguf.context_length;
    }
    // Fallback: try to fetch config.json to get max_position_embeddings
    else {
      try {
        const configUrl = `https://huggingface.co/${repoId}/raw/main/config.json`;
        const configResponse = await fetchWithRetry(configUrl);
        if (configResponse.ok) {
          const config: any = await configResponse.json();
          // Check various possible fields for max context
          if (config.max_position_embeddings && typeof config.max_position_embeddings === 'number') {
            maxContextSize = config.max_position_embeddings;
          } else if (config.n_positions && typeof config.n_positions === 'number') {
            maxContextSize = config.n_positions;
          } else if (config.seq_length && typeof config.seq_length === 'number') {
            maxContextSize = config.seq_length;
          } else if (config.model_max_length && typeof config.model_max_length === 'number') {
            maxContextSize = config.model_max_length;
          }
        }
      } catch {
        // Ignore config.json fetch errors - max_context_size will be undefined
      }
    }
    
    return { files, parameters, maxContextSize };
  } catch (error) {
    return { files: [] };
  }
}

/**
 * Check if file is an mmproj (vision encoder), not the main LLM
 */
function isMmprojFile(path: string): boolean {
  const basename = path.split('/').pop() || path;
  return /mmproj/i.test(basename);
}

/**
 * Pick preferred mmproj from repo files. Order: F16 → BF16 → F32
 */
function pickPreferredMmproj(files: Array<{ path: string }>): string | undefined {
  const mmprojFiles = files.filter((f) => isMmprojFile(f.path));
  if (mmprojFiles.length === 0) return undefined;
  for (const pref of ['F16', 'BF16', 'F32']) {
    const match = mmprojFiles.find((f) => f.path.includes(`mmproj-${pref}`) || f.path.includes(`mmproj.${pref}`));
    if (match) return match.path;
  }
  return mmprojFiles[0].path;
}

/**
 * Extract quantization from filename
 */
function extractQuantization(filename: string): string | null {
  for (const pattern of QUANTIZATION_PATTERNS) {
    if (filename.includes(pattern)) {
      return pattern;
    }
  }
  return null;
}

/**
 * Extract parameters from text
 */
function extractParameters(text: string): string | null {
  const match = text.match(/(\d+(?:\.\d+)?)(B|M|K)(?!\w)/i);
  if (match) {
    return `${match[1]}${match[2].toUpperCase()}`;
  }
  return null;
}

/**
 * Normalize model name
 */
function normalizeModelName(repoId: string): string {
  let name = repoId.replace(/-GGUF$/i, '');
  name = name.replace(/-Instruct-GGUF$/i, '');
  name = name.replace(/-Instruct$/i, '');
  const parts = name.split('/');
  if (parts.length > 1) {
    name = parts[1];
  }
  return name;
}

/**
 * Check if model is allowed
 */
function isModelAllowed(repoId: string, modelName: string, provider: string): { allowed: boolean; family?: string; variant?: string } {
  for (const familyConfig of ALLOWED_MODEL_FAMILIES) {
    if (!familyConfig.providers.includes(provider)) continue;
    
    // Check family match
    let familyMatch = false;
    if (familyConfig.name === 'Qwen') {
      familyMatch = /^Qwen/i.test(modelName);
    } else if (familyConfig.name === 'Llama') {
      familyMatch = /^Llama/i.test(modelName) || /^Meta-Llama/i.test(modelName);
    } else if (familyConfig.name === 'DeepSeek') {
      familyMatch = /^DeepSeek/i.test(modelName);
    } else if (familyConfig.name === 'Ministral') {
      familyMatch = /^(Ministral|Devstral)/i.test(modelName);
    } else if (familyConfig.name === 'Gemma') {
      familyMatch = /^Gemma/i.test(modelName);
    } else if (familyConfig.name === 'GPT-OSS') {
      familyMatch = /^gpt-oss/i.test(modelName) || /^GPT-OSS/i.test(modelName);
    } else if (familyConfig.name === 'Granite') {
      familyMatch = /^Granite/i.test(modelName);
    } else if (familyConfig.name === 'MiniMax') {
      familyMatch = /^MiniMax/i.test(modelName);
    } else if (familyConfig.name === 'Kimi') {
      familyMatch = /^Kimi/i.test(modelName);
    }
    
    if (!familyMatch) continue;
    
    // Check version
    let versionMatch = false;
    if (familyConfig.name === 'Qwen') {
      versionMatch = /Qwen[_-]?3(?:[^0-9]|$)/i.test(modelName) || 
                     /^Qwen3/i.test(modelName) ||
                     /Qwen[_-]?3\.5/i.test(modelName) ||
                     /Qwen3\.5/i.test(modelName);
    } else if (familyConfig.name === 'Llama') {
      versionMatch = /Llama[_-]?4(?:[^0-9]|$)/i.test(modelName) ||
                     /Llama[_-]?3\.3/i.test(modelName) ||
                     /Meta-Llama-4/i.test(modelName) ||
                     /Meta-Llama-3\.3/i.test(modelName);
    } else if (familyConfig.name === 'DeepSeek') {
      versionMatch = /DeepSeek[_-]?V?3\.2/i.test(modelName) ||
                     /DeepSeek[_-]?V?3\.1/i.test(modelName) ||
                     /DeepSeekCoder[_-]?V?2/i.test(modelName) ||
                     /DeepSeek[_-]?R1/i.test(modelName);
    } else if (familyConfig.name === 'Ministral') {
      versionMatch = /Ministral[_-]?3/i.test(modelName) ||
                     /Ministral[_-]?Large[_-]?3/i.test(modelName) ||
                     /Devstral[_-]?2/i.test(modelName);
    } else if (familyConfig.name === 'Gemma') {
      versionMatch = /Gemma[_-]?3(?:n)?/i.test(modelName);
    } else if (familyConfig.name === 'GPT-OSS') {
      versionMatch = true;
    } else if (familyConfig.name === 'Granite') {
      versionMatch = /Granite[_-]?4\.0/i.test(modelName) || /Granite[_-]?4/i.test(modelName);
    } else if (familyConfig.name === 'MiniMax') {
      versionMatch = /MiniMax[_-]?M2\.1/i.test(modelName) || /MiniMax[_-]?M2/i.test(modelName);
    } else if (familyConfig.name === 'Kimi') {
      versionMatch = /Kimi[_-]?K2\.5/i.test(modelName) || /Kimi[_-]?K2/i.test(modelName);
    }
    
    if (!versionMatch) continue;
    
    // Check variant
    let matchedVariant: string | undefined;
    for (const variant of familyConfig.variants) {
      if (variant === '*') {
        matchedVariant = 'Base';
        break;
      }
      const variantPattern = new RegExp(`-${variant}(?:-|$|GGUF)`, 'i');
      if (variantPattern.test(repoId) || variantPattern.test(modelName)) {
        matchedVariant = variant;
        break;
      }
    }
    
    if (familyConfig.name === 'DeepSeek' && !matchedVariant && familyConfig.variants.includes('Base')) {
      const hasVariantSuffix = familyConfig.variants.some(v => 
        v !== 'Base' && (
          new RegExp(`-${v}(?:-|$|GGUF)`, 'i').test(repoId) || 
          new RegExp(`-${v}(?:-|$|GGUF)`, 'i').test(modelName)
        )
      );
      if (!hasVariantSuffix) {
        matchedVariant = 'Base';
      }
    }
    
    return {
      allowed: true,
      family: familyConfig.name,
      variant: matchedVariant || 'Base',
    };
  }
  
  return { allowed: false };
}

/**
 * Infer capabilities from model name
 */
function inferCapabilities(modelName: string): Array<'chat' | 'vision' | 'embedding' | 'code' | 'reasoning'> {
  const text = modelName.toLowerCase();
  const capabilities: Array<'chat' | 'vision' | 'embedding' | 'code' | 'reasoning'> = [];
  
  if (text.includes('vision') || text.includes('multimodal') || text.includes('vl-')) {
    capabilities.push('vision');
  }
  if (text.includes('code') || text.includes('coder') || text.includes('starcoder')) {
    capabilities.push('code');
  }
  if (text.includes('embed') || text.includes('embedding')) {
    capabilities.push('embedding');
  }
  // Reasoning-capable models: DeepSeek-R1, Kimi K2.5, QwQ, etc.
  if (text.includes('deepseek-r1') || text.includes('kimi') || text.includes('qwq')) {
    capabilities.push('reasoning');
  }
  if (capabilities.length === 0) {
    capabilities.push('chat');
  }
  
  return [...new Set(capabilities)];
}

/**
 * Infer family type
 */
function inferFamily(modelName: string): 'MoE' | 'Dense' {
  const text = modelName.toLowerCase();
  if (text.includes('mixtral') || text.includes('moe') || text.includes('mixture-of-experts') || text.includes('minimax') || text.includes('kimi')) {
    return 'MoE';
  }
  if (text.includes('72b') && text.includes('qwen2.5')) {
    return 'MoE';
  }
  // Check for MoE pattern: X-B-AY-B (e.g., "30B-A3B", "235B-A22B", "480B-A35B")
  // This pattern indicates total parameters (X) with expert parameters (AY)
  if (/\d+b-a\d+b/i.test(modelName)) {
    return 'MoE';
  }
  if (text.includes('235b') || text.includes('a22b')) {
    return 'MoE';
  }
  return 'Dense';
}

/**
 * Get default context size
 */
function getDefaultContextSize(modelName: string): number {
  const name = modelName.toLowerCase();
  if (name.includes('qwen')) return 32768;
  if (name.includes('llama-3') || name.includes('llama3')) return 131072;
  if (name.includes('llama-2') || name.includes('llama2')) return 4096;
  if (name.includes('mistral')) return 32768;
  if (name.includes('minimax')) return 8192;
  if (name.includes('kimi')) return 131072;
  return 8192;
}

/**
 * Calculate VRAM requirements
 */
function calculateVRAMRequirements(parameters: string, quantization: string): number {
  const paramMatch = parameters.match(/(\d+(?:\.\d+)?)(B|M|K)/i);
  if (!paramMatch) return 0;
  
  const value = parseFloat(paramMatch[1]);
  const unit = paramMatch[2].toUpperCase();
  let baseParams = 0;
  
  if (unit === 'B') baseParams = value * 1e9;
  else if (unit === 'M') baseParams = value * 1e6;
  else if (unit === 'K') baseParams = value * 1e3;
  else return 0;
  
  const quantMultipliers: Record<string, number> = {
    'Q3_K_S': 0.3125,
    'Q4_K_S': 0.5,
    'Q5_K_S': 0.625,
    'Q6_K': 0.75,
    'Q8_0': 1.0,
    'Q8_K': 1.0,
    'F16': 2.0,
    'F32': 4.0,
    'BF16': 2.0,
  };
  
  const multiplier = quantMultipliers[quantization] || 0.5;
  const vramBytes = baseParams * multiplier;
  return Math.ceil(vramBytes / 1e9 * 10) / 10; // Round to 1 decimal place
}

/**
 * Generate model ID
 */
function generateModelId(modelName: string, quantization: string): string {
  return `${modelName.toLowerCase().replace(/[^a-z0-9]+/g, '-')}-${quantization.toLowerCase().replace(/[^a-z0-9]+/g, '-')}`;
}

/**
 * Process a single HuggingFace repo and return registry entries for it.
 */
async function processRepo(
  repoId: string,
  processedIds: Set<string>,
): Promise<ModelRegistryEntry[]> {
  const entries: ModelRegistryEntry[] = [];

  console.log(`  Model: ${repoId}`);

  const { files, parameters: apiParameters, maxContextSize: apiMaxContextSize } = await getModelFiles(repoId);
  console.log(`    Found ${files.length} GGUF files`);

  // Delay between models to reduce rate limiting (300ms-800ms)
  const delay = 300 + Math.random() * 500;
  await new Promise(resolve => setTimeout(resolve, delay));

  if (files.length === 0) {
    console.log(`    ⊘ Skipped - no GGUF files`);
    return entries;
  }

  const modelName = normalizeModelName(repoId);
  const provider = repoId.split('/')[0];
  const parameters = apiParameters || extractParameters(repoId) || extractParameters(modelName);

  if (!parameters) {
    console.log(`    ⊘ Skipped - could not extract parameters`);
    return entries;
  }

  const modelCheck = isModelAllowed(repoId, modelName, provider);
  if (!modelCheck.allowed) {
    console.log(`    ⊘ Skipped - not in allowed families/versions`);
    return entries;
  }

  // Special filter for Granite
  if (modelCheck.family === 'Granite') {
    const paramMatch = parameters.match(/(\d+(?:\.\d+)?)(B|M|K)/i);
    if (paramMatch) {
      const value = parseFloat(paramMatch[1]);
      const unit = paramMatch[2].toUpperCase();
      if (unit === 'B' && value !== 32 && value !== 7) {
        console.log(`    ⊘ Skipped - Granite only allows 32B and 7B`);
        return entries;
      }
    }
  }

  // Pick preferred mmproj (F16 → BF16 → F32) if repo has vision encoder
  const preferredMmproj = pickPreferredMmproj(files);

  for (const file of files) {
    // Skip mmproj files — they are vision encoders, not main LLM. Prevents F32 entries
    // incorrectly pointing to mmproj-F32.gguf instead of actual model.
    if (isMmprojFile(file.path)) continue;

    const quantization = extractQuantization(file.path);
    if (!quantization) continue;
    if (!QUANTIZATION_PATTERNS.includes(quantization)) continue;

    const modelId = generateModelId(modelName, quantization);
    if (processedIds.has(modelId)) continue;
    processedIds.add(modelId);

    let capabilities = inferCapabilities(modelName);
    if (preferredMmproj && !capabilities.includes('vision')) {
      capabilities = [...capabilities, 'vision'];
    }
    const family = inferFamily(modelName);
    const defaultContextSize = getDefaultContextSize(modelName);
    const maxContextSize = apiMaxContextSize || (defaultContextSize * 4);
    const vramGb = calculateVRAMRequirements(parameters, quantization);

    if (vramGb <= 0) continue;

    const entry: ModelRegistryEntry = {
      id: modelId,
      model_name: modelName,
      huggingface_id: repoId,
      quantization,
      family,
      parameters,
      default_context_size: defaultContextSize,
      max_context_size: maxContextSize,
      vram_requirements_gb: vramGb,
      capabilities,
      file_name: file.path,
      file_size_bytes: file.size,
    };
    if (preferredMmproj) {
      entry.mmproj_file_name = preferredMmproj;
    }
    entries.push(entry);

    console.log(`    ✓ Added ${quantization} - ${parameters} - ${vramGb}GB VRAM - max context: ${maxContextSize.toLocaleString()}`);
  }

  return entries;
}

/**
 * Sort models by parameter count (descending).
 */
function sortByParams(models: ModelRegistryEntry[]): ModelRegistryEntry[] {
  const parseParams = (params: string): number => {
    const match = params.match(/(\d+(?:\.\d+)?)(B|M|K)/i);
    if (!match) return 0;
    const value = parseFloat(match[1]);
    const unit = match[2].toUpperCase();
    if (unit === 'B') return value * 1e9;
    if (unit === 'M') return value * 1e6;
    if (unit === 'K') return value * 1e3;
    return value;
  };
  return [...models].sort((a, b) => parseParams(b.parameters) - parseParams(a.parameters));
}

/**
 * Write the final registry to disk.
 */
function writeRegistry(models: ModelRegistryEntry[]): ModelRegistry {
  const sorted = sortByParams(models);

  const registry: ModelRegistry = {
    version: '0.1.0',
    generated_at: new Date().toISOString(),
    models: sorted,
  };

  const validated = ModelRegistrySchema.parse(registry);
  writeFileSync(REGISTRY_PATH, JSON.stringify(validated, null, 2) + '\n', 'utf-8');

  console.log(`\n✓ Registry written: ${validated.models.length} models`);
  console.log(`  Written to: ${REGISTRY_PATH}`);

  return validated;
}

/**
 * Load existing registry from disk (returns empty model list if missing).
 */
function loadExistingRegistry(): ModelRegistryEntry[] {
  if (!existsSync(REGISTRY_PATH)) {
    return [];
  }
  try {
    const raw = JSON.parse(readFileSync(REGISTRY_PATH, 'utf-8'));
    const parsed = ModelRegistrySchema.parse(raw);
    return parsed.models;
  } catch (err) {
    console.warn('⚠ Could not parse existing registry, starting fresh:', err instanceof Error ? err.message : String(err));
    return [];
  }
}

/**
 * Full build — scan every collection and direct model.
 */
async function buildRegistry(): Promise<ModelRegistry> {
  console.log('Building model registry (full)...');
  console.log(`Scanning ${COLLECTIONS.length} collections...\n`);

  const models: ModelRegistryEntry[] = [];
  const processedIds = new Set<string>();

  for (const { owner, collection } of COLLECTIONS) {
    console.log(`Scanning ${owner}/${collection}...`);
    const modelIds = await fetchModelsFromCollection(owner, collection);

    if (modelIds.length > 0) {
      const delay = 500 + Math.random() * 1000;
      await new Promise(resolve => setTimeout(resolve, delay));
    }

    for (const repoId of modelIds) {
      if (!repoId || typeof repoId !== 'string') continue;
      const entries = await processRepo(repoId, processedIds);
      models.push(...entries);
    }
  }

  if (DIRECT_MODELS.length > 0) {
    console.log(`\nProcessing ${DIRECT_MODELS.length} direct model repos...`);
    for (const repoId of DIRECT_MODELS) {
      const entries = await processRepo(repoId, processedIds);
      models.push(...entries);
    }
  }

  // Add image generation models (stable-diffusion.cpp)
  const ids = new Set(models.map((m) => m.id));
  for (const entry of IMAGE_GEN_MODELS) {
    if (!ids.has(entry.id)) {
      models.push(entry);
      ids.add(entry.id);
    }
  }

  return writeRegistry(models);
}

/**
 * Patch mode — only fetch models matching a pattern and merge into existing registry.
 *
 * Usage: pnpm run build:registry -- --patch "Kimi"
 *
 * This loads registry.json, removes any entries whose huggingface_id or model_name
 * matches the pattern (case-insensitive), fetches fresh data for matching repos
 * from both COLLECTIONS and DIRECT_MODELS, merges the new entries in, and writes
 * the result back. Much faster than a full rebuild.
 */
async function patchRegistry(pattern: string): Promise<ModelRegistry> {
  const regex = new RegExp(pattern, 'i');

  console.log(`Patching registry with pattern: /${pattern}/i\n`);

  // 1. Load existing models and remove stale entries matching the pattern
  const existing = loadExistingRegistry();
  const kept = existing.filter(m => !regex.test(m.huggingface_id) && !regex.test(m.model_name));
  const removed = existing.length - kept.length;
  if (removed > 0) {
    console.log(`Removed ${removed} existing entries matching pattern`);
  }

  // 2. Collect repo IDs that match from collections + direct models
  const repoIds: string[] = [];

  // Check collections for matching repos
  for (const { owner, collection } of COLLECTIONS) {
    // Quick check: does the collection name match?
    if (!regex.test(collection) && !regex.test(owner)) continue;

    console.log(`Scanning ${owner}/${collection}...`);
    const modelIds = await fetchModelsFromCollection(owner, collection);
    for (const id of modelIds) {
      if (regex.test(id)) {
        repoIds.push(id);
      }
    }

    if (modelIds.length > 0) {
      const delay = 500 + Math.random() * 1000;
      await new Promise(resolve => setTimeout(resolve, delay));
    }
  }

  // Check direct models
  for (const repoId of DIRECT_MODELS) {
    if (regex.test(repoId)) {
      repoIds.push(repoId);
    }
  }

  if (repoIds.length === 0) {
    console.log(`\n⚠ No repos matched pattern "${pattern}".`);
    console.log('  Check COLLECTIONS and DIRECT_MODELS lists.');
    process.exit(1);
  }

  console.log(`\nFound ${repoIds.length} matching repos:`);
  for (const id of repoIds) {
    console.log(`  - ${id}`);
  }
  console.log('');

  // 3. Fetch fresh data for matching repos
  const processedIds = new Set(kept.map(m => m.id));
  const newModels: ModelRegistryEntry[] = [];

  for (const repoId of repoIds) {
    const entries = await processRepo(repoId, processedIds);
    newModels.push(...entries);
  }

  if (newModels.length === 0) {
    console.log('\n⚠ No new model entries were produced. Check allowed families/quantizations.');
    process.exit(1);
  }

  console.log(`\nPatch summary: kept ${kept.length}, added ${newModels.length}`);

  // 4. Merge and write
  return writeRegistry([...kept, ...newModels]);
}

/**
 * Update mode — scan everything but skip repos already in the registry.
 *
 * Usage: pnpm run build:registry -- --update
 *
 * Loads registry.json, builds a set of huggingface_id values already present,
 * then scans all collections and direct models. Any repo that already has at
 * least one entry is skipped entirely (no API calls). Only truly new repos
 * get fetched and appended. Great for incremental updates after committing
 * registry.json to git.
 */
async function updateRegistry(): Promise<ModelRegistry> {
  console.log('Updating model registry (incremental)...');

  // 1. Load existing registry and build skip-set of known repos
  const existing = loadExistingRegistry();
  const knownRepos = new Set(existing.map(m => m.huggingface_id));
  const processedIds = new Set(existing.map(m => m.id));

  console.log(`Existing registry: ${existing.length} entries from ${knownRepos.size} repos\n`);

  const newModels: ModelRegistryEntry[] = [];
  let skippedRepos = 0;

  // 2. Scan collections — skip repos we already have
  console.log(`Scanning ${COLLECTIONS.length} collections...\n`);

  for (const { owner, collection } of COLLECTIONS) {
    console.log(`Scanning ${owner}/${collection}...`);
    const modelIds = await fetchModelsFromCollection(owner, collection);

    if (modelIds.length > 0) {
      const delay = 500 + Math.random() * 1000;
      await new Promise(resolve => setTimeout(resolve, delay));
    }

    for (const repoId of modelIds) {
      if (!repoId || typeof repoId !== 'string') continue;

      if (knownRepos.has(repoId)) {
        skippedRepos++;
        continue;
      }

      const entries = await processRepo(repoId, processedIds);
      newModels.push(...entries);
    }
  }

  // 3. Scan direct models — skip repos we already have
  if (DIRECT_MODELS.length > 0) {
    console.log(`\nProcessing ${DIRECT_MODELS.length} direct model repos...`);
    for (const repoId of DIRECT_MODELS) {
      if (knownRepos.has(repoId)) {
        skippedRepos++;
        continue;
      }

      const entries = await processRepo(repoId, processedIds);
      newModels.push(...entries);
    }
  }

  console.log(`\nUpdate summary: ${skippedRepos} repos skipped (already present), ${newModels.length} new entries added`);

  const merged = [...existing, ...newModels];
  const ids = new Set(merged.map((m) => m.id));
  for (const entry of IMAGE_GEN_MODELS) {
    if (!ids.has(entry.id)) {
      merged.push(entry);
      ids.add(entry.id);
    }
  }

  return writeRegistry(merged);
}

// --- CLI entry point ---

const args = process.argv.slice(2);
const patchIndex = args.indexOf('--patch');
const updateMode = args.includes('--update');

if (patchIndex !== -1) {
  const pattern = args[patchIndex + 1];
  if (!pattern) {
    console.error('Usage: pnpm run build:registry -- --patch <pattern>');
    console.error('  e.g.: pnpm run build:registry -- --patch "Kimi"');
    process.exit(1);
  }
  patchRegistry(pattern).catch((error) => {
    console.error('Error patching registry:', error);
    process.exit(1);
  });
} else if (updateMode) {
  updateRegistry().catch((error) => {
    console.error('Error updating registry:', error);
    process.exit(1);
  });
} else {
  buildRegistry().catch((error) => {
    console.error('Error building registry:', error);
    process.exit(1);
  });
}
