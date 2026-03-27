package tools

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestJSONRPCIDMatches(t *testing.T) {
	tests := []struct {
		raw  string
		want int
		ok   bool
	}{
		{`2`, 2, true},
		{`2.0`, 2, true},
		{`"2"`, 2, true},
		{`3`, 2, false},
		{`""`, 2, false},
		{`null`, 2, false},
		{``, 2, false},
	}
	for _, tt := range tests {
		raw := json.RawMessage(tt.raw)
		if got := jsonRPCIDMatches(raw, tt.want); got != tt.ok {
			t.Fatalf("jsonRPCIDMatches(%q, %d) = %v, want %v", tt.raw, tt.want, got, tt.ok)
		}
	}
}

func TestInterpretMcpToolsCallResult_isError(t *testing.T) {
	raw := json.RawMessage(`{"isError":true,"content":[{"type":"text","text":"bad token"}]}`)
	_, err := interpretMcpToolsCallResult("get_file_contents", raw)
	if err == nil {
		t.Fatal("expected error for isError result")
	}
	if err.Error() != `MCP tool "get_file_contents": bad token` {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestInterpretMcpToolsCallResult_successPassthrough(t *testing.T) {
	raw := json.RawMessage(`{"isError":false,"content":[{"type":"text","text":"ok"}]}`)
	res, err := interpretMcpToolsCallResult("x", raw)
	if err != nil {
		t.Fatal(err)
	}
	if string(res.Output) != string(raw) {
		t.Fatalf("output: %s", res.Output)
	}
}

func TestNormalizeMCPArguments(t *testing.T) {
	if got := string(normalizeMCPArguments(json.RawMessage(`null`))); got != "{}" {
		t.Fatalf("null -> %q", got)
	}
	if got := string(normalizeMCPArguments(nil)); got != "{}" {
		t.Fatalf("nil -> %q", got)
	}
	in := json.RawMessage(`{"owner":"a"}`)
	if got := string(normalizeMCPArguments(in)); got != string(in) {
		t.Fatalf("object -> %q", got)
	}
	if got := string(normalizeMCPArguments(json.RawMessage(`"str"`))); got != "{}" {
		t.Fatalf("string -> %q", got)
	}
}

func TestMcpJSONRPCToolCallError_paramsArgumentsHint(t *testing.T) {
	msg := `[
	  {
	    "code": "invalid_type",
	    "expected": "object",
	    "received": "null",
	    "path": ["params", "arguments"]
	  }
	]`
	err := mcpJSONRPCToolCallError("get_file_contents", -32603, msg)
	if !strings.Contains(err.Error(), "Auxot maps") {
		t.Fatalf("expected hint in: %v", err)
	}
}
