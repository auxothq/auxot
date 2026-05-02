package codingtools

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

const (
	defaultReadLines = 100
	defaultReadBytes = 4_096       // 4 KB default in byte mode
	maxReadBytes     = 100 * 1024  // 100 KB hard cap in byte mode
	maxImageBytes    = 5 * 1024 * 1024 // 5 MB hard cap for image reads
)

// imageMediaTypes maps lowercase file extensions to their MIME types.
// Any file with one of these extensions is returned as a vision data URI
// instead of raw bytes so the LLM can see it directly.
var imageMediaTypes = map[string]string{
	".png":  "image/png",
	".jpg":  "image/jpeg",
	".jpeg": "image/jpeg",
	".gif":  "image/gif",
	".webp": "image/webp",
	".bmp":  "image/bmp",
	".tiff": "image/tiff",
	".tif":  "image/tiff",
	".ico":  "image/x-icon",
}

// ReadArgs are the arguments for the read tool.
type ReadArgs struct {
	Path string `json:"path"`
	// Line mode (default) — legacy aliases kept for backwards compatibility.
	Offset int `json:"offset,omitempty"`
	Limit  int `json:"limit,omitempty"`
	// Line mode — preferred names.
	OffsetLine int `json:"offset_line,omitempty"`
	LimitLines int `json:"limit_lines,omitempty"`
	// Byte mode — takes precedence when either field is non-zero.
	OffsetByte int `json:"offset_byte,omitempty"`
	LimitBytes int `json:"limit_bytes,omitempty"`
}

// ReadTool returns a Tool that reads files from the agent directory.
func ReadTool() Tool {
	return Tool{
		Name: "read",
		Description: "Read a file. Image files (png, jpg, jpeg, gif, webp, bmp, tiff, ico) are " +
			"returned as inline vision data so the model can see them directly. " +
			"Line mode (default): returns up to 100 lines from offset_line. " +
			"Byte mode: set offset_byte/limit_bytes for binary-safe or large-file reading. " +
			"Output is capped inline — use tool_recall or repeat with offset to read further.",
		Parameters: json.RawMessage(`{
			"type": "object",
			"properties": {
				"path": {
					"type": "string",
					"description": "Path to the file, relative to the agent directory"
				},
				"offset": {
					"type": "integer",
					"description": "Alias for offset_line (backwards compat). 1-indexed line to start from."
				},
				"limit": {
					"type": "integer",
					"description": "Alias for limit_lines (backwards compat). Max lines to return."
				},
				"offset_line": {
					"type": "integer",
					"description": "1-indexed line number to start reading from (line mode)."
				},
				"limit_lines": {
					"type": "integer",
					"description": "Maximum number of lines to return (line mode)."
				},
				"offset_byte": {
					"type": "integer",
					"description": "Byte offset to start reading from (byte mode). Setting this activates byte mode."
				},
				"limit_bytes": {
					"type": "integer",
					"description": "Maximum bytes to return (byte mode, default 4096, max 102400). Setting this activates byte mode."
				}
			},
			"required": ["path"]
		}`),
		Execute: func(ctx context.Context, workDir string, _ map[string]string, args json.RawMessage) (string, error) {
			return executeRead(workDir, args)
		},
	}
}

func executeRead(workDir string, rawArgs json.RawMessage) (string, error) {
	var args ReadArgs
	if err := json.Unmarshal(rawArgs, &args); err != nil {
		return "", fmt.Errorf("invalid read args: %w", err)
	}
	if args.Path == "" {
		return "", fmt.Errorf("path is required")
	}

	absPath, err := safePath(workDir, args.Path)
	if err != nil {
		return "", err
	}

	// Image files are returned as vision data URIs so the LLM can see them.
	if mime := imageMediaType(args.Path); mime != "" {
		return executeReadImage(absPath, args.Path, mime)
	}

	// Byte mode takes precedence when offset_byte > 0 or limit_bytes > 0.
	if args.OffsetByte > 0 || args.LimitBytes > 0 {
		return executeReadBytes(absPath, args)
	}

	return executeReadLines(absPath, args)
}

