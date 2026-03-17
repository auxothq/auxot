package codingtools

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// WriteArgs are the arguments for the write tool.
type WriteArgs struct {
	Path    string `json:"path"`
	Content string `json:"content"`
}

// WriteTool returns a Tool that writes (or overwrites) files in the agent directory.
func WriteTool() Tool {
	return Tool{
		Name:        "write",
		Description: "Write content to a file in the agent directory. Creates the file and any parent directories if they do not exist. Overwrites existing files.",
		Parameters: json.RawMessage(`{
			"type": "object",
			"properties": {
				"path": {
					"type": "string",
					"description": "Path to the file, relative to the agent directory"
				},
				"content": {
					"type": "string",
					"description": "Content to write to the file"
				}
			},
			"required": ["path", "content"]
		}`),
		Execute: func(ctx context.Context, workDir string, args json.RawMessage) (string, error) {
			return executeWrite(workDir, args)
		},
	}
}

func executeWrite(workDir string, rawArgs json.RawMessage) (string, error) {
	var args WriteArgs
	if err := json.Unmarshal(rawArgs, &args); err != nil {
		return "", fmt.Errorf("invalid write args: %w", err)
	}
	if args.Path == "" {
		return "", fmt.Errorf("path is required")
	}

	absPath, err := safePath(workDir, args.Path)
	if err != nil {
		return "", err
	}

	if err := os.MkdirAll(filepath.Dir(absPath), 0755); err != nil {
		return "", fmt.Errorf("create parent directories for %s: %w", args.Path, err)
	}

	if err := os.WriteFile(absPath, []byte(args.Content), 0644); err != nil {
		return "", fmt.Errorf("write %s: %w", args.Path, err)
	}

	return fmt.Sprintf("Wrote %d bytes to %s", len(args.Content), args.Path), nil
}
