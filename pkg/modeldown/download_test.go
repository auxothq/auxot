package modeldown

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
)

func TestEnsureLocalModel_SingleFile(t *testing.T) {
	dir := t.TempDir()
	modelPath := filepath.Join(dir, "model-Q4_K_M.gguf")

	// Create the file
	if err := os.WriteFile(modelPath, []byte("fake gguf"), 0o644); err != nil {
		t.Fatal(err)
	}

	got, err := ensureLocalModel(modelPath, testLogger(t))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != modelPath {
		t.Errorf("got %q, want %q", got, modelPath)
	}
}

func TestEnsureLocalModel_SingleFile_NotFound(t *testing.T) {
	_, err := ensureLocalModel("/nonexistent/model.gguf", testLogger(t))
	if err == nil {
		t.Fatal("expected error for missing file")
	}
}

func TestEnsureLocalModel_SplitModel_AllPresent(t *testing.T) {
	dir := t.TempDir()

	// Create 3 shard files
	for i := 1; i <= 3; i++ {
		name := fmt.Sprintf("Model-Q4_K_S-%05d-of-00003.gguf", i)
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte("shard data"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	// Point to the first shard
	firstShard := filepath.Join(dir, "Model-Q4_K_S-00001-of-00003.gguf")
	got, err := ensureLocalModel(firstShard, testLogger(t))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != firstShard {
		t.Errorf("got %q, want %q", got, firstShard)
	}
}

func TestEnsureLocalModel_SplitModel_MissingShards(t *testing.T) {
	dir := t.TempDir()

	// Create only shards 1 and 3 (missing shard 2)
	for _, i := range []int{1, 3} {
		name := fmt.Sprintf("Model-Q4_K_S-%05d-of-00003.gguf", i)
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte("shard data"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	firstShard := filepath.Join(dir, "Model-Q4_K_S-00001-of-00003.gguf")
	_, err := ensureLocalModel(firstShard, testLogger(t))
	if err == nil {
		t.Fatal("expected error for missing shard")
	}

	// Error should mention the missing shard
	if got := err.Error(); !contains(got, "00002-of-00003") {
		t.Errorf("error should mention missing shard 2, got: %s", got)
	}
	if got := err.Error(); !contains(got, "1 of 3 shards missing") {
		t.Errorf("error should mention count, got: %s", got)
	}
}

func TestEnsureLocalModel_SplitModel_6Shards(t *testing.T) {
	dir := t.TempDir()

	// Create all 6 shards like the real 480B model
	for i := 1; i <= 6; i++ {
		name := fmt.Sprintf("Qwen3-Coder-480B-A35B-Instruct-Q4_K_S-%05d-of-00006.gguf", i)
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte("shard"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	firstShard := filepath.Join(dir, "Qwen3-Coder-480B-A35B-Instruct-Q4_K_S-00001-of-00006.gguf")
	got, err := ensureLocalModel(firstShard, testLogger(t))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != firstShard {
		t.Errorf("got %q, want %q", got, firstShard)
	}
}

func contains(s, substr string) bool {
	return len(s) >= len(substr) && searchString(s, substr)
}

func searchString(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}

// testLogger returns a slog.Logger that writes to t.Log.
func testLogger(t *testing.T) *slog.Logger {
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug}))
}
