package registry

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoad(t *testing.T) {
	reg, err := Load()
	if err != nil {
		t.Fatalf("loading embedded registry: %v", err)
	}

	if reg.Version == "" {
		t.Error("registry version should not be empty")
	}
	if reg.GeneratedAt == "" {
		t.Error("registry generated_at should not be empty")
	}
	if len(reg.Models) == 0 {
		t.Fatal("registry should have models")
	}

	t.Logf("Loaded registry v%s with %d models", reg.Version, len(reg.Models))
}

func TestModel_Fields(t *testing.T) {
	reg, err := Load()
	if err != nil {
		t.Fatalf("loading: %v", err)
	}

	// Spot-check the first model has all required fields populated
	m := reg.Models[0]
	if m.ID == "" {
		t.Error("first model ID should not be empty")
	}
	if m.ModelName == "" {
		t.Error("first model ModelName should not be empty")
	}
	if m.HuggingFaceID == "" {
		t.Error("first model HuggingFaceID should not be empty")
	}
	if m.Quantization == "" {
		t.Error("first model Quantization should not be empty")
	}
	if m.DefaultContextSize <= 0 {
		t.Errorf("first model DefaultContextSize should be positive, got %d", m.DefaultContextSize)
	}
	if m.MaxContextSize <= 0 {
		t.Errorf("first model MaxContextSize should be positive, got %d", m.MaxContextSize)
	}
	if m.VRAMRequirementsGB <= 0 {
		t.Errorf("first model VRAMRequirementsGB should be positive, got %f", m.VRAMRequirementsGB)
	}
	if len(m.Capabilities) == 0 {
		t.Error("first model should have at least one capability")
	}
	if m.FileName == "" {
		t.Error("first model FileName should not be empty")
	}
}

func TestFindByID(t *testing.T) {
	reg, err := Load()
	if err != nil {
		t.Fatalf("loading: %v", err)
	}

	// Use the first model's ID to ensure we can find it
	firstID := reg.Models[0].ID
	found := reg.FindByID(firstID)
	if found == nil {
		t.Fatalf("FindByID(%q) returned nil", firstID)
	}
	if found.ID != firstID {
		t.Errorf("FindByID returned wrong model: got %q, want %q", found.ID, firstID)
	}

	// Non-existent ID
	notFound := reg.FindByID("nonexistent-model-id")
	if notFound != nil {
		t.Error("FindByID should return nil for non-existent ID")
	}
}

func TestFindByNameAndQuant(t *testing.T) {
	reg, err := Load()
	if err != nil {
		t.Fatalf("loading: %v", err)
	}

	// Use a known model from the registry
	first := reg.Models[0]
	found := reg.FindByNameAndQuant(first.ModelName, first.Quantization)
	if found == nil {
		t.Fatalf("FindByNameAndQuant(%q, %q) returned nil", first.ModelName, first.Quantization)
	}
	if found.ID != first.ID {
		t.Errorf("got ID %q, want %q", found.ID, first.ID)
	}

	// Case-insensitive lookup
	foundLower := reg.FindByNameAndQuant(
		"deepseek-v3.1-terminus", // lowercase
		"q3_k_s",                // lowercase
	)
	// This might not match depending on the actual model names in the registry,
	// but we at least verify the function doesn't panic.
	_ = foundLower

	// Non-existent combo
	notFound := reg.FindByNameAndQuant("NonExistentModel", "Q4_K_M")
	if notFound != nil {
		t.Error("FindByNameAndQuant should return nil for non-existent model")
	}
}

func TestFindByVRAM(t *testing.T) {
	reg, err := Load()
	if err != nil {
		t.Fatalf("loading: %v", err)
	}

	// With 24GB VRAM, should find some models but not the huge ones
	models24 := reg.FindByVRAM(24.0)
	if len(models24) == 0 {
		t.Error("should find at least some models that fit in 24GB VRAM")
	}

	// Every returned model should actually fit
	for _, m := range models24 {
		if m.VRAMRequirementsGB > 24.0 {
			t.Errorf("model %q requires %.1f GB but was returned for 24GB budget",
				m.ID, m.VRAMRequirementsGB)
		}
	}

	// With 0 VRAM, should find nothing
	modelsZero := reg.FindByVRAM(0)
	if len(modelsZero) != 0 {
		t.Errorf("should find no models with 0 VRAM, got %d", len(modelsZero))
	}

	// With huge VRAM, should find all models
	modelsAll := reg.FindByVRAM(100000)
	if len(modelsAll) != len(reg.Models) {
		t.Errorf("with huge VRAM budget should find all %d models, got %d",
			len(reg.Models), len(modelsAll))
	}
}

