package openai

import (
	"encoding/json"
	"testing"
)

func TestNormalizeVisionContent_plainStringUnchanged(t *testing.T) {
	in := MessageContentString("hello")
	out := NormalizeVisionContent(in)
	if string(out) != string(in) {
		t.Fatalf("expected unchanged, got %s", out)
	}
}

func TestNormalizeVisionContent_wrapsFlatImageURL(t *testing.T) {
	in := json.RawMessage(`[{"type":"text","text":"hi"},{"type":"image_url","image_url":"data:image/png;base64,abc"}]`)
	out := NormalizeVisionContent(in)

	var parts []struct {
		Type     string          `json:"type"`
		Text     string          `json:"text,omitempty"`
		ImageURL json.RawMessage `json:"image_url,omitempty"`
	}
	if err := json.Unmarshal(out, &parts); err != nil {
		t.Fatal(err)
	}
	if len(parts) != 2 {
		t.Fatalf("want 2 parts, got %d", len(parts))
	}
	if parts[0].Type != "text" || parts[0].Text != "hi" {
		t.Fatalf("text part: %+v", parts[0])
	}
	if parts[1].Type != "image_url" {
		t.Fatalf("want image_url part")
	}
	var nested struct {
		URL string `json:"url"`
	}
	if err := json.Unmarshal(parts[1].ImageURL, &nested); err != nil {
		t.Fatal(err)
	}
	if nested.URL != "data:image/png;base64,abc" {
		t.Fatalf("url: %q", nested.URL)
	}
}

func TestNormalizeVisionContent_nestedUnchanged(t *testing.T) {
	in := json.RawMessage(`[{"type":"image_url","image_url":{"url":"https://example.com/x.png"}}]`)
	out := NormalizeVisionContent(in)
	if string(out) != string(in) {
		t.Fatalf("expected unchanged, got %s", out)
	}
}
