package builtins

import (
	"embed"
	"encoding/json"
	"strings"
)

//go:embed skills/*.md
var skillFiles embed.FS

var registry map[string]string

func init() {
	registry = make(map[string]string)
	entries, err := skillFiles.ReadDir("skills")
	if err != nil {
		return
	}
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		name := entry.Name()
		if !strings.HasSuffix(name, ".md") {
			continue
		}
		stem := strings.TrimSuffix(name, ".md")
		parts := strings.SplitN(stem, "-", 2)
		var id string
		if len(parts) == 2 {
			id = parts[0] + ":" + parts[1]
		} else {
			id = stem
		}
		content, err := skillFiles.ReadFile("skills/" + name)
		if err == nil {
			registry[id] = string(content)
		}
	}
}

// LookupSkill returns the content of a built-in skill by its namespaced ID.
func LookupSkill(id string) (string, bool) {
	content, ok := registry[id]
	return content, ok
}

// LookupSkillFromArgs parses useSkill arguments JSON and looks up skill_id in the auxot: registry.
func LookupSkillFromArgs(argsJSON string) (string, bool) {
	var args struct {
		SkillID string `json:"skill_id"`
	}
	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
		return "", false
	}
	if !strings.HasPrefix(args.SkillID, "auxot:") {
		return "", false
	}
	return LookupSkill(args.SkillID)
}
