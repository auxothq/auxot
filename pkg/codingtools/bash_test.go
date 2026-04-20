package codingtools

import (
	"context"
	"encoding/json"
	"os/user"
	"path/filepath"
	"strings"
	"testing"
)

func parseBashResult(t *testing.T, out string) bashResultJSON {
	t.Helper()
	var r bashResultJSON
	if err := json.Unmarshal([]byte(out), &r); err != nil {
		t.Fatalf("invalid bash JSON: %v\n%s", err, out)
	}
	return r
}

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
	r := parseBashResult(t, out)
	combined := r.Stdout + r.Stderr
	if strings.Contains(combined, "LEAK") {
		t.Fatalf("child inherited parent-only env, output: %q", combined)
	}
}

func TestExecuteBashSeesToolEnv(t *testing.T) {
	wd := t.TempDir()
	raw, _ := json.Marshal(map[string]string{"command": "echo -n \"$JOB_SECRET\""})
	out, err := executeBash(context.Background(), wd, map[string]string{"JOB_SECRET": "xyzzy"}, raw)
	if err != nil {
		t.Fatal(err)
	}
	r := parseBashResult(t, out)
	if r.Stdout != "xyzzy" || r.Output != "xyzzy" {
		t.Fatalf("want xyzzy in stdout/output, got stdout=%q output=%q", r.Stdout, r.Output)
	}
}

func TestExecuteBashStdoutOnly(t *testing.T) {
	wd := t.TempDir()
	raw, _ := json.Marshal(map[string]string{"command": "echo -n hello"})
	out, err := executeBash(context.Background(), wd, nil, raw)
	if err != nil {
		t.Fatal(err)
	}
	r := parseBashResult(t, out)
	if r.Stdout != "hello" || r.Output != "hello" {
		t.Fatalf("stdout/output: got %q / %q", r.Stdout, r.Output)
	}
	if r.Stderr != "" {
		t.Fatalf("stderr: want empty, got %q", r.Stderr)
	}
	if r.Status != 0 {
		t.Fatalf("status: want 0, got %d", r.Status)
	}
}

func TestExecuteBashStderrOnly(t *testing.T) {
	wd := t.TempDir()
	raw, _ := json.Marshal(map[string]string{"command": "echo -n hi >&2"})
	out, err := executeBash(context.Background(), wd, nil, raw)
	if err != nil {
		t.Fatal(err)
	}
	r := parseBashResult(t, out)
	if r.Stdout != "" || r.Output != "" {
		t.Fatalf("stdout/output: want empty, got %q / %q", r.Stdout, r.Output)
	}
	if r.Stderr != "hi" {
		t.Fatalf("stderr: got %q", r.Stderr)
	}
	if r.Status != 0 {
		t.Fatalf("status: want 0, got %d", r.Status)
	}
}

func TestExecuteBashStdoutAndStderr(t *testing.T) {
	wd := t.TempDir()
	raw, _ := json.Marshal(map[string]string{"command": "echo out; echo err >&2"})
	out, err := executeBash(context.Background(), wd, nil, raw)
	if err != nil {
		t.Fatal(err)
	}
	r := parseBashResult(t, out)
	if strings.TrimSpace(r.Stdout) != "out" || r.Output != r.Stdout {
		t.Fatalf("stdout/output: got %q / %q", r.Stdout, r.Output)
	}
	if strings.TrimSpace(r.Stderr) != "err" {
		t.Fatalf("stderr: got %q", r.Stderr)
	}
	if r.Status != 0 {
		t.Fatalf("status: want 0, got %d", r.Status)
	}
}

func TestExecuteBashNonZeroWithStderr(t *testing.T) {
	wd := t.TempDir()
	raw, _ := json.Marshal(map[string]string{"command": "echo msg >&2; false"})
	out, err := executeBash(context.Background(), wd, nil, raw)
	if err != nil {
		t.Fatal(err)
	}
	r := parseBashResult(t, out)
	if r.Stdout != "" {
		t.Fatalf("stdout: want empty, got %q", r.Stdout)
	}
	if !strings.Contains(r.Stderr, "msg") {
		t.Fatalf("stderr should contain msg: %q", r.Stderr)
	}
	if !strings.Contains(r.Stderr, "[exit:") {
		t.Fatalf("stderr should contain exit note: %q", r.Stderr)
	}
	if r.Status != 1 {
		t.Fatalf("status: want 1 for false, got %d", r.Status)
	}
}

func TestExecuteBashEmptySuccess(t *testing.T) {
	wd := t.TempDir()
	raw, _ := json.Marshal(map[string]string{"command": "true"})
	out, err := executeBash(context.Background(), wd, nil, raw)
	if err != nil {
		t.Fatal(err)
	}
	r := parseBashResult(t, out)
	if r.Stdout != "" || r.Stderr != "" || r.Output != "" {
		t.Fatalf("want empty JSON fields, got %+v", r)
	}
	if r.Status != 0 {
		t.Fatalf("status: want 0, got %d", r.Status)
	}
}

