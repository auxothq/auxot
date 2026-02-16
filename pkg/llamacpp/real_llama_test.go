package llamacpp

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"testing"

	"github.com/auxothq/auxot/pkg/openai"
)

// TestRealLlamaCppToolSchemas sends tool schemas to the REAL llama.cpp server
// to reproduce the Jinja template error. Set LLAMA_URL env var.
func TestRealLlamaCppToolSchemas(t *testing.T) {
	llamaURL := os.Getenv("LLAMA_URL")
	if llamaURL == "" {
		t.Skip("LLAMA_URL not set; skipping real llama.cpp test")
	}

	// Verify llama.cpp is reachable
	resp, err := http.Get(llamaURL + "/health")
	if err != nil {
		t.Skipf("llama.cpp not reachable at %s: %v", llamaURL, err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Skipf("llama.cpp not healthy at %s: status %d", llamaURL, resp.StatusCode)
	}
	t.Logf("llama.cpp is running at %s", llamaURL)

	// ===== STEP 1: Minimal schemas that isolate specific JSON Schema keywords =====
	// Each test sends ONE tool with ONE property that has ONE extra keyword.
	// This pinpoints which keywords break minja.

	keywordTests := map[string]string{
		// Boolean values
		"default_false": `{"type":"object","properties":{"x":{"type":"boolean","default":false}},"required":["x"]}`,
		"default_true":  `{"type":"object","properties":{"x":{"type":"boolean","default":true}},"required":["x"]}`,
		"default_str":   `{"type":"object","properties":{"x":{"type":"string","default":"hello"}},"required":["x"]}`,
		"default_int":   `{"type":"object","properties":{"x":{"type":"integer","default":0}},"required":["x"]}`,

		// Numeric constraints
		"minimum":          `{"type":"object","properties":{"x":{"type":"number","minimum":0}},"required":["x"]}`,
		"maximum":          `{"type":"object","properties":{"x":{"type":"number","maximum":100}},"required":["x"]}`,
		"exclusiveMinimum": `{"type":"object","properties":{"x":{"type":"integer","exclusiveMinimum":0}},"required":["x"]}`,
		"minLength":        `{"type":"object","properties":{"x":{"type":"string","minLength":2}},"required":["x"]}`,
		"maxLength":        `{"type":"object","properties":{"x":{"type":"string","maxLength":100}},"required":["x"]}`,

		// Format
		"format_uri": `{"type":"object","properties":{"x":{"type":"string","format":"uri"}},"required":["x"]}`,

		// Array constraints
		"minItems":  `{"type":"object","properties":{"x":{"type":"array","items":{"type":"string"},"minItems":1}},"required":["x"]}`,
		"maxItems":  `{"type":"object","properties":{"x":{"type":"array","items":{"type":"string"},"maxItems":4}},"required":["x"]}`,

		// Object nested
		"nested_object":      `{"type":"object","properties":{"x":{"type":"object","properties":{"a":{"type":"string"}}}},"required":["x"]}`,
		"additionalProps_false": `{"type":"object","properties":{"x":{"type":"object","properties":{"a":{"type":"string"}},"additionalProperties":false}},"required":["x"]}`,
		"additionalProps_empty": `{"type":"object","properties":{"x":{"type":"object","additionalProperties":{}}},"required":["x"]}`,
		"propertyNames":        `{"type":"object","properties":{"x":{"type":"object","propertyNames":{"type":"string"}}},"required":["x"]}`,

		// anyOf
		"anyOf": `{"type":"object","properties":{"x":{"anyOf":[{"type":"string","enum":["a","b"]},{"type":"string","const":"c"}]}},"required":["x"]}`,

		// $schema at top level
		"dollar_schema": `{"$schema":"https://json-schema.org/draft/2020-12/schema","type":"object","properties":{"x":{"type":"string"}},"required":["x"]}`,

		// $schema + additionalProperties at top level
		"dollar_schema_with_addprops": `{"$schema":"https://json-schema.org/draft/2020-12/schema","type":"object","properties":{"x":{"type":"string"}},"required":["x"],"additionalProperties":false}`,

		// Simple passing baseline
		"simple_string": `{"type":"object","properties":{"x":{"type":"string","description":"test"}},"required":["x"]}`,
	}

	for name, schema := range keywordTests {
		t.Run("keyword/"+name, func(t *testing.T) {
			tool := openai.Tool{
				Type: "function",
				Function: openai.ToolFunction{
					Name:        "test_" + name,
					Description: "Test " + name,
					Parameters:  json.RawMessage(schema),
				},
			}
			err := sendToolsToLlama(t, llamaURL, []openai.Tool{tool})
			if err != nil {
				t.Errorf("FAILS with keyword %s: %v", name, err)
			} else {
				t.Logf("OK with keyword %s", name)
			}
		})
	}

	// ===== STEP 2: ALL 20 tools sanitized at once =====
	allTools := []openai.Tool{
		makeToolFromJSON(t, "Task", "Create a task", `{"$schema":"https://json-schema.org/draft/2020-12/schema","type":"object","properties":{"description":{"description":"A short (3-5 word) description of the task","type":"string"},"prompt":{"description":"The task for the agent to perform","type":"string"},"subagent_type":{"description":"The type of specialized agent to use for this task","type":"string"},"model":{"description":"Optional model to use for this agent.","type":"string","enum":["sonnet","opus","haiku"]},"resume":{"description":"Optional agent ID to resume from.","type":"string"},"run_in_background":{"description":"Set to true to run this agent in the background.","type":"boolean"},"max_turns":{"description":"Maximum number of agentic turns before stopping.","type":"integer","exclusiveMinimum":0,"maximum":9007199254740991}},"required":["description","prompt","subagent_type"],"additionalProperties":false}`),
		makeToolFromJSON(t, "TaskOutput", "Get task output", `{"$schema":"https://json-schema.org/draft/2020-12/schema","type":"object","properties":{"task_id":{"description":"The task ID to get output from","type":"string"},"block":{"description":"Whether to wait for completion","default":true,"type":"boolean"},"timeout":{"description":"Max wait time in ms","default":30000,"type":"number","minimum":0,"maximum":600000}},"required":["task_id","block","timeout"],"additionalProperties":false}`),
		makeToolFromJSON(t, "Bash", "Run a command", `{"$schema":"https://json-schema.org/draft/2020-12/schema","type":"object","properties":{"command":{"description":"The command to execute","type":"string"},"timeout":{"description":"Optional timeout in milliseconds (max 600000)","type":"number"},"description":{"description":"Description of what this command does","type":"string"}},"required":["command"],"additionalProperties":false}`),
		makeToolFromJSON(t, "Glob", "Find files by pattern", `{"$schema":"https://json-schema.org/draft/2020-12/schema","type":"object","properties":{"pattern":{"description":"The glob pattern to match files against","type":"string"},"path":{"description":"The directory to search in.","type":"string"}},"required":["pattern"],"additionalProperties":false}`),
		makeToolFromJSON(t, "Grep", "Search file contents", `{"$schema":"https://json-schema.org/draft/2020-12/schema","type":"object","properties":{"pattern":{"description":"The regular expression pattern to search for","type":"string"},"path":{"description":"File or directory to search in.","type":"string"},"output_mode":{"description":"Output mode","type":"string","enum":["content","files_with_matches","count"]}},"required":["pattern"],"additionalProperties":false}`),
		makeToolFromJSON(t, "ExitPlanMode", "Exit plan mode", `{"$schema":"https://json-schema.org/draft/2020-12/schema","type":"object","properties":{"allowedPrompts":{"description":"Prompt-based permissions","type":"array","items":{"type":"object","properties":{"tool":{"description":"The tool","type":"string","enum":["Bash"]},"prompt":{"description":"Semantic description","type":"string"}},"required":["tool","prompt"],"additionalProperties":false}},"pushToRemote":{"description":"Whether to push to remote","type":"boolean"}},"additionalProperties":{}}`),
		makeToolFromJSON(t, "Read", "Read a file", `{"$schema":"https://json-schema.org/draft/2020-12/schema","type":"object","properties":{"file_path":{"description":"The absolute path to the file","type":"string"},"offset":{"description":"Line number to start from","type":"number"},"limit":{"description":"Number of lines to read","type":"number"}},"required":["file_path"],"additionalProperties":false}`),
		makeToolFromJSON(t, "Edit", "Edit a file", `{"$schema":"https://json-schema.org/draft/2020-12/schema","type":"object","properties":{"file_path":{"description":"Path to modify","type":"string"},"old_string":{"description":"Text to replace","type":"string"},"new_string":{"description":"Replacement text","type":"string"},"replace_all":{"description":"Replace all occurrences","default":false,"type":"boolean"}},"required":["file_path","old_string","new_string"],"additionalProperties":false}`),
		makeToolFromJSON(t, "Write", "Write a file", `{"$schema":"https://json-schema.org/draft/2020-12/schema","type":"object","properties":{"file_path":{"description":"Path to write","type":"string"},"content":{"description":"Content to write","type":"string"}},"required":["file_path","content"],"additionalProperties":false}`),
		makeToolFromJSON(t, "NotebookEdit", "Edit notebook", `{"$schema":"https://json-schema.org/draft/2020-12/schema","type":"object","properties":{"notebook_path":{"description":"Path to notebook","type":"string"},"cell_id":{"description":"Cell ID to edit","type":"string"},"new_source":{"description":"New source","type":"string"},"cell_type":{"description":"Cell type","type":"string","enum":["code","markdown"]},"edit_mode":{"description":"Edit mode","type":"string","enum":["replace","insert","delete"]}},"required":["notebook_path","new_source"],"additionalProperties":false}`),
		makeToolFromJSON(t, "WebFetch", "Fetch URL", `{"$schema":"https://json-schema.org/draft/2020-12/schema","type":"object","properties":{"url":{"description":"URL to fetch","type":"string","format":"uri"},"prompt":{"description":"Prompt for content","type":"string"}},"required":["url","prompt"],"additionalProperties":false}`),
		makeToolFromJSON(t, "WebSearch", "Search web", `{"$schema":"https://json-schema.org/draft/2020-12/schema","type":"object","properties":{"query":{"description":"Search query","type":"string","minLength":2},"allowed_domains":{"description":"Allowed domains","type":"array","items":{"type":"string"}},"blocked_domains":{"description":"Blocked domains","type":"array","items":{"type":"string"}}},"required":["query"],"additionalProperties":false}`),
		makeToolFromJSON(t, "TaskStop", "Stop a task", `{"$schema":"https://json-schema.org/draft/2020-12/schema","type":"object","properties":{"task_id":{"description":"ID of task to stop","type":"string"}},"additionalProperties":false}`),
		makeToolFromJSON(t, "AskUserQuestion", "Ask user", `{"$schema":"https://json-schema.org/draft/2020-12/schema","type":"object","properties":{"questions":{"description":"Questions","minItems":1,"maxItems":4,"type":"array","items":{"type":"object","properties":{"question":{"description":"The question","type":"string"}}}},"answers":{"description":"User answers","type":"object","propertyNames":{"type":"string"},"additionalProperties":{"type":"string"}}},"required":["questions"],"additionalProperties":false}`),
		makeToolFromJSON(t, "Skill", "Use a skill", `{"$schema":"https://json-schema.org/draft/2020-12/schema","type":"object","properties":{"skill":{"description":"Skill name","type":"string"},"args":{"description":"Skill arguments","type":"string"}},"required":["skill"],"additionalProperties":false}`),
		makeToolFromJSON(t, "EnterPlanMode", "Enter plan mode", `{"$schema":"https://json-schema.org/draft/2020-12/schema","type":"object","properties":{},"additionalProperties":false}`),
		makeToolFromJSON(t, "TaskCreate", "Create task", `{"$schema":"https://json-schema.org/draft/2020-12/schema","type":"object","properties":{"subject":{"description":"A brief title for the task","type":"string"},"description":{"description":"A detailed description","type":"string"},"metadata":{"description":"Arbitrary metadata","type":"object","propertyNames":{"type":"string"},"additionalProperties":{}}},"required":["subject","description"],"additionalProperties":false}`),
		makeToolFromJSON(t, "TaskGet", "Get a task", `{"$schema":"https://json-schema.org/draft/2020-12/schema","type":"object","properties":{"taskId":{"description":"Task ID","type":"string"}},"required":["taskId"],"additionalProperties":false}`),
		makeToolFromJSON(t, "TaskUpdate", "Update a task", `{"$schema":"https://json-schema.org/draft/2020-12/schema","type":"object","properties":{"taskId":{"description":"Task ID","type":"string"},"status":{"description":"New status","anyOf":[{"type":"string","enum":["pending","in_progress","completed"]},{"type":"string","const":"deleted"}]},"metadata":{"description":"Metadata to merge","type":"object","propertyNames":{"type":"string"},"additionalProperties":{}}},"required":["taskId"],"additionalProperties":false}`),
		makeToolFromJSON(t, "TaskList", "List tasks", `{"$schema":"https://json-schema.org/draft/2020-12/schema","type":"object","properties":{},"additionalProperties":false}`),
	}

	// Unsanitized should fail
	t.Run("all_20_unsanitized", func(t *testing.T) {
		err := sendToolsToLlama(t, llamaURL, allTools)
		if err != nil {
			t.Logf("EXPECTED failure with unsanitized tools: %v", err)
		} else {
			t.Log("Unsanitized tools passed (unexpected but ok)")
		}
	})

	// Sanitized should pass
	t.Run("all_20_sanitized", func(t *testing.T) {
		sanitized, err := SanitizeTools(allTools)
		if err != nil {
			t.Fatalf("SanitizeTools failed: %v", err)
		}
		err = sendToolsToLlama(t, llamaURL, sanitized)
		if err != nil {
			t.Fatalf("ALL 20 SANITIZED tools STILL FAIL: %v", err)
		}
		t.Log("ALL 20 sanitized tools PASSED against real llama.cpp")
	})
}

func makeToolFromJSON(t *testing.T, name, description, paramsJSON string) openai.Tool {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal([]byte(paramsJSON), &m); err != nil {
		t.Fatalf("Invalid JSON for tool %s: %v", name, err)
	}
	return openai.Tool{
		Type: "function",
		Function: openai.ToolFunction{
			Name:        name,
			Description: description,
			Parameters:  json.RawMessage(paramsJSON),
		},
	}
}

func sendToolsToLlama(t *testing.T, llamaURL string, tools []openai.Tool) error {
	t.Helper()

	req := openai.ChatCompletionRequest{
		Model: "local",
		Messages: []openai.Message{
			{Role: "user", Content: "Say hello"},
		},
		Tools:  tools,
		Stream: false,
	}

	body, err := json.Marshal(req)
	if err != nil {
		return fmt.Errorf("marshal: %w", err)
	}

	httpReq, err := http.NewRequest("POST", llamaURL+"/v1/chat/completions", bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("create request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(httpReq)
	if err != nil {
		return fmt.Errorf("send: %w", err)
	}
	defer resp.Body.Close()

	respBody, _ := io.ReadAll(resp.Body)

	if resp.StatusCode != 200 {
		errStr := string(respBody)
		if len(errStr) > 200 {
			errStr = errStr[:200] + "..."
		}
		return fmt.Errorf("status %d: %s", resp.StatusCode, errStr)
	}

	return nil
}
