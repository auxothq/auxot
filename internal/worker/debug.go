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
	"regexp"
	"strings"
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

// dataImageBase64Re matches RFC 2397 image data URIs so debug logs never echo huge base64 blobs.
var dataImageBase64Re = regexp.MustCompile(`(?i)data:image/[a-z0-9.+-]+;base64,[a-z0-9+/=]*`)

// redactImageDataURIs replaces inline image data URIs with a short placeholder (length hints only).
func redactImageDataURIs(s string) string {
	return dataImageBase64Re.ReplaceAllStringFunc(s, func(match string) string {
		lower := strings.ToLower(match)
		idx := strings.Index(lower, ";base64,")
		if idx < 0 {
			return "<image data redacted>"
		}
		b64 := match[idx+len(";base64,"):]
		n := len(b64)
		approxBin := (n * 3) / 4
		mime := "image/*"
		if strings.HasPrefix(lower, "data:") {
			rest := match[len("data:"):idx] // e.g. image/png
			if rest != "" {
				mime = rest
			}
		}
		return fmt.Sprintf("<%s base64 redacted: %d chars ~%d KiB>", mime, n, (approxBin+1023)/1024)
	})
}

func redactImagePartsForLog(parts []protocol.ImagePart) []map[string]any {
	if len(parts) == 0 {
		return nil
	}
	out := make([]map[string]any, len(parts))
	for i, p := range parts {
		out[i] = map[string]any{
			"mime_type": p.MIMEType,
			"data":      fmt.Sprintf("<redacted %d bytes>", len(p.Data)),
		}
	}
	return out
}

// buildSmartWebSocketPayload creates a smart view of WebSocket messages for level 1.
// For JobMessage: collapses tools to names, deduplicates messages
// For messages that may carry image bytes or data URIs: strips binary payloads from the log view
// For other messages: returns as-is (they're already small)
func buildSmartWebSocketPayload(msg any) any {
	switch jobMsg := msg.(type) {
	case protocol.JobMessage:
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
		if len(jobMsg.Tools) > 0 {
			toolNames := make([]string, len(jobMsg.Tools))
			for i, t := range jobMsg.Tools {
				toolNames[i] = t.Function.Name
			}
			payload["tools"] = toolNames
		}
		payload["num_messages"] = len(jobMsg.Messages)
		return payload

	case protocol.JobToolCallResultMessage:
		out := map[string]any{
			"type":     jobMsg.Type,
			"job_id":   jobMsg.JobID,
			"call_id":  jobMsg.CallID,
			"result":   redactImageDataURIs(jobMsg.Result),
			"is_error": jobMsg.IsError,
		}
		if parts := redactImagePartsForLog(jobMsg.ImageParts); parts != nil {
			out["image_parts"] = parts
		}
		return out

	case protocol.BuiltinToolMessage:
		return map[string]any{
			"type":      jobMsg.Type,
			"job_id":    jobMsg.JobID,
			"id":        jobMsg.ID,
			"name":      jobMsg.Name,
			"arguments": redactImageDataURIs(jobMsg.Args),
			"result":    redactImageDataURIs(jobMsg.Result),
		}

	case protocol.ToolResultMessage:
		m := map[string]any{
			"type":          jobMsg.Type,
			"job_id":        jobMsg.JobID,
			"parent_job_id": jobMsg.ParentJobID,
			"tool_call_id":  jobMsg.ToolCallID,
			"tool_name":     jobMsg.ToolName,
			"result":        redactImageDataURIs(string(jobMsg.Result)),
			"error":         jobMsg.Error,
			"duration_ms":   jobMsg.DurationMS,
		}
		if parts := redactImagePartsForLog(jobMsg.ImageParts); parts != nil {
			m["image_parts"] = parts
		}
		return m

	case protocol.AgentToolResultMessage:
		return map[string]any{
			"type":         jobMsg.Type,
			"job_id":       jobMsg.JobID,
			"tool_call_id": jobMsg.ToolCallID,
			"content":      redactImageDataURIs(jobMsg.Content),
			"is_error":     jobMsg.IsError,
		}

	case protocol.AgentLocalToolResultMessage:
		return map[string]any{
			"type":   jobMsg.Type,
			"call_id": jobMsg.CallID,
			"result": redactImageDataURIs(jobMsg.Result),
			"error":  jobMsg.Error,
		}

	case protocol.AgentFileUploadMessage:
		return map[string]any{
			"type":           jobMsg.Type,
			"correlation_id": jobMsg.CorrelationID,
			"file_name":      jobMsg.FileName,
			"mime_type":      jobMsg.MimeType,
			"data":           fmt.Sprintf("<redacted %d bytes>", len(jobMsg.Data)),
		}

	default:
		return msg
	}
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
