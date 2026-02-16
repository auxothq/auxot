// Package registry provides the model registry — a curated catalog of
// LLM models with their quantization variants, VRAM requirements, and capabilities.
//
// The registry data is embedded into the binary at compile time via //go:embed.
// This means every auxot-router and auxot-worker binary ships with a complete
// model catalog, no network needed.
//
// The registry can be overridden at runtime with --registry-file for custom
// or fine-tuned models.
//
// To update registry.json from the published npm package:
//
//	make sync-registry
//	# or: go generate ./pkg/registry/
//go:generate make -C ../.. sync-registry
package registry

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"os"
	"regexp"
	"strconv"
	"strings"
)

//go:embed registry.json
var embeddedRegistryJSON []byte

// Registry is the top-level structure containing all known models.
type Registry struct {
	Version     string  `json:"version"`
	GeneratedAt string  `json:"generated_at"`
	Models      []Model `json:"models"`
}

// Model represents a single model+quantization entry.
type Model struct {
	ID                string   `json:"id"`
	ModelName         string   `json:"model_name"`
	HuggingFaceID     string   `json:"huggingface_id"`
	Quantization      string   `json:"quantization"`
	Family            string   `json:"family"`
	Parameters        string   `json:"parameters"`
	DefaultContextSize int     `json:"default_context_size"`
	MaxContextSize    int      `json:"max_context_size"`
	VRAMRequirementsGB float64 `json:"vram_requirements_gb"`
	Capabilities      []string `json:"capabilities"`
	FileName          string   `json:"file_name"`
	FileSizeBytes     *int64   `json:"file_size_bytes,omitempty"`
}

// Load parses the embedded registry data and returns a Registry.
// This is cheap to call — the JSON is already in memory, just needs parsing.
func Load() (*Registry, error) {
	return parse(embeddedRegistryJSON)
}

// LoadFromFile reads a registry JSON file from disk instead of using the
// embedded data. Used for --registry-file override.
func LoadFromFile(path string) (*Registry, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading registry file %q: %w", path, err)
	}
	return parse(data)
}

// parse unmarshals JSON bytes into a Registry and validates it.
func parse(data []byte) (*Registry, error) {
	var reg Registry
	if err := json.Unmarshal(data, &reg); err != nil {
		return nil, fmt.Errorf("parsing registry JSON: %w", err)
	}

	if len(reg.Models) == 0 {
		return nil, fmt.Errorf("registry has no models")
	}

	return &reg, nil
}

// FindByID returns the model with the given ID, or nil if not found.
func (r *Registry) FindByID(id string) *Model {
	for i := range r.Models {
		if r.Models[i].ID == id {
			return &r.Models[i]
		}
	}
	return nil
}

// FindByNameAndQuant searches for a model matching both name and quantization.
// The comparison is case-insensitive for convenience.
func (r *Registry) FindByNameAndQuant(modelName, quantization string) *Model {
	nameLower := strings.ToLower(modelName)
	quantLower := strings.ToLower(quantization)

	for i := range r.Models {
		if strings.ToLower(r.Models[i].ModelName) == nameLower &&
			strings.ToLower(r.Models[i].Quantization) == quantLower {
			return &r.Models[i]
		}
	}
	return nil
}

// FindByName returns all quantization variants for a given model name.
// The comparison is case-insensitive.
func (r *Registry) FindByName(modelName string) []Model {
	nameLower := strings.ToLower(modelName)
	var results []Model
	for _, m := range r.Models {
		if strings.ToLower(m.ModelName) == nameLower {
			results = append(results, m)
		}
	}
	return results
}

// FindByVRAM returns all models that fit within the given VRAM budget (in GB).
func (r *Registry) FindByVRAM(maxVRAMGB float64) []Model {
	var results []Model
	for _, m := range r.Models {
		if m.VRAMRequirementsGB <= maxVRAMGB {
			results = append(results, m)
		}
	}
	return results
}

// FindByCapability returns all models that have a specific capability.
func (r *Registry) FindByCapability(cap string) []Model {
	capLower := strings.ToLower(cap)
	var results []Model
	for _, m := range r.Models {
		for _, c := range m.Capabilities {
			if strings.ToLower(c) == capLower {
				results = append(results, m)
				break
			}
		}
	}
	return results
}

// splitPattern matches GGUF split filenames like "Name-00001-of-00006.gguf".
var splitPattern = regexp.MustCompile(`^(.*)-(\d{5})-of-(\d{5})(\.gguf)$`)

// IsSplit reports whether this model is a multi-file (sharded) GGUF model.
// Split models have filenames like "Model-Q4_K_S-00001-of-00006.gguf".
func (m *Model) IsSplit() bool {
	base := m.bareFileName()
	return splitPattern.MatchString(base)
}

// ShardCount returns the total number of shards for a split model.
// Returns 1 for single-file models.
func (m *Model) ShardCount() int {
	base := m.bareFileName()
	matches := splitPattern.FindStringSubmatch(base)
	if matches == nil {
		return 1
	}
	n, err := strconv.Atoi(matches[3])
	if err != nil {
		return 1
	}
	return n
}

// ShardFileNames returns all shard filenames for a split model.
// For single-file models, it returns a slice with just the original filename.
// The returned filenames include the subdirectory prefix if present in FileName
// (e.g., "Q4_K_S/Model-Q4_K_S-00001-of-00006.gguf").
func (m *Model) ShardFileNames() []string {
	if !m.IsSplit() {
		return []string{m.FileName}
	}

	base := m.bareFileName()
	matches := splitPattern.FindStringSubmatch(base)
	if matches == nil {
		return []string{m.FileName}
	}

	prefix := matches[1]  // e.g., "Qwen3-Coder-480B-A35B-Instruct-Q4_K_S"
	total, _ := strconv.Atoi(matches[3])
	ext := matches[4]     // ".gguf"

	// Preserve subdirectory prefix (e.g., "Q4_K_S/")
	dir := m.subDir()

	shards := make([]string, total)
	for i := 0; i < total; i++ {
		shard := fmt.Sprintf("%s-%05d-of-%05d%s", prefix, i+1, total, ext)
		if dir != "" {
			shard = dir + "/" + shard
		}
		shards[i] = shard
	}
	return shards
}

// bareFileName returns just the filename without any subdirectory prefix.
func (m *Model) bareFileName() string {
	parts := strings.Split(m.FileName, "/")
	return parts[len(parts)-1]
}

// subDir returns the subdirectory prefix from FileName, or "" if none.
// e.g., "Q4_K_S/Model.gguf" → "Q4_K_S", "Model.gguf" → ""
func (m *Model) subDir() string {
	idx := strings.LastIndex(m.FileName, "/")
	if idx < 0 {
		return ""
	}
	return m.FileName[:idx]
}

// UniqueModelNames returns a deduplicated list of model names in the registry.
func (r *Registry) UniqueModelNames() []string {
	seen := make(map[string]bool)
	var names []string
	for _, m := range r.Models {
		if !seen[m.ModelName] {
			seen[m.ModelName] = true
			names = append(names, m.ModelName)
		}
	}
	return names
}
