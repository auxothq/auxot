package codingtools

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

func SaveMemoryTool() Tool {
	return Tool{
		Name:        "saveMemory",
		Description: "Persist information to memory/ for recall in future sessions. " +
			"Modes: 'append' adds timestamped entry to daily log, " +
			"'context' updates memory/context.md, " +
			"'update' overwrites a specific memory file.",
		Parameters: json.RawMessage(`{
			"type": "object",
			"properties": {
				"mode": {
					"type": "string",
					"enum": ["append", "update", "context"],
					"description": "append=daily log, context=context.md, update=specific file"
				},
				"content": {
					"type": "string",
					"description": "Content to save"
				},
				"file": {
					"type": "string",
					"description": "Target file relative to memory/ (update mode only)"
				}
			},
			"required": ["mode", "content"]
		}`),
		Execute: executeSaveMemory,
	}
}

func executeSaveMemory(ctx context.Context, workDir string, args json.RawMessage) (string, error) {
	var p struct {
		Mode    string `json:"mode"`
		Content string `json:"content"`
		File    string `json:"file"`
	}
	if err := json.Unmarshal(args, &p); err != nil {
		return "", fmt.Errorf("parse args: %w", err)
	}

	memDir := filepath.Join(workDir, "memory")
	if err := os.MkdirAll(memDir, 0o755); err != nil {
		return "", fmt.Errorf("create memory dir: %w", err)
	}

	switch p.Mode {
	case "append":
		logDir := filepath.Join(memDir, "daily-log")
		if err := os.MkdirAll(logDir, 0o755); err != nil {
			return "", fmt.Errorf("create daily-log dir: %w", err)
		}
		logFile := filepath.Join(logDir, time.Now().Format("2006-01-02")+".md")
		f, err := os.OpenFile(logFile, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
		if err != nil {
			return "", err
		}
		defer f.Close()
		fmt.Fprintf(f, "\n## %s\n%s\n", time.Now().Format("15:04:05"), p.Content)
		return "Appended to daily log.", nil

	case "context":
		err := os.WriteFile(filepath.Join(memDir, "context.md"), []byte(p.Content), 0o644)
		if err != nil {
			return "", err
		}
		return "Updated memory/context.md.", nil

	case "update":
		if p.File == "" {
			return "error: 'file' required for update mode", nil
		}
		target, err := safePath(memDir, p.File)
		if err != nil {
			return "", err
		}
		if err := os.WriteFile(target, []byte(p.Content), 0o644); err != nil {
			return "", err
		}
		return fmt.Sprintf("Updated memory/%s.", p.File), nil

	default:
		return fmt.Sprintf("error: unknown mode %q", p.Mode), nil
	}
}
