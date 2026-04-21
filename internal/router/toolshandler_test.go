package router

import (
	"encoding/json"
	"io"
	"log/slog"
	"strings"
	"testing"

	"github.com/auxothq/auxot/pkg/protocol"
)

// makePickWorkerHandler builds a minimal ToolsWSHandler with the given
// pre-populated workers. Only the fields exercised by pickToolsWorker are set.
func makePickWorkerHandler(workers []*toolsConn) *ToolsWSHandler {
	h := &ToolsWSHandler{
		toolsWorkers: make(map[string]*toolsConn, len(workers)),
		logger:       slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	for _, w := range workers {
		h.toolsWorkers[w.id] = w
	}
	return h
}

// TestPickToolsWorker_Deterministic verifies that the same threadID always
// maps to the same worker across repeated calls, even though Go maps iterate
// in random order.
func TestPickToolsWorker_Deterministic(t *testing.T) {
	t.Parallel()

	workers := []*toolsConn{
		{id: "worker-a", tools: []string{"web_fetch"}},
		{id: "worker-b", tools: []string{"web_fetch"}},
		{id: "worker-c", tools: []string{"web_fetch"}},
	}
	h := makePickWorkerHandler(workers)

	var first *toolsConn
	for i := 0; i < 5; i++ {
		w := h.pickToolsWorker("web_fetch", "thread-A")
		if w == nil {
			t.Fatalf("call %d: pickToolsWorker returned nil", i)
		}
		if first == nil {
			first = w
		} else if w != first {
			t.Errorf("call %d: got worker %q but call 0 returned %q — routing is not deterministic", i, w.id, first.id)
		}
	}
}

// TestPickToolsWorker_DifferentThreads verifies that calls with distinct
// threadIDs return valid (non-nil) workers and do not panic. With 3 workers,
// at least two out of three distinct threads may land on different workers;
// we assert correctness rather than distribution here.
func TestPickToolsWorker_DifferentThreads(t *testing.T) {
	t.Parallel()

	workers := []*toolsConn{
		{id: "worker-a", tools: []string{"web_fetch"}},
		{id: "worker-b", tools: []string{"web_fetch"}},
		{id: "worker-c", tools: []string{"web_fetch"}},
	}
	h := makePickWorkerHandler(workers)

	threads := []string{"thread-A", "thread-B", "thread-C"}
	results := make(map[string]string) // threadID → worker id

	for _, tid := range threads {
		w := h.pickToolsWorker("web_fetch", tid)
		if w == nil {
			t.Fatalf("pickToolsWorker returned nil for thread %q", tid)
		}
		// Calling again with the same threadID must yield the same worker.
		w2 := h.pickToolsWorker("web_fetch", tid)
		if w2 == nil || w2.id != w.id {
			t.Errorf("second call for thread %q returned different worker: %v vs %v", tid, w.id, w2)
		}
		results[tid] = w.id
	}

	// At least verify we have results for all threads (no panics).
	if len(results) != len(threads) {
		t.Errorf("expected %d results, got %d", len(threads), len(results))
	}
}

// TestPickToolsWorker_NoEligible verifies that pickToolsWorker returns nil
// when no worker advertises the requested tool.
func TestPickToolsWorker_NoEligible(t *testing.T) {
	t.Parallel()

	workers := []*toolsConn{
		{id: "worker-a", tools: []string{"web_search"}},
	}
	h := makePickWorkerHandler(workers)

	w := h.pickToolsWorker("browser", "thread-A")
	if w != nil {
		t.Errorf("expected nil for unknown tool, got worker %q", w.id)
	}
}

// TestPickToolsWorker_EmptyToolName verifies that an empty toolName selects
// among all workers (no capability filter).
func TestPickToolsWorker_EmptyToolName(t *testing.T) {
	t.Parallel()

	workers := []*toolsConn{
		{id: "worker-a", tools: []string{"web_search"}},
		{id: "worker-b", tools: []string{"browser"}},
	}
	h := makePickWorkerHandler(workers)

	// Any non-nil worker is acceptable.
	w := h.pickToolsWorker("", "thread-X")
	if w == nil {
		t.Error("expected a worker for empty toolName, got nil")
	}
}

// TestBuildMultimodalContent verifies the JSON shape produced for a text + one
// PNG image pair. The spec requires an array with a "text" part first and an
// "image_url" part whose url begins with "data:image/png;base64,".
func TestBuildMultimodalContent(t *testing.T) {
	t.Parallel()

	// Minimal PNG-like bytes — we only check that they are base64-encoded correctly.
	pngBytes := []byte{0x89, 0x50, 0x4E, 0x47, 0x0D, 0x0A, 0x1A, 0x0A}
	imgs := []protocol.ImagePart{
		{MIMEType: "image/png", Data: pngBytes},
	}

	raw := buildMultimodalContent("hello tool result", imgs)

	var parts []map[string]any
	if err := json.Unmarshal(raw, &parts); err != nil {
		t.Fatalf("buildMultimodalContent returned invalid JSON array: %v", err)
	}
	if len(parts) != 2 {
		t.Fatalf("expected 2 parts (text + image), got %d: %s", len(parts), raw)
	}

	// Part 0: text
	if parts[0]["type"] != "text" {
		t.Errorf("parts[0].type: got %q, want %q", parts[0]["type"], "text")
	}
	if parts[0]["text"] != "hello tool result" {
		t.Errorf("parts[0].text: got %q, want %q", parts[0]["text"], "hello tool result")
	}

	// Part 1: image_url
	if parts[1]["type"] != "image_url" {
		t.Errorf("parts[1].type: got %q, want %q", parts[1]["type"], "image_url")
	}
	imgURL, ok := parts[1]["image_url"].(map[string]any)
	if !ok || imgURL == nil {
		t.Fatalf("parts[1].image_url is not an object: %v", parts[1]["image_url"])
	}
	url, _ := imgURL["url"].(string)
	if !strings.HasPrefix(url, "data:image/png;base64,") {
		t.Errorf("image_url.url should start with 'data:image/png;base64,', got %q", url)
	}
	// Verify the base64 suffix is non-empty (the PNG bytes are encoded).
	suffix := strings.TrimPrefix(url, "data:image/png;base64,")
	if suffix == "" {
		t.Error("image_url.url has empty base64 payload")
	}
}
