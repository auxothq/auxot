package codingtools

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"time"
)

const (
	defaultBashTimeoutSecs = 120
	bashInlineBytes        = 1_024          // returned inline to the LLM
	maxBashOutputBytes     = 100 * 1024     // collected from subprocess, full result
)

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
		Name:        "bash",
		Description: "Execute a bash command. The working directory is the agent directory. stdout and stderr are captured and returned combined. Output is capped at 100KB.",
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

	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out

	// Run and capture exit code in the output — don't return an error for
	// non-zero exit codes, as the model needs to see the output to diagnose.
	if err := cmd.Run(); err != nil {
		if out.Len() == 0 {
			// No output and command failed — surface the error.
			return "", fmt.Errorf("bash: %w", err)
		}
		// Include a note about the non-zero exit.
		fmt.Fprintf(&out, "\n[exit: %v]", err)
	}

	result := out.String()
	if len(result) > maxBashOutputBytes {
		result = result[:maxBashOutputBytes] + "\n[output truncated at 100KB]"
	}
	if result == "" {
		return "(no output)", nil
	}

	// Cap inline output to the LLM; return the tail (most recent output is most actionable).
	if len(result) > bashInlineBytes {
		callID := "<call_id>"
		if id, ok := toolEnv["_call_id"]; ok && id != "" {
			callID = id
		}
		header := fmt.Sprintf("[output truncated — showing last 1024 bytes; use tool_recall(%q, offset_byte=0) to read from the beginning]\n", callID)
		tail := result[len(result)-bashInlineBytes:]
		return header + tail, nil
	}
	return result, nil
}
