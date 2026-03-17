package codingtools

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"time"
)

const (
	defaultBashTimeoutSecs = 120
	maxBashOutputBytes     = 100 * 1024 // 100 KB
)

// BashArgs are the arguments for the bash tool.
type BashArgs struct {
	Command string `json:"command"`
	Timeout int    `json:"timeout,omitempty"` // seconds, default 120
}

// BashTool returns a Tool that executes bash commands in the agent directory.
// The container itself is the security boundary — bash is not sandboxed to the
// agent directory, matching the behavior of Claude Code and Pi.
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
		Execute: func(ctx context.Context, workDir string, args json.RawMessage) (string, error) {
			return executeBash(ctx, workDir, args)
		},
	}
}

func executeBash(ctx context.Context, workDir string, rawArgs json.RawMessage) (string, error) {
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
	return result, nil
}