func TestExecuteBashInlineTruncation_LargeOutput(t *testing.T) {
	wd := t.TempDir()
	raw, _ := json.Marshal(map[string]string{
		"command": `python3 -u -c 'import sys; sys.stdout.write("A" * 10240)'`,
	})
	out, err := executeBash(context.Background(), wd, nil, raw)
	if err != nil {
		t.Fatal(err)
	}
	if len(out) > bashInlineBytes+200 {
		t.Fatalf("inline JSON too large: %d bytes (want ≤ ~%d)", len(out), bashInlineBytes+200)
	}
	if !json.Valid([]byte(out)) {
		t.Fatal("result must be valid JSON")
	}
	r := parseBashResult(t, out)
	hasTail := strings.HasSuffix(strings.TrimRight(r.Stdout, "\n"), "AAAA") || strings.HasSuffix(strings.TrimRight(r.Output, "\n"), "AAAA")
	hasRecall := strings.Contains(out, "tool_recall")
	if !hasTail && !hasRecall {
		t.Fatalf("expected tail of A's or tool_recall hint, got: %q", out[:min(len(out), 400)])
	}
}

func TestExecuteBashInlineTruncation_SmallOutput(t *testing.T) {
	wd := t.TempDir()
	raw, _ := json.Marshal(map[string]string{
		"command": `python3 -u -c 'import sys; sys.stdout.write("B" * 499)'`,
	})
	out, err := executeBash(context.Background(), wd, nil, raw)
	if err != nil {
		t.Fatal(err)
	}
	r := parseBashResult(t, out)
	if strings.Contains(out, "tool_recall") && len(out) < bashInlineBytes {
		t.Fatalf("unexpected tool_recall hint for small output: %q", out[:min(len(out), 200)])
	}
	if !strings.Contains(r.Stdout, "BBBB") {
		t.Fatalf("expected raw stdout content, got: %q", r.Stdout[:min(len(r.Stdout), 200)])
	}
}

func TestExecuteBashInlineTruncation_UsesCallID(t *testing.T) {
	wd := t.TempDir()
	raw, _ := json.Marshal(map[string]string{
		"command": `python3 -u -c 'import sys; sys.stdout.write("C" * 10240)'`,
	})
	toolEnv := map[string]string{"_call_id": "call-abc-123"}
	out, err := executeBash(context.Background(), wd, toolEnv, raw)
	if err != nil {
		t.Fatal(err)
	}
	// Call ID appears in tool_recall hint when inline JSON cannot fit even minimal content.
	if strings.Contains(out, "tool_recall") && !strings.Contains(out, "call-abc-123") {
		t.Fatalf("expected call ID in tool_recall hint, got: %q", out[:min(len(out), 500)])
	}
}

func TestApplyMaxBashPayloadTrimsFromStderrFirst(t *testing.T) {
	p := bashResultJSON{
		Stdout: strings.Repeat("O", 60000),
		Stderr: strings.Repeat("E", 60000),
		Status: 9,
	}
	applyMaxBashPayload(&p)
	if p.Status != 9 {
		t.Fatalf("applyMaxBashPayload must preserve status, got %d", p.Status)
	}
	if len(p.Stdout)+len(p.Stderr) > maxBashOutputBytes {
		t.Fatalf("sum too large: %d + %d = %d (max %d)", len(p.Stdout), len(p.Stderr), len(p.Stdout)+len(p.Stderr), maxBashOutputBytes)
	}
	if !strings.Contains(p.Stderr, bash100KBTruncationSuffix) {
		t.Fatalf("expected 100KB truncation note in stderr, got stderr len=%d", len(p.Stderr))
	}
	if len(p.Stdout) != 60000 {
		t.Fatalf("expected stdout unchanged when stderr absorbs overflow, got len=%d", len(p.Stdout))
	}
	if len(p.Stderr) >= 60000 {
		t.Fatalf("expected stderr shortened first, got len=%d", len(p.Stderr))
	}
}

func TestExecuteBashLargeCombinedStreamsReturnsValidJSON(t *testing.T) {
	wd := t.TempDir()
	raw, _ := json.Marshal(map[string]string{
		"command": `python3 -u -c 'import sys; sys.stdout.write("O"*60000); sys.stderr.write("E"*60000); sys.stdout.flush(); sys.stderr.flush()'`,
	})
	out, err := executeBash(context.Background(), wd, nil, raw)
	if err != nil {
		t.Fatal(err)
	}
	if !json.Valid([]byte(out)) || len(out) > bashInlineBytes+50 {
		t.Fatalf("want valid compact JSON ≤ ~inline cap, got len=%d", len(out))
	}
	r := parseBashResult(t, out)
	if r.Status != 0 {
		t.Fatalf("status: want 0, got %d", r.Status)
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
