package codingtools

import (
	"context"
	"encoding/json"
	"os/user"
	"path/filepath"
	"strings"
	"testing"
)

func TestProcessUsernameFallsBackToOSUser(t *testing.T) {
	cur, err := user.Current()
	if err != nil {
		t.Skip("user.Current:", err)
	}
	t.Setenv("USER", "")
	t.Setenv("USERNAME", "")
	got := processUsernameForBash()
	if got != cur.Username {
		t.Fatalf("processUsernameForBash() = %q, want %q (user.Current.Username)", got, cur.Username)
	}
}

func TestBashSubprocessEnvMinimalKeys(t *testing.T) {
	wd := t.TempDir()
	absWD, err := filepath.Abs(wd)
	if err != nil {
		t.Fatal(err)
	}
	absWD = filepath.Clean(absWD)

	env := bashSubprocessEnv(wd, nil)
	m := parseEnvSlice(env)

	for _, key := range []string{"HOME", "PATH", "PWD", "USER", "TERM"} {
		if _, ok := m[key]; !ok {
			t.Fatalf("missing required key %q in %v", key, env)
		}
	}
	if len(m) != 5 {
		t.Fatalf("want exactly 5 keys by default, got %d: %v", len(m), m)
	}
	if m["PWD"] != absWD {
		t.Fatalf("PWD = %q, want %q", m["PWD"], absWD)
	}
}

func TestBashSubprocessEnvMergeExtra(t *testing.T) {
	wd := t.TempDir()
	env := bashSubprocessEnv(wd, map[string]string{"GITHUB_TOKEN": "x", "USER": "override"})
	m := parseEnvSlice(env)
	if m["GITHUB_TOKEN"] != "x" {
		t.Fatalf("GITHUB_TOKEN = %q", m["GITHUB_TOKEN"])
	}
	if m["USER"] != "override" {
		t.Fatalf("extra should override USER, got %q", m["USER"])
	}
}

func TestExecuteBashDoesNotInheritParentEnv(t *testing.T) {
	t.Setenv("AUXOT_BASH_ENV_LEAK_TEST", "should-not-see")
	wd := t.TempDir()
	raw, _ := json.Marshal(map[string]string{
		"command": "if [ -n \"$AUXOT_BASH_ENV_LEAK_TEST\" ]; then echo LEAK; fi",
	})
	out, err := executeBash(context.Background(), wd, nil, raw)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out, "LEAK") {
		t.Fatalf("child inherited parent-only env, output: %q", out)
	}
}

func TestExecuteBashSeesToolEnv(t *testing.T) {
	wd := t.TempDir()
	raw, _ := json.Marshal(map[string]string{"command": "echo -n \"$JOB_SECRET\""})
	out, err := executeBash(context.Background(), wd, map[string]string{"JOB_SECRET": "xyzzy"}, raw)
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(out) != "xyzzy" {
		t.Fatalf("want xyzzy, got %q", out)
	}
}

func TestExecuteBashInlineTruncation_LargeOutput(t *testing.T) {
	wd := t.TempDir()
	// Command emits slightly more than 1 KB (10 KB of 'A's).
	raw, _ := json.Marshal(map[string]string{
		"command": "python3 -c \"print('A' * 10240, end='')\"",
	})
	out, err := executeBash(context.Background(), wd, nil, raw)
	if err != nil {
		t.Fatal(err)
	}
	if len(out) > bashInlineBytes+200 {
		t.Fatalf("inline output too large: %d bytes (want ≤ ~%d)", len(out), bashInlineBytes+200)
	}
	if !strings.Contains(out, "output truncated") {
		t.Fatalf("expected truncation header, got: %q", out[:min(len(out), 200)])
	}
	// The tail must end with 'A's (the last chars of the 10 KB run).
	if !strings.HasSuffix(strings.TrimRight(out, "\n"), "AAAA") {
		t.Fatalf("expected tail to end with 'A's, got suffix: %q", out[max(0, len(out)-20):])
	}
}

func TestExecuteBashInlineTruncation_SmallOutput(t *testing.T) {
	wd := t.TempDir()
	// Command emits 500 bytes — fits within bashInlineBytes, returned as-is.
	raw, _ := json.Marshal(map[string]string{
		"command": "python3 -c \"print('B' * 499, end='')\"",
	})
	out, err := executeBash(context.Background(), wd, nil, raw)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out, "output truncated") {
		t.Fatalf("unexpected truncation header for small output: %q", out[:min(len(out), 200)])
	}
	if !strings.Contains(out, "BBBB") {
		t.Fatalf("expected raw output, got: %q", out[:min(len(out), 200)])
	}
}

func TestExecuteBashInlineTruncation_UsesCallID(t *testing.T) {
	wd := t.TempDir()
	raw, _ := json.Marshal(map[string]string{
		"command": "python3 -c \"print('C' * 10240, end='')\"",
	})
	toolEnv := map[string]string{"_call_id": "call-abc-123"}
	out, err := executeBash(context.Background(), wd, toolEnv, raw)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "call-abc-123") {
		t.Fatalf("expected call ID in truncation header, got: %q", out[:min(len(out), 300)])
	}
}

func parseEnvSlice(env []string) map[string]string {
	m := make(map[string]string, len(env))
	for _, kv := range env {
		k, v, ok := strings.Cut(kv, "=")
		if !ok {
			continue
		}
		m[k] = v
	}
	return m
}
