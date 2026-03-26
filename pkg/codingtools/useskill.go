package codingtools

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

func UseSkillTool() Tool {
	return Tool{
		Name:        "useSkill",
		Description: "Load a skill's full instructions from the skills/ directory. " +
			"Pass the skill ID (folder name). Returns the complete SKILL.md content.",
		Parameters: json.RawMessage(`{
			"type": "object",
			"properties": {
				"skill_id": {
					"type": "string",
					"description": "Skill identifier (folder name under skills/)"
				}
			},
			"required": ["skill_id"]
		}`),
		Execute: func(ctx context.Context, workDir string, _ map[string]string, args json.RawMessage) (string, error) {
			return executeUseSkill(ctx, workDir, args)
		},
	}
}

func executeUseSkill(ctx context.Context, workDir string, args json.RawMessage) (string, error) {
	var p struct {
		SkillID string `json:"skill_id"`
	}
	if err := json.Unmarshal(args, &p); err != nil {
		return "", fmt.Errorf("parse args: %w", err)
	}

	absPath, err := safePath(workDir, filepath.Join("skills", p.SkillID, "SKILL.md"))
	if err != nil {
		return "", err
	}

	content, err := os.ReadFile(absPath)
	if err != nil {
		if os.IsNotExist(err) {
			return fmt.Sprintf("Skill %q not found. Check skills/ directory.", p.SkillID), nil
		}
		return "", err
	}
	return string(content), nil
}