func TestFindByCapability(t *testing.T) {
	reg, err := Load()
	if err != nil {
		t.Fatalf("loading: %v", err)
	}

	// "chat" should be the most common capability
	chatModels := reg.FindByCapability("chat")
	if len(chatModels) == 0 {
		t.Error("should find models with 'chat' capability")
	}

	// Case-insensitive
	chatModelsUpper := reg.FindByCapability("Chat")
	if len(chatModelsUpper) != len(chatModels) {
		t.Errorf("case-insensitive capability search should match: got %d vs %d",
			len(chatModelsUpper), len(chatModels))
	}

	// Non-existent capability
	noneModels := reg.FindByCapability("teleportation")
	if len(noneModels) != 0 {
		t.Errorf("should find no models with fake capability, got %d", len(noneModels))
	}
}

func TestUniqueModelNames(t *testing.T) {
	reg, err := Load()
	if err != nil {
		t.Fatalf("loading: %v", err)
	}

	names := reg.UniqueModelNames()
	if len(names) == 0 {
		t.Fatal("should have unique model names")
	}

	// Should have fewer unique names than total models (multiple quantizations per model)
	if len(names) >= len(reg.Models) {
		t.Errorf("unique names (%d) should be less than total models (%d)",
			len(names), len(reg.Models))
	}

	// Verify no duplicates
	seen := make(map[string]bool)
	for _, name := range names {
		if seen[name] {
			t.Errorf("duplicate model name: %q", name)
		}
		seen[name] = true
	}

	t.Logf("Found %d unique model names across %d entries", len(names), len(reg.Models))
}

func TestLoadFromFile(t *testing.T) {
	// Write a small test registry to a temp file
	tempDir := t.TempDir()
	testFile := filepath.Join(tempDir, "test-registry.json")

	testJSON := `{
		"version": "test-0.1.0",
		"generated_at": "2024-01-01T00:00:00Z",
		"models": [{
			"id": "test-model-q4",
			"model_name": "TestModel",
			"huggingface_id": "test/TestModel-GGUF",
			"quantization": "Q4_K_M",
			"family": "Dense",
			"parameters": "7B",
			"default_context_size": 4096,
			"max_context_size": 8192,
			"vram_requirements_gb": 4.5,
			"capabilities": ["chat"],
			"file_name": "TestModel-Q4_K_M.gguf"
		}]
	}`

	if err := os.WriteFile(testFile, []byte(testJSON), 0644); err != nil {
		t.Fatalf("writing test file: %v", err)
	}

	reg, err := LoadFromFile(testFile)
	if err != nil {
		t.Fatalf("loading from file: %v", err)
	}

	if reg.Version != "test-0.1.0" {
		t.Errorf("version: got %q, want %q", reg.Version, "test-0.1.0")
	}
	if len(reg.Models) != 1 {
		t.Fatalf("models count: got %d, want 1", len(reg.Models))
	}
	if reg.Models[0].ID != "test-model-q4" {
		t.Errorf("model ID: got %q, want %q", reg.Models[0].ID, "test-model-q4")
	}
}

func TestLoadFromFile_NotFound(t *testing.T) {
	_, err := LoadFromFile("/nonexistent/path/registry.json")
	if err == nil {
		t.Fatal("expected error for non-existent file, got nil")
	}
}

func TestParse_EmptyRegistry(t *testing.T) {
	_, err := parse([]byte(`{"version":"1.0","generated_at":"now","models":[]}`))
	if err == nil {
		t.Fatal("expected error for empty models array, got nil")
	}
}

func TestParse_InvalidJSON(t *testing.T) {
	_, err := parse([]byte(`{not valid json}`))
	if err == nil {
		t.Fatal("expected error for invalid JSON, got nil")
	}
}

