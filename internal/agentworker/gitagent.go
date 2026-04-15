package agentworker

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

type GitAgent struct {
	Dir          string
	Soul         string
	Rules        string
	Agents       string
	Skills       []SkillMeta
	MemoryIndex  string
	MemoryCtx    string
	KnowledgeIdx string
	Bootstrap    string
	Config       AgentConfig
}

type SkillMeta struct {
	ID          string
	Name        string
	Description string
	Version     string
	Category    string
	Path        string
}

type AgentConfig struct {
	Name        string         `yaml:"name"`
	Version     string         `yaml:"version"`
	Description string         `yaml:"description"`
	Model       *ModelConfig   `yaml:"model,omitempty"`
	Runtime     *RuntimeConfig `yaml:"runtime,omitempty"`
	Tags        []string       `yaml:"tags,omitempty"`
}

type ModelConfig struct {
	Preferred   string `yaml:"preferred"`
	Constraints struct {
		Temperature float64 `yaml:"temperature"`
		MaxTokens   int     `yaml:"max_tokens"`
	} `yaml:"constraints"`
}

type RuntimeConfig struct {
	MaxTurns int `yaml:"max_turns"`
	Timeout  int `yaml:"timeout"`
}

// SoulMarkdownExists reports whether SOUL.md exists in dir (regular file or other).
func SoulMarkdownExists(dir string) bool {
	_, err := os.Stat(filepath.Join(dir, "SOUL.md"))
	return err == nil
}

// ReadAgentConfigOptional loads agent.yaml when present; otherwise returns zero AgentConfig.
func ReadAgentConfigOptional(dir string) AgentConfig {
	cfgPath := filepath.Join(dir, "agent.yaml")
	data, err := os.ReadFile(cfgPath)
	if err != nil {
		return AgentConfig{}
	}
	var cfg AgentConfig
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return AgentConfig{}
	}
	return cfg
}

// LoadGitAgent reads a gitagent directory and returns a fully populated GitAgent.
// SOUL.md is required; all other files are optional.
func LoadGitAgent(dir string) (*GitAgent, error) {
	soulPath := filepath.Join(dir, "SOUL.md")
	soul, err := os.ReadFile(soulPath)
	if err != nil {
		return nil, fmt.Errorf("SOUL.md is required: %w", err)
	}

	ga := &GitAgent{
		Dir:          dir,
		Soul:         string(soul),
		Rules:        readOptional(dir, "RULES.md"),
		Agents:       readOptional(dir, "AGENTS.md"),
		MemoryIndex:  readOptional(dir, filepath.Join("memory", "MEMORY.md")),
		MemoryCtx:    readOptional(dir, filepath.Join("memory", "context.md")),
		KnowledgeIdx: readOptional(dir, filepath.Join("knowledge", "index.yaml")),
		Bootstrap:    readOptional(dir, filepath.Join("hooks", "bootstrap.md")),
		Skills:       scanSkills(dir),
	}

	cfgPath := filepath.Join(dir, "agent.yaml")
	cfgData, err := os.ReadFile(cfgPath)
	if err == nil {
		if err := yaml.Unmarshal(cfgData, &ga.Config); err != nil {
			return nil, fmt.Errorf("parse agent.yaml: %w", err)
		}
	}

	return ga, nil
}

func readOptional(dir, rel string) string {
	data, err := os.ReadFile(filepath.Join(dir, rel))
	if err != nil {
		return ""
	}
	return string(data)
}

