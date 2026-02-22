// Package tools implements the built-in tool executors for auxot-tools.
//
// Each tool is a function that receives a JSON arguments object and returns
// a JSON result object (or an error). Tools are registered by name and
// dispatched by the tools worker when it receives a ToolJobMessage.
//
// Built-in tools:
//   - code_executor  — sandboxed JavaScript execution via goja
//   - web_fetch      — HTTP client using Go's net/http (no CORS)
package tools

import (
	"context"
	"encoding/json"
	"fmt"
)

// Result is the output of a tool execution.
type Result struct {
	// Output is the JSON-encoded result to return to the LLM as the tool content.
	// It must be valid JSON. String results should be marshaled as JSON strings.
	Output json.RawMessage
}

// Executor is a function that executes a tool call.
// ctx carries the deadline/cancellation for the call.
// args is the raw JSON object from the LLM's tool call arguments.
type Executor func(ctx context.Context, args json.RawMessage) (Result, error)

// Registry maps tool names to their executor functions.
// Tools are registered at startup and looked up by name when jobs arrive.
type Registry struct {
	executors map[string]Executor
}

// NewRegistry creates an empty tool registry.
func NewRegistry() *Registry {
	return &Registry{executors: make(map[string]Executor)}
}

// Register adds a named tool to the registry.
// Calling Register with a name that already exists overwrites the previous executor.
func (r *Registry) Register(name string, exec Executor) {
	r.executors[name] = exec
}

// Execute dispatches a tool call by name. Returns an error if the tool is unknown.
func (r *Registry) Execute(ctx context.Context, name string, args json.RawMessage) (Result, error) {
	exec, ok := r.executors[name]
	if !ok {
		return Result{}, fmt.Errorf("unknown tool: %q", name)
	}
	return exec(ctx, args)
}

// Names returns the list of registered tool names.
func (r *Registry) Names() []string {
	names := make([]string, 0, len(r.executors))
	for name := range r.executors {
		names = append(names, name)
	}
	return names
}

// Executor returns the executor function for the named tool and a boolean
// indicating whether the tool exists. Useful for copying tools between registries.
func (r *Registry) Executor(name string) (Executor, bool) {
	exec, ok := r.executors[name]
	return exec, ok
}

// DefaultRegistry returns a Registry pre-populated with all built-in tools.
func DefaultRegistry() *Registry {
	r := NewRegistry()
	r.Register("code_executor", CodeExecutor)
	r.Register("web_fetch", WebFetch)
	r.Register("web_search", WebSearch)
	r.Register("web_answers", WebAnswers)
	return r
}

// BuiltinDefinitions returns the JSON schema definitions for all built-in tools,
// suitable for inclusion in an LLM's tool list.
func BuiltinDefinitions() []ToolDefinition {
	return []ToolDefinition{
		CodeExecutorDefinition,
		WebFetchDefinition,
		BraveWebSearchDefinition,
		BraveWebAnswersDefinition,
	}
}

// ToolDefinition holds the metadata for a built-in tool.
type ToolDefinition struct {
	Name        string
	Description string
	Parameters  json.RawMessage // JSON Schema for the input object
}
