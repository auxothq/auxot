package cliworker

import (
	"bytes"
	"encoding/json"
	"strings"

	"github.com/auxothq/auxot/pkg/protocol"
)

const imageOmittedRetryText = "[image omitted — retry without vision]"

// stripJobImagesExceptLatestInCurrentTurn returns a deep copy of msgs with
// images aggressively stripped to recover from a "prompt too long" error:
//
//   - All images in turns BEFORE the last user message are removed entirely.
//   - In the current turn (at and after the last user message), only the
//     single most-recent image is kept; all earlier images in that turn are
//     stripped. This handles screenshot-heavy captcha/agentic loops where
//     many frames were captured but only the latest matters.
//
// This is the primary recovery path for errPromptTooLong. If the stripped
// prompt is still too long, the caller should additionally drop the oldest
// messages via trimOldestMessages.
func stripJobImagesExceptLatestInCurrentTurn(msgs []protocol.ChatMessage) []protocol.ChatMessage {
	if len(msgs) == 0 {
		return msgs
	}

	// Find the last user message — that is the boundary of the current turn.
	lastUserIdx := -1
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i].Role == "user" {
			lastUserIdx = i
			break
		}
	}

	out := make([]protocol.ChatMessage, len(msgs))
	for i := range msgs {
		out[i] = msgs[i]
	}

	// All messages strictly BEFORE the last user message: strip all images.
	for i := 0; i < lastUserIdx; i++ {
		out[i].Content = stripChatMessageContent(msgs[i].Content)
	}

	// Current turn (lastUserIdx onward): keep only the LATEST image across the
	// entire turn, strip the rest. Walk backwards so the first image we find is
	// the most recent; mark it kept and strip any subsequent images we encounter.
	latestKept := false
	for i := len(out) - 1; i >= lastUserIdx; i-- {
		if !chatMessageHasImage(out[i].Content) {
			continue
		}
		if !latestKept {
			latestKept = true // preserve the most recent image in this turn
			continue
		}
		out[i].Content = stripChatMessageContent(out[i].Content)
	}

	return out
}

// chatMessageHasImage returns true when the message content contains at least
// one image_url or image part. Handles both multimodal JSON arrays and plain
// strings with embedded data URIs.
func chatMessageHasImage(content json.RawMessage) bool {
	if len(content) == 0 {
		return false
	}
	trimmed := bytes.TrimSpace(content)
	if len(trimmed) == 0 {
		return false
	}
	switch trimmed[0] {
	case '[':
		var parts []struct {
			Type string `json:"type"`
		}
		if err := json.Unmarshal(content, &parts); err != nil {
			return false
		}
		for _, p := range parts {
			t := strings.ToLower(strings.TrimSpace(p.Type))
			if t == "image_url" || t == "image" {
				return true
			}
		}
		return false
	case '"':
		var s string
		if err := json.Unmarshal(content, &s); err != nil {
			return false
		}
		return dataURIRe.MatchString(s)
	default:
		return false
	}
}

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