// BuildSystemPrompt composes the full system prompt from all GitAgent sources
// plus server-provided auxotContext. toolNames is the combined list of local
// and external tool names available to the agent.
func (ga *GitAgent) BuildSystemPrompt(auxotContext string, toolNames []string) string {
	var b strings.Builder

	b.WriteString(ga.Soul)

	if ga.Rules != "" {
		b.WriteString("\n\n")
		b.WriteString(ga.Rules)
	}

	if ga.Agents != "" {
		b.WriteString("\n\n")
		b.WriteString(ga.Agents)
	}

	if len(ga.Skills) > 0 {
		b.WriteString("\n\n[available skills]\n")
		b.WriteString("Skills available in your skills/ directory. Call useSkill(skill_id) to load full instructions.\n\n")
		for _, s := range ga.Skills {
			b.WriteString(fmt.Sprintf("- **%s** (%s): %s\n", s.Name, s.ID, s.Description))
		}
		b.WriteString("[/available skills]")
	}

	if ga.MemoryIndex != "" || ga.MemoryCtx != "" {
		b.WriteString("\n\n[memory]\n")
		b.WriteString("Your persistent memory from previous sessions:\n\n")
		if ga.MemoryIndex != "" {
			b.WriteString(ga.MemoryIndex)
			b.WriteString("\n")
		}
		if ga.MemoryCtx != "" {
			if ga.MemoryIndex != "" {
				b.WriteString("\n")
			}
			b.WriteString(ga.MemoryCtx)
			b.WriteString("\n")
		}
		b.WriteString("\nUse Read/Write on memory/ for full history. Use saveMemory to persist learnings for next session.\n")
		b.WriteString("[/memory]")
	}

	if ga.KnowledgeIdx != "" {
		b.WriteString("\n\n[knowledge index]\n")
		b.WriteString(ga.KnowledgeIdx)
		b.WriteString("\n[/knowledge index]")
	}

	if auxotContext != "" {
		b.WriteString("\n\n")
		b.WriteString(auxotContext)
	}

	b.WriteString("\n\n[operational rules]\n")
	b.WriteString("- You are running inside Auxot. Respond conversationally.\n")
	b.WriteString("- Write to memory/ to persist state across sessions.\n")
	b.WriteString("[/operational rules]")

	if ga.Bootstrap != "" {
		b.WriteString("\n\n[bootstrap]\n")
		b.WriteString(ga.Bootstrap)
		b.WriteString("\n[/bootstrap]")
	}

	return b.String()
}

func scanSkills(dir string) []SkillMeta {
	skillsDir := filepath.Join(dir, "skills")
	entries, err := os.ReadDir(skillsDir)
	if err != nil {
		return nil
	}

	var skills []SkillMeta
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		skillFile := filepath.Join(skillsDir, e.Name(), "SKILL.md")
		data, err := os.ReadFile(skillFile)
		if err != nil {
			continue
		}
		sm, err := parseSkillFrontmatter(data)
		if err != nil {
			continue
		}
		sm.ID = e.Name()
		sm.Path = filepath.Join("skills", e.Name(), "SKILL.md")
		skills = append(skills, *sm)
	}
	return skills
}

func parseSkillFrontmatter(content []byte) (*SkillMeta, error) {
	content = bytes.TrimSpace(content)
	if !bytes.HasPrefix(content, []byte("---")) {
		return nil, fmt.Errorf("no frontmatter delimiter found")
	}

	rest := content[3:]
	idx := bytes.Index(rest, []byte("\n---"))
	if idx < 0 {
		return nil, fmt.Errorf("closing frontmatter delimiter not found")
	}
	fmBlock := rest[:idx]

	var fm struct {
		Name        string `yaml:"name"`
		Description string `yaml:"description"`
		Version     string `yaml:"version"`
		Metadata    struct {
			Category string `yaml:"category"`
		} `yaml:"metadata"`
	}
	if err := yaml.Unmarshal(fmBlock, &fm); err != nil {
		return nil, fmt.Errorf("parse frontmatter YAML: %w", err)
	}

	return &SkillMeta{
		Name:        fm.Name,
		Description: fm.Description,
		Version:     fm.Version,
		Category:    fm.Metadata.Category,
	}, nil
}

// RefreshMemory re-reads just the memory files from disk.
func (ga *GitAgent) RefreshMemory() {
	ga.MemoryIndex = readOptional(ga.Dir, filepath.Join("memory", "MEMORY.md"))
	ga.MemoryCtx = readOptional(ga.Dir, filepath.Join("memory", "context.md"))
}