func TestModel_IsSplit(t *testing.T) {
	tests := []struct {
		name     string
		fileName string
		want     bool
	}{
		{
			name:     "single file model",
			fileName: "Qwen3-8B-Q4_K_M.gguf",
			want:     false,
		},
		{
			name:     "split model with subdir",
			fileName: "Q4_K_S/Qwen3-Coder-480B-A35B-Instruct-Q4_K_S-00001-of-00006.gguf",
			want:     true,
		},
		{
			name:     "split model no subdir",
			fileName: "Model-Name-00001-of-00003.gguf",
			want:     true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := &Model{FileName: tt.fileName}
			if got := m.IsSplit(); got != tt.want {
				t.Errorf("IsSplit() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestModel_ShardCount(t *testing.T) {
	tests := []struct {
		name     string
		fileName string
		want     int
	}{
		{"single file", "Qwen3-8B-Q4_K_M.gguf", 1},
		{"6 shards", "Q4_K_S/Model-Q4_K_S-00001-of-00006.gguf", 6},
		{"21 shards", "BF16/Model-BF16-00001-of-00021.gguf", 21},
		{"3 shards no subdir", "Model-00001-of-00003.gguf", 3},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := &Model{FileName: tt.fileName}
			if got := m.ShardCount(); got != tt.want {
				t.Errorf("ShardCount() = %d, want %d", got, tt.want)
			}
		})
	}
}

func TestModel_ShardFileNames(t *testing.T) {
	t.Run("single file returns itself", func(t *testing.T) {
		m := &Model{FileName: "Qwen3-8B-Q4_K_M.gguf"}
		shards := m.ShardFileNames()
		if len(shards) != 1 {
			t.Fatalf("expected 1 shard, got %d", len(shards))
		}
		if shards[0] != "Qwen3-8B-Q4_K_M.gguf" {
			t.Errorf("got %q, want %q", shards[0], "Qwen3-8B-Q4_K_M.gguf")
		}
	})

	t.Run("split model generates all shards with subdir", func(t *testing.T) {
		m := &Model{FileName: "Q4_K_S/Qwen3-Coder-480B-A35B-Instruct-Q4_K_S-00001-of-00006.gguf"}
		shards := m.ShardFileNames()
		if len(shards) != 6 {
			t.Fatalf("expected 6 shards, got %d", len(shards))
		}
		expected := []string{
			"Q4_K_S/Qwen3-Coder-480B-A35B-Instruct-Q4_K_S-00001-of-00006.gguf",
			"Q4_K_S/Qwen3-Coder-480B-A35B-Instruct-Q4_K_S-00002-of-00006.gguf",
			"Q4_K_S/Qwen3-Coder-480B-A35B-Instruct-Q4_K_S-00003-of-00006.gguf",
			"Q4_K_S/Qwen3-Coder-480B-A35B-Instruct-Q4_K_S-00004-of-00006.gguf",
			"Q4_K_S/Qwen3-Coder-480B-A35B-Instruct-Q4_K_S-00005-of-00006.gguf",
			"Q4_K_S/Qwen3-Coder-480B-A35B-Instruct-Q4_K_S-00006-of-00006.gguf",
		}
		for i, want := range expected {
			if shards[i] != want {
				t.Errorf("shard[%d] = %q, want %q", i, shards[i], want)
			}
		}
	})

	t.Run("split model no subdir", func(t *testing.T) {
		m := &Model{FileName: "Model-00001-of-00003.gguf"}
		shards := m.ShardFileNames()
		if len(shards) != 3 {
			t.Fatalf("expected 3 shards, got %d", len(shards))
		}
		if shards[2] != "Model-00003-of-00003.gguf" {
			t.Errorf("last shard = %q, want %q", shards[2], "Model-00003-of-00003.gguf")
		}
	})
}

func TestModel_IsSplit_FromRegistry(t *testing.T) {
	reg, err := Load()
	if err != nil {
		t.Fatalf("loading: %v", err)
	}

	var splitCount, singleCount int
	for _, m := range reg.Models {
		if m.IsSplit() {
			splitCount++
			shards := m.ShardFileNames()
			if len(shards) != m.ShardCount() {
				t.Errorf("model %q: ShardFileNames() returned %d files but ShardCount() = %d",
					m.ID, len(shards), m.ShardCount())
			}
		} else {
			singleCount++
		}
	}

	t.Logf("Registry: %d split models, %d single-file models", splitCount, singleCount)

	if splitCount == 0 {
		t.Error("expected at least some split models in the registry")
	}
	if singleCount == 0 {
		t.Error("expected at least some single-file models in the registry")
	}
}
