package worker

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"regexp"
	"strings"

	"github.com/auxothq/auxot/pkg/protocol"
)

var (
	// toolCallBlockRe matches a full <tool_call>...</tool_call> block including newlines.
	toolCallBlockRe = regexp.MustCompile(`(?s)<tool_call>\s*(.*?)\s*</tool_call>`)

	// functionNameRe extracts the tool name from <function=NAME> lines.
	functionNameRe = regexp.MustCompile(`<function=(\S+?)>`)

	// paramRe extracts <parameter=NAME>VALUE</parameter> pairs; VALUE may span lines.
	paramRe = regexp.MustCompile(`(?s)<parameter=(\S+?)>(.*?)</parameter>`)

	// inlineToolCallRe matches the "[Calling tool: NAME({...})]" fallback format that
	// some Qwen3 builds emit when XML parsing has failed previously in the conversation.
	inlineToolCallRe = regexp.MustCompile(`\[Calling tool:\s*(\w+)\((\{.*?\})\)\]`)
)

// extractXMLToolCalls parses Qwen3-style tool call XML from reasoning content.
// Returns extracted tool calls and the reasoning content with tool call blocks stripped.
//
// Qwen3 emits this format inside its thinking block when llama.cpp doesn't
// produce structured delta.tool_calls:
//
//	<tool_call>
//	<function=TOOL_NAME>
//	<parameter=PARAM_NAME>VALUE</parameter>
//	</function>
//	</tool_call>
func extractXMLToolCalls(reasoning string) ([]protocol.ToolCall, string) {
	if !strings.Contains(reasoning, "<tool_call>") {
		return nil, reasoning
	}

	var calls []protocol.ToolCall

	stripped := toolCallBlockRe.ReplaceAllStringFunc(reasoning, func(block string) string {
		m := toolCallBlockRe.FindStringSubmatch(block)
		if m == nil {
			return block
		}
		content := m[1]

		nameMatch := functionNameRe.FindStringSubmatch(content)
		if nameMatch == nil {
			// Can't parse tool name — leave the block in place so nothing is lost.
			return block
		}
		toolName := nameMatch[1]

		// Build the arguments object. If a parameter value is itself valid JSON
		// (object, array, number, bool) we embed it as raw JSON rather than
		// double-encoding it as a string. This handles the common case where the
		// model passes structured args like:
		//   <parameter=args>{"count": 5}</parameter>
		// which must become {"args":{"count":5}} not {"args":"{\"count\":5}"}.
		paramsRaw := make(map[string]json.RawMessage)
		for _, pm := range paramRe.FindAllStringSubmatch(content, -1) {
			val := strings.TrimSpace(pm[2])
			// Only embed raw JSON for objects and arrays. Numbers, booleans, and
			// plain strings are kept as JSON strings for backward compatibility with
			// llama.cpp tool calls that pass numeric/boolean values as text.
			if len(val) > 0 && (val[0] == '{' || val[0] == '[') && json.Valid([]byte(val)) {
				paramsRaw[pm[1]] = json.RawMessage(val)
			} else {
				encoded, err := json.Marshal(val)
				if err != nil {
					continue
				}
				paramsRaw[pm[1]] = json.RawMessage(encoded)
			}
		}

		argsJSON, err := json.Marshal(paramsRaw)
		if err != nil {
			return block
		}

		b := make([]byte, 4)
		_, _ = rand.Read(b)
		callID := "xml_" + hex.EncodeToString(b)

		calls = append(calls, protocol.ToolCall{
			ID:   callID,
			Type: "function",
			Function: protocol.ToolFunction{
				Name:      toolName,
				Arguments: string(argsJSON),
			},
		})

		return ""
	})

	// Also handle "[Calling tool: NAME({...})]" inline format.
	stripped = inlineToolCallRe.ReplaceAllStringFunc(stripped, func(match string) string {
		m := inlineToolCallRe.FindStringSubmatch(match)
		if m == nil {
			return match
		}
		toolName := m[1]
		argsJSON := m[2]
		if !json.Valid([]byte(argsJSON)) {
			return match
		}
		b := make([]byte, 4)
		_, _ = rand.Read(b)
		calls = append(calls, protocol.ToolCall{
			ID:   "xml_" + hex.EncodeToString(b),
			Type: "function",
			Function: protocol.ToolFunction{
				Name:      toolName,
				Arguments: argsJSON,
			},
		})
		return ""
	})

	return calls, strings.TrimSpace(stripped)
}

// ExtractXMLToolCallsFromContent is the exported entry point used by executor
// to check both reasoning and response content.
func ExtractXMLToolCallsFromContent(text string) ([]protocol.ToolCall, string) {
	return extractXMLToolCalls(text)
}
