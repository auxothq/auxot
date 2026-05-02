package codingtools

import (
	"encoding/base64"
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

// TestReadImagePNG verifies that reading a .png file returns a vision data URI
// instead of raw line-numbered text, proving the image pipeline is wired.
func TestReadImagePNG(t *testing.T) {
	dir := t.TempDir()
	// Minimal 1×1 red pixel PNG (67 bytes, valid PNG header).
	pngBytes := []byte{
		0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a, // PNG signature
		0x00, 0x00, 0x00, 0x0d, 0x49, 0x48, 0x44, 0x52, // IHDR chunk
		0x00, 0x00, 0x00, 0x01, 0x00, 0x00, 0x00, 0x01,
		0x08, 0x02, 0x00, 0x00, 0x00, 0x90, 0x77, 0x53,
		0xde, 0x00, 0x00, 0x00, 0x0c, 0x49, 0x44, 0x41, // IDAT chunk
		0x54, 0x08, 0xd7, 0x63, 0xf8, 0xcf, 0xc0, 0x00,
		0x00, 0x00, 0x02, 0x00, 0x01, 0xe2, 0x21, 0xbc,
		0x33, 0x00, 0x00, 0x00, 0x00, 0x49, 0x45, 0x4e, // IEND chunk
		0x44, 0xae, 0x42, 0x60, 0x82,
	}
	if err := os.WriteFile(dir+"/pixel.png", pngBytes, 0o644); err != nil {
		t.Fatal(err)
	}

	raw, _ := json.Marshal(map[string]string{"path": "pixel.png"})
	out, err := executeRead(dir, raw)
	if err != nil {
		t.Fatal(err)
	}

	if !strings.HasPrefix(out, "data:image/png;base64,") {
		t.Fatalf("expected data URI prefix, got: %q", out[:min(len(out), 80)])
	}

	// Verify the base64 payload round-trips back to the original bytes.
	b64Part := strings.TrimPrefix(out, "data:image/png;base64,")
	decoded, err := base64.StdEncoding.DecodeString(b64Part)
	if err != nil {
		t.Fatalf("base64 decode failed: %v", err)
	}
	if string(decoded) != string(pngBytes) {
		t.Fatalf("decoded bytes do not match original PNG")
	}
}

// TestReadImageJPEG verifies .jpg extension is mapped to image/jpeg.
func TestReadImageJPEG(t *testing.T) {
	dir := t.TempDir()
	// Minimal JFIF JPEG header (enough bytes to write a valid-looking file).
	jpegBytes := []byte{0xff, 0xd8, 0xff, 0xe0, 0x00, 0x10, 0x4a, 0x46, 0x49, 0x46}
	if err := os.WriteFile(dir+"/photo.jpg", jpegBytes, 0o644); err != nil {
		t.Fatal(err)
	}

	raw, _ := json.Marshal(map[string]string{"path": "photo.jpg"})
	out, err := executeRead(dir, raw)
	if err != nil {
		t.Fatal(err)
	}

	if !strings.HasPrefix(out, "data:image/jpeg;base64,") {
		t.Fatalf("expected data:image/jpeg;base64, prefix, got: %q", out[:min(len(out), 80)])
	}
}

// TestReadNonImageStillWorks verifies that text files continue to use line mode.
func TestReadNonImageStillWorks(t *testing.T) {
	dir := t.TempDir()
	writeTestFile(t, dir+"/notes.txt", "hello\nworld\n")

	raw, _ := json.Marshal(map[string]string{"path": "notes.txt"})
	out, err := executeRead(dir, raw)
	if err != nil {
		t.Fatal(err)
	}

	if strings.HasPrefix(out, "data:") {
		t.Fatalf("text file should not produce a data URI, got: %q", out[:min(len(out), 80)])
	}
	if !strings.Contains(out, "1|hello") {
		t.Fatalf("expected line-numbered output, got: %q", out)
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

