// Package codingtools provides Read, Write, Edit, and Bash tools for the
// embedded agentic loop. These are equivalent to the built-in tools in Claude
// Code and Pi, implemented in Go for a dependency-free ~15MB image.
package codingtools

import (
	"context"
	"encoding/json"

	openai "github.com/sashabaranov/go-openai"
)

// Tool describes a single callable tool available to the agentic loop.
type Tool struct {
	Name        string
	Description string
	Parameters  json.RawMessage // JSON Schema for the function parameters
	Execute     func(ctx context.Context, workDir string, args json.RawMessage) (string, error)
}

// AllTools returns the full set of coding tools available to the agent.
func AllTools() []Tool {
	return []Tool{ReadTool(), WriteTool(), EditTool(), BashTool(), UseSkillTool(), SaveMemoryTool()}
}

// OpenAIToolDefs converts AllTools into the OpenAI chat completions tool format
// for inclusion in an API request.
func OpenAIToolDefs() []openai.Tool {
	tools := AllTools()
	out := make([]openai.Tool, len(tools))
	for i, t := range tools {
		t := t // capture loop variable
		out[i] = openai.Tool{
			Type: openai.ToolTypeFunction,
			Function: &openai.FunctionDefinition{
				Name:        t.Name,
				Description: t.Description,
				Parameters:  t.Parameters,
			},
		}
	}
	return out
}

// FindTool returns the Tool with the given name, or nil if not found.
func FindTool(name string) *Tool {
	for _, t := range AllTools() {
		if t.Name == name {
			return &t
		}
	}
	return nil
}
