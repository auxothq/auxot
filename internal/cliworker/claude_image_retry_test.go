package cliworker

import (
	"encoding/json"
	"errors"
	"fmt"
	"testing"

	"github.com/auxothq/auxot/pkg/protocol"
)

func TestStderrIndicatesImageLoadError(t *testing.T) {
	tests := []struct {
		stderr string
		want   bool
	}{
		{"Could not process image: bad format", true},
		{"could not process image", true},
		{"COULD NOT PROCESS IMAGE", true},
		{"error: something else", false},
		{"", false},
	}
	for _, tt := range tests {
		if got := stderrIndicatesImageLoadError(tt.stderr); got != tt.want {
			t.Errorf("stderrIndicatesImageLoadError(%q) = %v, want %v", tt.stderr, got, tt.want)
		}
	}
}

func TestStripJobChatMessages_multimodalRemovesImageURL(t *testing.T) {
	raw := json.RawMessage(`[{"type":"text","text":"hi"},{"type":"image_url","image_url":{"url":"data:image/png;base64,AAAA"}}]`)
	in := []protocol.ChatMessage{{Role: "user", Content: raw}}
	out := stripJobChatMessages(in)

	if len(out) != 1 {
		t.Fatalf("len(out) = %d, want 1", len(out))
	}
	var parts []map[string]any
	if err := json.Unmarshal(out[0].Content, &parts); err != nil {
		t.Fatal(err)
	}
	if len(parts) != 2 {
		t.Fatalf("want 2 parts (text + notice), got %d: %v", len(parts), parts)
	}
	if parts[0]["type"] != "text" || parts[0]["text"] != "hi" {
		t.Errorf("first part = %#v, want text hi", parts[0])
	}
	if parts[1]["type"] != "text" || parts[1]["text"] != imageOmittedRetryText {
		t.Errorf("second part = %#v, want notice text", parts[1])
	}
	// Original unchanged
	var origParts []map[string]any
	_ = json.Unmarshal(in[0].Content, &origParts)
	if len(origParts) != 2 {
		t.Errorf("original message should be unchanged, got %d parts", len(origParts))
	}
}

func TestStripJobChatMessages_multimodalImageType(t *testing.T) {
	raw := json.RawMessage(`[{"type":"image","source":{"type":"base64"}}]`)
	out := stripJobChatMessages([]protocol.ChatMessage{{Role: "user", Content: raw}})
	var parts []map[string]any
	if err := json.Unmarshal(out[0].Content, &parts); err != nil {
		t.Fatal(err)
	}
	if len(parts) != 1 || parts[0]["type"] != "text" {
		t.Fatalf("got %v", parts)
	}
}

func TestStripJobChatMessages_plainStringStripsDataURI(t *testing.T) {
	raw := protocol.ChatContentString(`see data:image/png;base64,QUJD`)
	out := stripJobChatMessages([]protocol.ChatMessage{{Role: "user", Content: raw}})
	var s string
	if err := json.Unmarshal(out[0].Content, &s); err != nil {
		t.Fatal(err)
	}
	want := "see " + imageOmittedRetryText
	if s != want {
		t.Fatalf("got %q, want %q", s, want)
	}
}

func TestStripJobChatMessages_noImagesUnchanged(t *testing.T) {
	raw := json.RawMessage(`[{"type":"text","text":"only text"}]`)
	in := []protocol.ChatMessage{{Role: "user", Content: raw}}
	out := stripJobChatMessages(in)
	if string(out[0].Content) != string(in[0].Content) {
		t.Errorf("content changed: %s", string(out[0].Content))
	}
}

func TestErrImageLoadErrorSentinel(t *testing.T) {
	if errImageLoadError.Error() != "cliworker: image load error" {
		t.Fatalf("unexpected sentinel: %v", errImageLoadError)
	}
	wrapped := fmt.Errorf("claude: %w", errImageLoadError)
	if !errors.Is(wrapped, errImageLoadError) {
		t.Fatal("errors.Is should recognize sentinel when wrapped")
	}
}
