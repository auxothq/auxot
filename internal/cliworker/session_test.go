package cliworker

import (
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestClaudeSessionIDForCompactionKey(t *testing.T) {
	t.Run("empty", func(t *testing.T) {
		if got := claudeSessionIDForCompactionKey(""); got != "" {
			t.Fatalf("got %q, want empty", got)
		}
	})
	t.Run("numeric DB id maps to stable UUID", func(t *testing.T) {
		got := claudeSessionIDForCompactionKey("10218")
		if _, err := uuid.Parse(got); err != nil {
			t.Fatalf("not a UUID: %q: %v", got, err)
		}
		again := claudeSessionIDForCompactionKey("10218")
		if got != again {
			t.Fatalf("not stable: %q vs %q", got, again)
		}
	})
	t.Run("valid UUID passes through", func(t *testing.T) {
		u := "550e8400-e29b-41d4-a716-446655440000"
		if got := claudeSessionIDForCompactionKey(u); !strings.EqualFold(got, u) {
			t.Fatalf("got %q, want %q", got, u)
		}
	})
}

// TestSessionFilePath verifies the path construction including symlink resolution.
func TestSessionFilePath(t *testing.T) {
	t.Run("basic path construction", func(t *testing.T) {
		got := sessionFilePath("/cfg", "/work", "abc-123")
		// On Linux /work has no symlink so path is direct.
		// On macOS /tmp resolves, but /work is synthetic here.
		want := filepath.Join("/cfg", "projects", "-work", "abc-123.jsonl")
		if got != want {
			// Accept the macOS-resolved form too — the important thing is
			// that the session ID and .jsonl suffix are present.
			if filepath.Base(got) != "abc-123.jsonl" {
				t.Errorf("sessionFilePath base = %q, want abc-123.jsonl", filepath.Base(got))
			}
		}
	})

	t.Run("slashes in workDir are replaced with dashes", func(t *testing.T) {
		p := sessionFilePath("/cfg", "/a/b/c", "sid")
		dir := filepath.Dir(p)
		base := filepath.Base(dir)
		// The escaped CWD "-a-b-c" (leading slash also becomes dash).
		if base != "-a-b-c" {
			t.Errorf("escaped CWD dir = %q, want \"-a-b-c\"", base)
		}
	})
}

// TestCleanupOldSessionFiles verifies that stale session files are removed
// while the active session file is preserved.
func TestCleanupOldSessionFiles(t *testing.T) {
	tmpCfg := t.TempDir()
	workDir := t.TempDir()
	log := slog.Default()

	// Derive the project directory using the same logic as cleanupOldSessionFiles
	// (which calls EvalSymlinks on workDir internally).
	projectDir := filepath.Dir(sessionFilePath(tmpCfg, workDir, "probe"))
	if err := os.MkdirAll(projectDir, 0o755); err != nil {
		t.Fatal(err)
	}

	currentSession := "current-session-id"
	staleSession1 := "stale-session-1"
	staleSession2 := "stale-session-2"

	for _, name := range []string{currentSession, staleSession1, staleSession2} {
		if err := os.WriteFile(filepath.Join(projectDir, name+".jsonl"), []byte("data"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	cleanupOldSessionFiles(tmpCfg, workDir, currentSession, log)

	if _, err := os.Stat(filepath.Join(projectDir, currentSession+".jsonl")); err != nil {
		t.Errorf("current session file should not have been removed: %v", err)
	}
	for _, stale := range []string{staleSession1, staleSession2} {
		if _, err := os.Stat(filepath.Join(projectDir, stale+".jsonl")); err == nil {
			t.Errorf("stale session file %q should have been removed", stale)
		}
	}
}

// TestCleanupStaleSessionsByAge verifies that CleanupStaleSessions removes files
// older than maxAge and keeps recent files intact.
func TestCleanupStaleSessionsByAge(t *testing.T) {
	tmpCfg := t.TempDir()
	workDir := t.TempDir()

	projectDir := filepath.Dir(sessionFilePath(tmpCfg, workDir, "probe"))
	if err := os.MkdirAll(projectDir, 0o755); err != nil {
		t.Fatal(err)
	}

	staleFile := filepath.Join(projectDir, "old-session.jsonl")
	if err := os.WriteFile(staleFile, []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}
	pastTime := time.Now().Add(-2 * time.Hour)
	if err := os.Chtimes(staleFile, pastTime, pastTime); err != nil {
		t.Fatal(err)
	}

	freshFile := filepath.Join(projectDir, "new-session.jsonl")
	if err := os.WriteFile(freshFile, []byte("new"), 0o644); err != nil {
		t.Fatal(err)
	}

	CleanupStaleSessions(tmpCfg, workDir, 1*time.Hour)

	if _, err := os.Stat(staleFile); err == nil {
		t.Error("stale session file should have been removed")
	}
	if _, err := os.Stat(freshFile); err != nil {
		t.Errorf("fresh session file should not have been removed: %v", err)
	}
}
