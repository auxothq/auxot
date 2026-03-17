package codingtools

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
)

// EditArgs are the arguments for the edit tool.
type EditArgs struct {
	Path       string `json:"path"`
	OldString  string `json:"old_string"`
	NewString  string `json:"new_string"`
	ReplaceAll bool   `json:"replace_all,omitempty"`
}

// EditTool returns a Tool that performs exact string replacement in a file.
func EditTool() Tool {
	return Tool{
		Name:        "edit",
		Description: "Replace an exact string in a file. By default, old_string must appear exactly once — use replace_all to replace every occurrence. Fails if old_string is not found.",
		Parameters: json.RawMessage(`{
			"type": "object",
			"properties": {
				"path": {
					"type": "string",
					"description": "Path to the file, relative to the agent directory"
				},
				"old_string": {
					"type": "string",
					"description": "The exact string to find and replace"
				},
				"new_string": {
					"type": "string",
					"description": "The replacement string"
				},
				"replace_all": {
					"type": "boolean",
					"description": "Replace all occurrences instead of requiring exactly one (default: false)"
				}
			},
			"required": ["path", "old_string", "new_string"]
		}`),
		Execute: func(ctx context.Context, workDir string, args json.RawMessage) (string, error) {
			return executeEdit(workDir, args)
		},
	}
}

func executeEdit(workDir string, rawArgs json.RawMessage) (string, error) {
	var args EditArgs
	if err := json.Unmarshal(rawArgs, &args); err != nil {
		return "", fmt.Errorf("invalid edit args: %w", err)
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

	content := string(data)
	count := strings.Count(content, args.OldString)

	if count == 0 {
		return "", fmt.Errorf("old_string not found in %s", args.Path)
	}
	if count > 1 && !args.ReplaceAll {
		return "", fmt.Errorf("old_string appears %d times in %s; set replace_all=true to replace all occurrences", count, args.Path)
	}

	var updated string
	if args.ReplaceAll {
		updated = strings.ReplaceAll(content, args.OldString, args.NewString)
	} else {
		updated = strings.Replace(content, args.OldString, args.NewString, 1)
	}

	if err := os.WriteFile(absPath, []byte(updated), 0644); err != nil {
		return "", fmt.Errorf("write %s: %w", args.Path, err)
	}

	replacements := count
	if !args.ReplaceAll {
		replacements = 1
	}
	return fmt.Sprintf("Replaced %d occurrence(s) in %s", replacements, args.Path), nil
}
