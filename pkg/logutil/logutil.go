// Package logutil provides shared logging utilities for all Auxot binaries.
//
// Key features:
// - Routes INFO/DEBUG logs to stdout, WARN/ERROR logs to stderr (Unix convention)
// - Auto-detects whether stdout is a TTY and switches between pretty-printed
//   JSON (human at terminal) and compact JSON (piped to a file, log aggregator, CI, etc.)
package logutil

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
)

// isTTY is set once at init time (checks stdout for piping detection).
var isTTY bool

func init() {
	stat, err := os.Stdout.Stat()
	if err == nil {
		isTTY = (stat.Mode() & os.ModeCharDevice) != 0
	}
}

// IsTTY reports whether stdout appears to be a terminal.
func IsTTY() bool {
	return isTTY
}

// Output returns a writer that routes logs by severity level:
// - INFO/DEBUG → stdout (pretty-printed if TTY, compact if piped)
// - WARN/ERROR → stderr (always compact for tooling)
//
// Pass the return value to slog.NewJSONHandler as the destination writer.
func Output(w io.Writer) io.Writer {
	return &levelRoutingWriter{
		stdout: maybeWrapPretty(os.Stdout),
		stderr: os.Stderr, // Always compact for stderr (errors/warnings)
	}
}

// maybeWrapPretty wraps w in a pretty-printer if stdout is a TTY.
func maybeWrapPretty(w io.Writer) io.Writer {
	if !isTTY {
		return w
	}
	return &prettyJSONWriter{w: w}
}

// levelRoutingWriter inspects each log line's "level" field and routes to
// stdout (info/debug) or stderr (warn/error).
type levelRoutingWriter struct {
	stdout io.Writer
	stderr io.Writer
}

func (lw *levelRoutingWriter) Write(p []byte) (int, error) {
	// Parse JSON to extract level field
	var logEntry map[string]interface{}
	if err := json.Unmarshal(p, &logEntry); err != nil {
		// Not valid JSON — send to stderr as a safety measure
		return lw.stderr.Write(p)
	}

	level, ok := logEntry["level"].(string)
	if !ok {
		// No level field — default to stdout
		return lw.stdout.Write(p)
	}

	// Route based on level
	switch level {
	case "WARN", "ERROR":
		return lw.stderr.Write(p)
	default: // INFO, DEBUG, etc.
		return lw.stdout.Write(p)
	}
}

// prettyJSONWriter re-indents each JSON line written to it.
type prettyJSONWriter struct {
	w io.Writer
}

func (pw *prettyJSONWriter) Write(p []byte) (int, error) {
	trimmed := bytes.TrimRight(p, "\n")
	var buf bytes.Buffer
	if err := json.Indent(&buf, trimmed, "", "  "); err != nil {
		// Not valid JSON — pass through unchanged
		return pw.w.Write(p)
	}
	buf.WriteByte('\n')
	_, err := pw.w.Write(buf.Bytes())
	return len(p), err // Return original len to satisfy io.Writer contract
}
