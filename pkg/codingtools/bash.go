package codingtools

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"time"
)

const (
	defaultBashTimeoutSecs = 120
	bashInlineBytes        = 1_024      // returned inline to the LLM (marshaled JSON size)
	maxBashOutputBytes     = 100 * 1024 // len(stdout)+len(stderr) from subprocess (before JSON)
)

const bash100KBTruncationSuffix = "\n[output truncated at 100KB]"

// bashResultJSON is the compact JSON object returned to the LLM for bash runs.
type bashResultJSON struct {
	Stdout string `json:"stdout"`
	Stderr string `json:"stderr"`
	Output string `json:"output"` // always identical to Stdout (LLM ergonomics)
	Status int    `json:"status"` // process exit code; 0 success; -1 if not an exec.ExitError
}

// BashArgs are the arguments for the bash tool.
type BashArgs struct {
	Command string `json:"command"`
	Timeout int    `json:"timeout,omitempty"` // seconds, default 120
}

// BashTool returns a Tool that executes bash commands in the agent directory.
// The container itself is the security boundary — bash is not sandboxed to the
// agent directory, matching the behavior of Claude Code and Pi.
//
// The bash subprocess does not inherit the full worker environment: it receives
// only HOME, PATH, PWD, USER, and TERM, plus any optional per-job variables from
// the server (tool.execute env). USER is $USER / $USERNAME if set, otherwise
// os/user.Current (same effective user as the worker — e.g. Dockerfile USER
// when that variable is not exported).
func BashTool() Tool {
	return Tool{
		Name: "bash",
		Description: "Execute a bash command in the agent directory. " +
			"The result is a JSON object string with string fields stdout, stderr, and output " +
			"(output is always the same as stdout), and integer status (shell exit code; -1 when the failure was not a normal non-zero exit). " +
			"Combined stdout/stderr payload is capped at 100KB; inline JSON size is capped at 1KB.",
		Parameters: json.RawMessage(`{
			"type": "object",
			"properties": {
				"command": {
					"type": "string",
					"description": "The bash command to execute"
				},
				"timeout": {
					"type": "integer",
					"description": "Timeout in seconds (default: 120)"
				}
			},
			"required": ["command"]
		}`),
		Execute: func(ctx context.Context, workDir string, toolEnv map[string]string, args json.RawMessage) (string, error) {
			return executeBash(ctx, workDir, toolEnv, args)
		},
	}
}

// processUsernameForBash resolves USER for the bash child without inheriting
// the full process environment. Order: USER, USERNAME (Windows), then
// os/user.Current for the running uid (covers Docker USER with no $USER).
func processUsernameForBash() string {
	if u := os.Getenv("USER"); u != "" {
		return u
	}
	if u := os.Getenv("USERNAME"); u != "" {
		return u
	}
	if cur, err := user.Current(); err == nil && cur.Username != "" {
		return cur.Username
	}
	return ""
}

// bashSubprocessEnv builds a minimal environment for the bash child: HOME,
// PATH, PWD, USER, TERM from the worker, then merges extra (server-supplied
// credentials, etc.) which may add or override keys.
func bashSubprocessEnv(workDir string, extra map[string]string) []string {
	pwd, err := filepath.Abs(workDir)
	if err != nil {
		pwd = filepath.Clean(workDir)
	} else {
		pwd = filepath.Clean(pwd)
	}

	path := os.Getenv("PATH")
	if path == "" {
		path = "/usr/local/bin:/usr/bin:/bin"
	}

	term := os.Getenv("TERM")
	if term == "" {
		term = "dumb"
	}

	user := processUsernameForBash()

	env := map[string]string{
		"HOME": os.Getenv("HOME"),
		"PATH": path,
		"PWD":  pwd,
		"USER": user,
		"TERM": term,
	}

	for k, v := range extra {
		if k != "" {
			env[k] = v
		}
	}

	out := make([]string, 0, len(env))
	for k, v := range env {
		out = append(out, k+"="+v)
	}
	return out
}

func executeBash(ctx context.Context, workDir string, toolEnv map[string]string, rawArgs json.RawMessage) (string, error) {
	var args BashArgs
	if err := json.Unmarshal(rawArgs, &args); err != nil {
		return "", fmt.Errorf("invalid bash args: %w", err)
	}
	if args.Command == "" {
		return "", fmt.Errorf("command is required")
	}

	timeout := defaultBashTimeoutSecs
	if args.Timeout > 0 {
		timeout = args.Timeout
	}

	cmdCtx, cancel := context.WithTimeout(ctx, time.Duration(timeout)*time.Second)
	defer cancel()

	cmd := exec.CommandContext(cmdCtx, "bash", "-c", args.Command)
	cmd.Dir = workDir
	cmd.Env = bashSubprocessEnv(workDir, toolEnv)

	var stdoutBuf, stderrBuf bytes.Buffer
	cmd.Stdout = &stdoutBuf
	cmd.Stderr = &stderrBuf

	// Run and capture exit code — don't return an error for non-zero exits when
	// there is output, as the model needs to see streams to diagnose.
	runErr := cmd.Run()
	status := 0
	if runErr != nil {
		if stdoutBuf.Len() == 0 && stderrBuf.Len() == 0 {
			return "", fmt.Errorf("bash: %w", runErr)
		}
		var exitErr *exec.ExitError
		if errors.As(runErr, &exitErr) {
			status = exitErr.ExitCode()
		} else {
			status = -1
		}
		fmt.Fprintf(&stderrBuf, "\n[exit: %v]", runErr)
	}

	p := bashResultJSON{
		Stdout: stdoutBuf.String(),
		Stderr: stderrBuf.String(),
		Status: status,
	}
	p.Output = p.Stdout

	applyMaxBashPayload(&p)
	return marshalBashResultInline(p, toolEnv), nil
}

