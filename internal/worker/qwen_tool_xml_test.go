package worker

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestExtractXMLToolCalls_BasicSingleCall(t *testing.T) {
	reasoning := `<tool_call>
<function=web_search>
<parameter=query>golang regexp multiline</parameter>
</function>
</tool_call>`

	calls, stripped := extractXMLToolCalls(reasoning)

	if len(calls) != 1 {
		t.Fatalf("expected 1 call, got %d", len(calls))
	}
	if calls[0].Function.Name != "web_search" {
		t.Errorf("expected tool name %q, got %q", "web_search", calls[0].Function.Name)
	}
	if calls[0].Type != "function" {
		t.Errorf("expected type %q, got %q", "function", calls[0].Type)
	}
	if !strings.HasPrefix(calls[0].ID, "xml_") {
		t.Errorf("expected ID to start with xml_, got %q", calls[0].ID)
	}

	var args map[string]string
	if err := json.Unmarshal([]byte(calls[0].Function.Arguments), &args); err != nil {
		t.Fatalf("arguments not valid JSON: %v", err)
	}
	if args["query"] != "golang regexp multiline" {
		t.Errorf("expected query=%q, got %q", "golang regexp multiline", args["query"])
	}

	if stripped != "" {
		t.Errorf("expected stripped reasoning to be empty, got %q", stripped)
	}
}

func TestExtractXMLToolCalls_NoTag(t *testing.T) {
	reasoning := "I am just thinking about the problem. No tools needed."

	calls, stripped := extractXMLToolCalls(reasoning)

	if calls != nil {
		t.Errorf("expected nil calls, got %v", calls)
	}
	if stripped != reasoning {
		t.Errorf("expected reasoning unchanged, got %q", stripped)
	}
}

func TestExtractXMLToolCalls_MultiLineParameter(t *testing.T) {
	reasoning := `<tool_call>
<function=code_exec>
<parameter=code>package main

import "fmt"

func main() {
	fmt.Println("hello")
}</parameter>
</function>
</tool_call>`

	calls, _ := extractXMLToolCalls(reasoning)

	if len(calls) != 1 {
		t.Fatalf("expected 1 call, got %d", len(calls))
	}

	var args map[string]string
	if err := json.Unmarshal([]byte(calls[0].Function.Arguments), &args); err != nil {
		t.Fatalf("arguments not valid JSON: %v", err)
	}

	if !strings.Contains(args["code"], "fmt.Println") {
		t.Errorf("expected code to contain fmt.Println, got %q", args["code"])
	}
	if !strings.Contains(args["code"], "\n") {
		t.Errorf("expected code to contain newlines, got %q", args["code"])
	}
}

func TestExtractXMLToolCalls_ThinkingTextPreserved(t *testing.T) {
	reasoning := `I need to search for some information to answer this question.

<tool_call>
<function=web_search>
<parameter=query>auxot agent orchestration</parameter>
</function>
</tool_call>`

	calls, stripped := extractXMLToolCalls(reasoning)

	if len(calls) != 1 {
		t.Fatalf("expected 1 call, got %d", len(calls))
	}
	if calls[0].Function.Name != "web_search" {
		t.Errorf("expected tool name web_search, got %q", calls[0].Function.Name)
	}

	if !strings.Contains(stripped, "I need to search for some information") {
		t.Errorf("expected thinking text preserved in stripped, got %q", stripped)
	}
	if strings.Contains(stripped, "<tool_call>") {
		t.Errorf("expected tool_call block removed from stripped, got %q", stripped)
	}
}

func TestExtractXMLToolCalls_MultipleToolCalls(t *testing.T) {
	reasoning := `<tool_call>
<function=read_file>
<parameter=path>/etc/hosts</parameter>
</function>
</tool_call>

<tool_call>
<function=web_search>
<parameter=query>best practices</parameter>
<parameter=limit>10</parameter>
</function>
</tool_call>`

	calls, stripped := extractXMLToolCalls(reasoning)

	if len(calls) != 2 {
		t.Fatalf("expected 2 calls, got %d", len(calls))
	}

	if calls[0].Function.Name != "read_file" {
		t.Errorf("expected first tool read_file, got %q", calls[0].Function.Name)
	}
	if calls[1].Function.Name != "web_search" {
		t.Errorf("expected second tool web_search, got %q", calls[1].Function.Name)
	}

	var args0 map[string]string
	if err := json.Unmarshal([]byte(calls[0].Function.Arguments), &args0); err != nil {
		t.Fatalf("call[0] arguments not valid JSON: %v", err)
	}
	if args0["path"] != "/etc/hosts" {
		t.Errorf("expected path=/etc/hosts, got %q", args0["path"])
	}

	var args1 map[string]string
	if err := json.Unmarshal([]byte(calls[1].Function.Arguments), &args1); err != nil {
		t.Fatalf("call[1] arguments not valid JSON: %v", err)
	}
	if args1["query"] != "best practices" {
		t.Errorf("expected query=%q, got %q", "best practices", args1["query"])
	}
	if args1["limit"] != "10" {
		t.Errorf("expected limit=10, got %q", args1["limit"])
	}

	if strings.TrimSpace(stripped) != "" {
		t.Errorf("expected stripped to be empty (only whitespace), got %q", stripped)
	}

	// IDs should be unique
	if calls[0].ID == calls[1].ID {
		t.Errorf("expected unique IDs, both are %q", calls[0].ID)
	}
}
