package agentworker

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

func TestLocalToolNames(t *testing.T) {
	got := localToolNames()
	want := []string{"Read", "Write", "Edit", "Bash", "useSkill", "saveMemory"}

	if len(got) != len(want) {
		t.Fatalf("localToolNames() returned %d tools, want %d", len(got), len(want))
	}

	for i, name := range want {
		if got[i] != name {
			t.Errorf("localToolNames()[%d] = %q, want %q", i, got[i], name)
		}
	}
}

func TestSoulDigest_NoFile(t *testing.T) {
	dir := t.TempDir()
	w := &Worker{cfg: Config{Dir: dir}}

	got := w.soulDigest()
	if got != "" {
		t.Errorf("soulDigest() with no SOUL.md = %q, want empty string", got)
	}
}

func TestSoulDigest_WithFile(t *testing.T) {
	dir := t.TempDir()
	soulPath := filepath.Join(dir, "SOUL.md")
	
	content := "# Agent Soul\nThis is the agent's identity."
	if err := os.WriteFile(soulPath, []byte(content), 0644); err != nil {
		t.Fatalf("failed to write SOUL.md: %v", err)
	}

	w := &Worker{cfg: Config{Dir: dir}}
	got := w.soulDigest()

	if got == "" {
		t.Fatal("soulDigest() with SOUL.md returned empty string")
	}

	// Verify format: "mtime:X,size:Y"
	var mtime, size int64
	n, err := fmt.Sscanf(got, "mtime:%d,size:%d", &mtime, &size)
	if err != nil || n != 2 {
		t.Errorf("soulDigest() = %q, want format mtime:X,size:Y", got)
	}

	if size != int64(len(content)) {
		t.Errorf("soulDigest() size = %d, want %d", size, len(content))
	}

	if mtime <= 0 {
		t.Errorf("soulDigest() mtime = %d, want positive value", mtime)
	}
}

func TestBackoffReset(t *testing.T) {
	// This test documents the backoff reset behavior. Backoff is reset when:
	// 1. connectAndRun calls onConnected after hello_ack (connection established)
	// 2. connectAndRun returns nil (clean shutdown)
	//
	// That ensures a disconnect after a successful session does not inherit
	// stale delay from earlier dial failures. See worker.go Run() and connectAndRun().
	t.Skip("backoff reset is verified by code inspection in worker.go")
}
