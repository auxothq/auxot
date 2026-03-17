package codingtools

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
)

// ReadArgs are the arguments for the read tool.
type ReadArgs struct {
	Path   string `json:"path"`
	Offset int    `json:"offset,omitempty"` // 1-indexed line to start from
	Limit  int    `json:"limit,omitempty"`  // max lines to return
}

// ReadTool returns a Tool that reads files from the agent directory.
func ReadTool() Tool {
	return Tool{
		Name:        "read",
		Description: "Read a file from the agent directory. Returns file contents with line numbers prefixed (LINE|CONTENT). Use offset and limit to read a slice of a large file.",
		Parameters: json.RawMessage(`{
			"type": "object",
			"properties": {
				"path": {
					"type": "string",
					"description": "Path to the file, relative to the agent directory"
				},
				"offset": {
					"type": "integer",
					"description": "Line number to start reading from (1-indexed). Omit to start from the beginning."
				},
				"limit": {
					"type": "integer",
					"description": "Maximum number of lines to return. Omit to read the entire file."
				}
			},
			"required": ["path"]
		}`),
		Execute: func(ctx context.Context, workDir string, args json.RawMessage) (string, error) {
			return executeRead(workDir, args)
		},
	}
}

func executeRead(workDir string, rawArgs json.RawMessage) (string, error) {
	var args ReadArgs
	if err := json.Unmarshal(rawArgs, &args); err != nil {
		return "", fmt.Errorf("invalid read args: %w", err)
	}
	if args.Path == "" {
		return "", fmt.Errorf("path is required")
	}

	absPath, err := safePath(workDir, args.Path)
	if err != nil {
		return "", err
	}

	data, err := os.ReadFile(absPath)
	if err != nil {
		return "", fmt.Errorf("read %s: %w", args.Path, err)
	}

	lines := strings.Split(string(data), "\n")

	start := 0
	if args.Offset > 0 {
		start = args.Offset - 1
	}
	if start >= len(lines) {
		return "", fmt.Errorf("offset %d exceeds file length %d lines", args.Offset, len(lines))
	}

	end := len(lines)
	if args.Limit > 0 && start+args.Limit < end {
		end = start + args.Limit
	}

	selected := lines[start:end]
	var sb strings.Builder
	for i, line := range selected {
		fmt.Fprintf(&sb, "%d|%s\n", start+i+1, line)
	}
	return sb.String(), nil
}