// applyMaxBashPayload ensures len(stdout)+len(stderr) <= maxBashOutputBytes in the
// final JSON string fields: trim from the end of stderr first, then stdout, then
// append one shared truncation note to stderr (included in the byte budget).
func applyMaxBashPayload(p *bashResultJSON) {
	total := len(p.Stdout) + len(p.Stderr)
	if total <= maxBashOutputBytes {
		p.Output = p.Stdout
		return
	}
	suffix := bash100KBTruncationSuffix
	contentBudget := maxBashOutputBytes - len(suffix)
	if contentBudget < 0 {
		contentBudget = 0
	}
	over := total - contentBudget
	// Trim stderr from the end first.
	if over > 0 && len(p.Stderr) > 0 {
		if over >= len(p.Stderr) {
			over -= len(p.Stderr)
			p.Stderr = ""
		} else {
			p.Stderr = p.Stderr[:len(p.Stderr)-over]
			over = 0
		}
	}
	if over > 0 && len(p.Stdout) > 0 {
		if over >= len(p.Stdout) {
			p.Stdout = ""
		} else {
			p.Stdout = p.Stdout[:len(p.Stdout)-over]
		}
	}
	p.Stderr += suffix
	p.Output = p.Stdout
}

func marshalBashResultInline(p bashResultJSON, toolEnv map[string]string) string {
	p.Output = p.Stdout
	b, err := json.Marshal(p)
	if err != nil {
		return minimalBashInlineJSON(toolEnv, p.Status)
	}
	if len(b) <= bashInlineBytes {
		return string(b)
	}
	// Fit JSON into bashInlineBytes: trim from the end of stderr first (longest
	// stderr prefix), then stdout (longest stdout prefix), using binary search
	// so we do not wipe content in one step (JSON size is not linear in string length).
	so, se := p.Stdout, p.Stderr
	// Largest stderr prefix (trim suffix first) that still allows some stdout.
	bestKE := 0
	lo, hi := 0, len(se)
	for lo <= hi {
		mid := (lo + hi + 1) / 2
		p2 := bashResultJSON{Stdout: so, Stderr: se[:mid], Output: so, Status: p.Status}
		b2, err2 := json.Marshal(p2)
		if err2 != nil {
			hi = mid - 1
			continue
		}
		if len(b2) <= bashInlineBytes {
			bestKE = mid
			lo = mid + 1
		} else {
			hi = mid - 1
		}
	}
	seUse := se[:bestKE]
	// Largest stdout prefix with stderr fixed.
	bestKO := 0
	lo, hi = 0, len(so)
	for lo <= hi {
		mid := (lo + hi + 1) / 2
		p2 := bashResultJSON{Stdout: so[:mid], Stderr: seUse, Output: so[:mid], Status: p.Status}
		b2, err2 := json.Marshal(p2)
		if err2 != nil {
			hi = mid - 1
			continue
		}
		if len(b2) <= bashInlineBytes {
			bestKO = mid
			lo = mid + 1
		} else {
			hi = mid - 1
		}
	}
	out := bashResultJSON{Stdout: so[:bestKO], Stderr: seUse, Output: so[:bestKO], Status: p.Status}
	b, err = json.Marshal(out)
	if err != nil || len(b) > bashInlineBytes {
		return minimalBashInlineJSON(toolEnv, p.Status)
	}
	return string(b)
}

func minimalBashInlineJSON(toolEnv map[string]string, status int) string {
	callID := "<call_id>"
	if id, ok := toolEnv["_call_id"]; ok && id != "" {
		callID = id
	}
	// Hint mirrors prior inline header spirit: tool_recall for full content.
	msg := fmt.Sprintf("[output truncated — use tool_recall(%q, offset_byte=0) to read from the beginning]", callID)
	p := bashResultJSON{
		Stdout: "",
		Stderr: msg,
		Output: "",
		Status: status,
	}
	b, err := json.Marshal(p)
	if err != nil {
		fallback, _ := json.Marshal(bashResultJSON{Status: status})
		return string(fallback)
	}
	if len(b) <= bashInlineBytes {
		return string(b)
	}
	// Last resort: shorten stderr message (field carries the hint).
	for len(msg) > 0 {
		msg = msg[:len(msg)-1]
		p.Stderr = msg
		b, err = json.Marshal(p)
		if err != nil {
			fallback, _ := json.Marshal(bashResultJSON{Status: status})
			return string(fallback)
		}
		if len(b) <= bashInlineBytes {
			return string(b)
		}
	}
	fallback, _ := json.Marshal(bashResultJSON{Status: status})
	return string(fallback)
}
