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

		params := make(map[string]string)
		for _, pm := range paramRe.FindAllStringSubmatch(content, -1) {
			params[pm[1]] = pm[2]
		}

		argsJSON, err := json.Marshal(params)
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

	return calls, strings.TrimSpace(stripped)
}
