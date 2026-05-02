package cliworker

import (
	"bytes"
	"encoding/json"
	"strings"

	"github.com/auxothq/auxot/pkg/protocol"
)

const imageOmittedRetryText = "[image omitted — retry without vision]"

// stderrIndicatesImageLoadError matches Claude CLI stderr when the API rejects
// an image (corrupt or unsupported), prompting a single retry without vision.
func stderrIndicatesImageLoadError(stderr string) bool {
	return strings.Contains(strings.ToLower(stderr), "could not process image")
}

// stripJobChatMessages returns a deep copy of msgs with image parts removed
// from each message's Content: multimodal JSON arrays lose type image_url/image
// (with an optional notice text part); plain JSON string content has embedded
// image data URIs replaced (see dataURIRe in mcp_stdio.go).
func stripJobChatMessages(msgs []protocol.ChatMessage) []protocol.ChatMessage {
	if len(msgs) == 0 {
		return msgs
	}
	out := make([]protocol.ChatMessage, len(msgs))
	for i := range msgs {
		out[i] = msgs[i]
		out[i].Content = stripChatMessageContent(msgs[i].Content)
	}
	return out
}

func stripChatMessageContent(content json.RawMessage) json.RawMessage {
	if len(content) == 0 {
		return content
	}
	trimmed := bytes.TrimSpace(content)
	if len(trimmed) == 0 {
		return content
	}
	switch trimmed[0] {
	case '[':
		return stripMultimodalContentArray(content)
	case '"':
		return stripDataURIsFromStringContent(content)
	default:
		return content
	}
}

func stripMultimodalContentArray(content json.RawMessage) json.RawMessage {
	var parts []json.RawMessage
	if err := json.Unmarshal(content, &parts); err != nil {
		return content
	}
	filtered := make([]json.RawMessage, 0, len(parts))
	strippedAny := false
	for _, p := range parts {
		var head struct {
			Type string `json:"type"`
		}
		if err := json.Unmarshal(p, &head); err != nil {
			filtered = append(filtered, p)
			continue
		}
		t := strings.ToLower(strings.TrimSpace(head.Type))
		if t == "image_url" || t == "image" {
			strippedAny = true
			continue
		}
		filtered = append(filtered, p)
	}
	if !strippedAny {
		return content
	}
	if len(filtered) == 0 {
		b, err := json.Marshal([]map[string]any{
			{"type": "text", "text": imageOmittedRetryText},
		})
		if err != nil {
			return content
		}
		return b
	}
	ph, err := json.Marshal(map[string]any{"type": "text", "text": imageOmittedRetryText})
	if err != nil {
		return content
	}
	filtered = append(filtered, json.RawMessage(ph))
	b, err := json.Marshal(filtered)
	if err != nil {
		return content
	}
	return b
}

func stripDataURIsFromStringContent(content json.RawMessage) json.RawMessage {
	var s string
	if err := json.Unmarshal(content, &s); err != nil {
		return content
	}
	cleaned := dataURIRe.ReplaceAllString(s, imageOmittedRetryText)
	if cleaned == s {
		return content
	}
	b, err := json.Marshal(cleaned)
	if err != nil {
		return content
	}
	return b
}
