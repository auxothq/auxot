// Debug logging for the worker.
//
// All output is JSON. When stderr is a TTY, JSON is pretty-printed
// for human readability. When piped (CI, log aggregators, etc.),
// JSON is compact — one object per line.
//
// Debug levels (--debug flag):
//
//	Level 0 (default): quiet — no debug logging
//	Level 1: WebSocket messages (router ↔ worker) — smart/collapsed, deduplicated
//	Level 2: Level 1 + llama.cpp requests/responses — full logging with per-token output
package worker

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"sync"
	"time"

	"github.com/auxothq/auxot/pkg/logutil"
	"github.com/auxothq/auxot/pkg/protocol"
)

var (
	debugLevel int
	debugMu    sync.Mutex
)

// SetDebugLevel sets the global debug verbosity (0, 1, or 2).
func SetDebugLevel(level int) {
	debugMu.Lock()
	debugLevel = level
	debugMu.Unlock()
}

// DebugLevel returns the current debug level.
func DebugLevel() int {
	debugMu.Lock()
	defer debugMu.Unlock()
	return debugLevel
}

// --- Level 1: WebSocket messages ---

// DebugServerToClient logs a message received from the router.
// All levels use smart filtering for JobMessages (full schemas are in the session JSONL).
// Level 2 additionally logs llama.cpp request/response detail.
func DebugServerToClient(msg any) {
	if DebugLevel() < 1 {
		return
	}
	payload := buildSmartWebSocketPayload(msg)
	writeDebugEntry("ws_recv", "router", "worker", payload)
}

// DebugClientToServer logs a message sent to the router.
// Token and complete messages are suppressed (already covered by INFO logs).
// Level 2 additionally logs llama.cpp request/response detail.
func DebugClientToServer(msg any) {
	if DebugLevel() < 1 {
		return
	}

	// Skip per-token messages — too noisy and already streamed by INFO logs.
	if _, ok := msg.(protocol.TokenMessage); ok {
		return
	}
	// Skip complete messages — already shown in "job completed" INFO log.
	if _, ok := msg.(protocol.CompleteMessage); ok {
		return
	}

	payload := buildSmartWebSocketPayload(msg)
	writeDebugEntry("ws_send", "worker", "router", payload)
}

// --- Level 2: llama.cpp HTTP ---

// DebugWorkerToLlama logs an HTTP request sent to llama.cpp (level >= 2 only).
// At level 1, we already logged the job from the router, so we skip this to avoid duplication.
// At level 2, we show the full request with complete tool schemas and message history.
func DebugWorkerToLlama(msg any) {
	level := DebugLevel()
	if level < 2 {
		return
	}

	// Level 2+: Show full request (no smart filtering at this level)
	writeDebugEntry("llama_request", "worker", "llama.cpp", msg)
}

// DebugLlamaToWorker logs an HTTP response/chunk from llama.cpp (level >= 2).
func DebugLlamaToWorker(chunk string) {
	if DebugLevel() < 2 {
		return
	}
	writeDebugEntry("llama_response", "llama.cpp", "worker", chunk)
}

// buildSmartWebSocketPayload creates a smart view of WebSocket messages for level 1.
// For JobMessage: collapses tools to names, deduplicates messages
// For other messages: returns as-is (they're already small)
func buildSmartWebSocketPayload(msg any) any {
	// Check if it's a JobMessage (the one with full tools and messages)
	jobMsg, ok := msg.(protocol.JobMessage)
	if !ok {
		// Not a job message - return as-is (other messages are small)
		return msg
	}

	// Build smart payload for JobMessage
	payload := map[string]any{
		"type":   jobMsg.Type,
		"job_id": jobMsg.JobID,
	}

	if jobMsg.Temperature != nil {
		payload["temperature"] = *jobMsg.Temperature
	}
	if jobMsg.MaxTokens != nil {
		payload["max_tokens"] = *jobMsg.MaxTokens
	}
	if jobMsg.ReasoningEffort != "" {
		payload["reasoning_effort"] = jobMsg.ReasoningEffort
	}

	// Collapse tools to just names
	if len(jobMsg.Tools) > 0 {
		toolNames := make([]string, len(jobMsg.Tools))
		for i, t := range jobMsg.Tools {
			toolNames[i] = t.Function.Name
		}
		payload["tools"] = toolNames
	}

	// Log message count only — content is in the session JSONL file.
	payload["num_messages"] = len(jobMsg.Messages)

	return payload
}

// debugEntry is the JSON envelope for debug log lines.
// Matches slog's JSON shape so all output is visually consistent.
type debugEntry struct {
	Time    string `json:"time"`
	Level   string `json:"level"`
	Msg     string `json:"msg"`
	Source  string `json:"source"`
	Dest    string `json:"dest"`
	Payload any    `json:"payload"`
}

func writeDebugEntry(msg, source, dest string, payload any) {
	entry := debugEntry{
		Time:    time.Now().Format(time.RFC3339Nano),
		Level:   "DEBUG",
		Msg:     msg,
		Source:  source,
		Dest:    dest,
		Payload: payload,
	}
	data := formatJSON(entry)
	// DEBUG logs go to stdout (not stderr) to follow Unix conventions
	_, _ = os.Stdout.Write(data)
	_, _ = os.Stdout.Write([]byte("\n"))
}

// formatJSON marshals v — pretty on TTY, compact on pipe.
// Uses SetEscapeHTML(false) so angle brackets aren't escaped.
func formatJSON(v any) []byte {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)

	if logutil.IsTTY() {
		enc.SetIndent("", "  ")
	}

	if err := enc.Encode(v); err != nil {
		return []byte(fmt.Sprintf(`{"error":"marshal: %v"}`, err))
	}

	// Encode adds a trailing newline — strip it since callers add their own
	b := buf.Bytes()
	if len(b) > 0 && b[len(b)-1] == '\n' {
		b = b[:len(b)-1]
	}
	return b
}
