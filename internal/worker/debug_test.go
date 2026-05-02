package worker

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/auxothq/auxot/pkg/protocol"
)

func TestRedactImageDataURIs(t *testing.T) {
	const tinyB64 = "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mP8z8BQDwAEhQGAhKmMIQAAAABJRU5ErkJggg=="
	raw := `before data:image/png;base64,` + tinyB64 + ` after`
	got := redactImageDataURIs(raw)
	if strings.Contains(got, tinyB64) {
		t.Fatalf("expected tiny base64 removed from string, got %q", got)
	}
	if !strings.Contains(got, "image/png") || !strings.Contains(got, "redacted") {
		t.Fatalf("expected redaction placeholder with mime hint, got %q", got)
	}
	if !strings.Contains(got, "before") || !strings.Contains(got, "after") {
		t.Fatalf("expected surrounding text preserved, got %q", got)
	}
}

func TestBuildSmartWebSocketPayload_JobToolCallResultRedactsBase64(t *testing.T) {
	const b64 = "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mP8z8BQDwAEhQGAhKmMIQAAAABJRU5ErkJggg=="
	resultJSON := `{"return":[{"type":"image_url","image_url":{"url":"data:image/png;base64,` + b64 + `"}}]}`
	msg := protocol.JobToolCallResultMessage{
		Type:    protocol.TypeJobToolCallResult,
		JobID:   "job-1",
		CallID:  "call-1",
		Result:  resultJSON,
		IsError: false,
	}
	payload := buildSmartWebSocketPayload(msg)
	data, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	s := string(data)
	if strings.Contains(s, b64) {
		t.Fatalf("marshaled debug payload still contained raw base64: %s", s)
	}
}

func TestBuildSmartWebSocketPayload_JobToolCallResultRedactsImageParts(t *testing.T) {
	msg := protocol.JobToolCallResultMessage{
		Type:   protocol.TypeJobToolCallResult,
		JobID:  "j",
		CallID: "c",
		Result: "ok",
		ImageParts: []protocol.ImagePart{
			{MIMEType: "image/png", Data: []byte{1, 2, 3, 4, 5}},
		},
	}
	payload := buildSmartWebSocketPayload(msg)
	m, ok := payload.(map[string]any)
	if !ok {
		t.Fatalf("expected map, got %T", payload)
	}
	parts, _ := m["image_parts"].([]map[string]any)
	if len(parts) != 1 {
		t.Fatalf("image_parts: %+v", m["image_parts"])
	}
	dataStr, _ := parts[0]["data"].(string)
	if dataStr != "<redacted 5 bytes>" {
		t.Fatalf("data field: %q", dataStr)
	}
}
