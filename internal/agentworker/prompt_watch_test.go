package agentworker

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/fsnotify/fsnotify"
)

func TestIsPromptSourceRel(t *testing.T) {
	cases := []struct {
		rel  string
		want bool
	}{
		{"SOUL.md", true},
		{"AGENTS.md", true},
		{"RULES.md", true},
		{"agent.yaml", true},
		{"memory/MEMORY.md", true},
		{"memory/context.md", true},
		{"knowledge/index.yaml", true},
		{"hooks/bootstrap.md", true},
		{"skills/foo/SKILL.md", true},
		{"skills/foo/bar/SKILL.md", false},
		{"README.md", false},
		{"memory/notes.md", false},
	}
	for _, tc := range cases {
		if got := isPromptSourceRel(tc.rel); got != tc.want {
			t.Errorf("isPromptSourceRel(%q) = %v, want %v", tc.rel, got, tc.want)
		}
	}
}

func TestPromptWatchEventRelevant(t *testing.T) {
	root := t.TempDir()
	soul := filepath.Join(root, "SOUL.md")
	if err := os.WriteFile(soul, []byte("x"), 0644); err != nil {
		t.Fatal(err)
	}

	ev := fsnotify.Event{Name: soul, Op: fsnotify.Write}
	if !promptWatchEventRelevant(root, ev) {
		t.Error("Write on SOUL.md should be relevant")
	}

	evChmod := fsnotify.Event{Name: soul, Op: fsnotify.Chmod}
	if promptWatchEventRelevant(root, evChmod) {
		t.Error("Chmod-only should be ignored")
	}

	evOther := fsnotify.Event{Name: filepath.Join(root, "other.txt"), Op: fsnotify.Write}
	if promptWatchEventRelevant(root, evOther) {
		t.Error("unrelated file should be ignored")
	}
}