// imageMediaType returns the MIME type for image files based on extension,
// or empty string if the file is not a recognised image format.
func imageMediaType(path string) string {
	return imageMediaTypes[strings.ToLower(filepath.Ext(path))]
}

// executeReadImage reads an image file, base64-encodes it, and returns a
// data URI that the vision pipeline can extract and forward to the LLM.
func executeReadImage(absPath, origPath, mimeType string) (string, error) {
	fi, err := os.Stat(absPath)
	if err != nil {
		return "", fmt.Errorf("read %s: %w", origPath, err)
	}
	if fi.Size() > maxImageBytes {
		return "", fmt.Errorf("image %s is too large (%d bytes, max %d); use offset_byte/limit_bytes to retrieve chunks",
			origPath, fi.Size(), maxImageBytes)
	}
	data, err := os.ReadFile(absPath)
	if err != nil {
		return "", fmt.Errorf("read %s: %w", origPath, err)
	}
	encoded := base64.StdEncoding.EncodeToString(data)
	return fmt.Sprintf("data:%s;base64,%s", mimeType, encoded), nil
}

func executeReadBytes(absPath string, args ReadArgs) (string, error) {
	f, err := os.Open(absPath)
	if err != nil {
		return "", fmt.Errorf("read %s: %w", args.Path, err)
	}
	defer f.Close()

	fi, err := f.Stat()
	if err != nil {
		return "", fmt.Errorf("stat %s: %w", args.Path, err)
	}
	total := fi.Size()

	limitBytes := args.LimitBytes
	if limitBytes <= 0 {
		limitBytes = defaultReadBytes
	}
	if limitBytes > maxReadBytes {
		limitBytes = maxReadBytes
	}

	start := int64(args.OffsetByte)
	if start >= total {
		return fmt.Sprintf("[showing bytes %d–%d of %d; file has no more content]", start, start, total), nil
	}

	if _, err := f.Seek(start, io.SeekStart); err != nil {
		return "", fmt.Errorf("seek %s: %w", args.Path, err)
	}

	toRead := int64(limitBytes)
	if start+toRead > total {
		toRead = total - start
	}

	buf := make([]byte, toRead)
	n, err := io.ReadFull(f, buf)
	if err != nil && err != io.ErrUnexpectedEOF {
		return "", fmt.Errorf("read %s: %w", args.Path, err)
	}
	buf = buf[:n]

	end := start + int64(n)
	content := string(buf)
	if end < total {
		content += fmt.Sprintf("\n[showing bytes %d–%d of %d; use offset_byte=%d to continue]", start, end, total, end)
	}
	return content, nil
}

func executeReadLines(absPath string, args ReadArgs) (string, error) {
	data, err := os.ReadFile(absPath)
	if err != nil {
		return "", fmt.Errorf("read %s: %w", args.Path, err)
	}

	lines := strings.Split(string(data), "\n")

	// Resolve offset: offset_line takes precedence over legacy offset.
	offsetLine := args.OffsetLine
	if offsetLine == 0 {
		offsetLine = args.Offset
	}

	start := 0
	if offsetLine > 0 {
		start = offsetLine - 1
	}
	if start >= len(lines) {
		return "", fmt.Errorf("offset %d exceeds file length %d lines", offsetLine, len(lines))
	}

	// Resolve limit: limit_lines takes precedence over legacy limit.
	limitLines := args.LimitLines
	if limitLines == 0 {
		limitLines = args.Limit
	}

	usingDefault := false
	if limitLines == 0 {
		limitLines = defaultReadLines
		usingDefault = true
	}

	end := len(lines)
	if start+limitLines < end {
		end = start + limitLines
	}

	selected := lines[start:end]
	var sb strings.Builder
	for i, line := range selected {
		fmt.Fprintf(&sb, "%d|%s\n", start+i+1, line)
	}

	// Append a trailer only when the default cap was applied and more lines exist.
	if usingDefault && end < len(lines) {
		fmt.Fprintf(&sb, "[output capped at %d lines — file has %d total lines; use offset_line/limit_lines or offset_byte/limit_bytes to read further]", defaultReadLines, len(lines))
	}

	return sb.String(), nil
}
