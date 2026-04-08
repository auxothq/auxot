package codingtools

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"
)

// writeTestFile is a test helper that writes content to a file at the given absolute path.
func writeTestFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestReadLineModeDefaultCap verifies that reading a 200-line file with no
// limit returns exactly defaultReadLines lines plus the continuation trailer.
func TestReadLineModeDefaultCap(t *testing.T) {
	dir := t.TempDir()
	var sb strings.Builder
	for i := 1; i <= 200; i++ {
		fmt.Fprintf(&sb, "line%d\n", i)
	}
	writeTestFile(t, dir+"/big.txt", sb.String())

	raw, _ := json.Marshal(map[string]string{"path": "big.txt"})
	out, err := executeRead(dir, raw)
	if err != nil {
		t.Fatal(err)
	}

	// Each of the 100 content lines ends with \n → 100 newlines in the body.
	lineCount := strings.Count(out, "\n")
	if lineCount != defaultReadLines {
		t.Fatalf("want %d newlines (= %d content lines), got %d\noutput: %q",
			defaultReadLines, defaultReadLines, lineCount, out[:min(len(out), 300)])
	}
	if !strings.Contains(out, "output capped at 100 lines") {
		t.Fatalf("expected trailer, suffix: %q", out[max(0, len(out)-200):])
	}
	if !strings.Contains(out, "use offset_line/limit_lines") {
		t.Fatalf("trailer missing usage hint: %q", out[max(0, len(out)-200):])
	}
}

// TestReadLineModeExplicitLimit verifies that an explicit limit_lines returns
// that many lines without appending the trailer.
func TestReadLineModeExplicitLimit(t *testing.T) {
	dir := t.TempDir()
	var sb strings.Builder
	for i := 1; i <= 300; i++ {
		fmt.Fprintf(&sb, "line%d\n", i)
	}
	writeTestFile(t, dir+"/big.txt", sb.String())

	type reqArgs struct {
		Path       string `json:"path"`
		LimitLines int    `json:"limit_lines"`
	}
	raw, _ := json.Marshal(reqArgs{Path: "big.txt", LimitLines: 200})
	out, err := executeRead(dir, raw)
	if err != nil {
		t.Fatal(err)
	}

	lineCount := strings.Count(out, "\n")
	if lineCount != 200 {
		t.Fatalf("want 200 newlines, got %d", lineCount)
	}
	if strings.Contains(out, "output capped") {
		t.Fatalf("unexpected trailer for explicit limit, suffix: %q", out[max(0, len(out)-200):])
	}
	if !strings.Contains(out, "200|line200") {
		t.Fatalf("expected last line to be '200|line200', suffix: %q", out[max(0, len(out)-100):])
	}
}

// TestReadByteModeExact verifies that limit_bytes=512 returns exactly 512 bytes
// of content and includes the correct continuation hint.
func TestReadByteModeExact(t *testing.T) {
	dir := t.TempDir()
	// Create a file with 1024 known bytes (no newlines).
	content := strings.Repeat("X", 1024)
	writeTestFile(t, dir+"/file.bin", content)

	type reqArgs struct {
		Path       string `json:"path"`
		LimitBytes int    `json:"limit_bytes"`
	}
	// LimitBytes > 0 activates byte mode; OffsetByte defaults to 0.
	raw, _ := json.Marshal(reqArgs{Path: "file.bin", LimitBytes: 512})
	out, err := executeRead(dir, raw)
	if err != nil {
		t.Fatal(err)
	}

	// Content portion is 512 'X's; continuation hint follows on a new line.
	if !strings.HasPrefix(out, strings.Repeat("X", 512)) {
		t.Fatalf("expected 512 X's at start, got: %q", out[:min(len(out), 60)])
	}
	if !strings.Contains(out, "use offset_byte=512") {
		t.Fatalf("expected continuation hint with offset_byte=512, got suffix: %q", out[max(0, len(out)-200):])
	}
	if !strings.Contains(out, "of 1024") {
		t.Fatalf("expected total byte count 1024 in hint, got suffix: %q", out[max(0, len(out)-200):])
	}
}

// TestReadByteModeOneLongLine verifies that a file consisting of a single line
// longer than defaultReadBytes returns exactly defaultReadBytes bytes with a
// continuation hint when byte mode is requested.
func TestReadByteModeOneLongLine(t *testing.T) {
	dir := t.TempDir()
	// 8 KB single-line content (no newlines).
	content := strings.Repeat("Y", 8192)
	writeTestFile(t, dir+"/longline.txt", content)

	type reqArgs struct {
		Path       string `json:"path"`
		LimitBytes int    `json:"limit_bytes"`
	}
	raw, _ := json.Marshal(reqArgs{Path: "longline.txt", LimitBytes: defaultReadBytes})
	out, err := executeRead(dir, raw)
	if err != nil {
		t.Fatal(err)
	}

	// First defaultReadBytes chars must all be 'Y'.
	if !strings.HasPrefix(out, strings.Repeat("Y", defaultReadBytes)) {
		t.Fatalf("expected %d Y's at start, prefix: %q", defaultReadBytes, out[:min(len(out), 60)])
	}
	// Continuation hint must point to the correct next offset.
	expected := fmt.Sprintf("use offset_byte=%d to continue", defaultReadBytes)
	if !strings.Contains(out, expected) {
		t.Fatalf("expected %q in output, suffix: %q", expected, out[max(0, len(out)-200):])
	}
}

// TestReadByteModeNoMoreContent verifies the message when offset_byte is at or
// beyond the end of file.
func TestReadByteModeNoMoreContent(t *testing.T) {
	dir := t.TempDir()
	writeTestFile(t, dir+"/small.txt", "hello")

	type reqArgs struct {
		Path       string `json:"path"`
		OffsetByte int    `json:"offset_byte"`
		LimitBytes int    `json:"limit_bytes"`
	}
	raw, _ := json.Marshal(reqArgs{Path: "small.txt", OffsetByte: 100, LimitBytes: 512})
	out, err := executeRead(dir, raw)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "no more content") {
		t.Fatalf("expected 'no more content' message, got: %q", out)
	}
}

// TestReadLineModeBackwardsCompat verifies that the legacy offset/limit fields
// work identically to offset_line/limit_lines.
func TestReadLineModeBackwardsCompat(t *testing.T) {
	dir := t.TempDir()
	var sb strings.Builder
	for i := 1; i <= 10; i++ {
		fmt.Fprintf(&sb, "row%d\n", i)
	}
	writeTestFile(t, dir+"/file.txt", sb.String())

	type legacyArgs struct {
		Path   string `json:"path"`
		Offset int    `json:"offset"`
		Limit  int    `json:"limit"`
	}
	raw, _ := json.Marshal(legacyArgs{Path: "file.txt", Offset: 3, Limit: 4})
	out, err := executeRead(dir, raw)
	if err != nil {
		t.Fatal(err)
	}
	// Should return lines 3–6: row3, row4, row5, row6.
	for _, want := range []string{"3|row3", "4|row4", "5|row5", "6|row6"} {
		if !strings.Contains(out, want) {
			t.Fatalf("expected %q in output: %q", want, out)
		}
	}
	if strings.Contains(out, "7|") {
		t.Fatalf("unexpected line 7 in output: %q", out)
	}
}

