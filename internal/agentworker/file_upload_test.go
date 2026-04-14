package agentworker

import (
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"github.com/auxothq/auxot/pkg/protocol"
)

// newTestWSPair creates a connected pair of gorilla WebSocket connections for
// testing. workerConn is the side the Worker writes acks to; serverConn is
// what reads those acks to verify behaviour.
func newTestWSPair(t *testing.T) (workerConn, serverConn *websocket.Conn) {
	t.Helper()

	upgrader := websocket.Upgrader{
		CheckOrigin: func(r *http.Request) bool { return true },
	}
	ch := make(chan *websocket.Conn, 1)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			t.Errorf("WS upgrade: %v", err)
			return
		}
		ch <- c
	}))
	t.Cleanup(srv.Close)

	wsURL := "ws" + srv.URL[4:]
	wc, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		t.Fatalf("dial test WS: %v", err)
	}
	t.Cleanup(func() { wc.Close() })

	sc := <-ch
	t.Cleanup(func() { sc.Close() })

	return wc, sc
}

// newTestWorker creates a Worker with its conn pre-set for testing.
func newTestWorker(t *testing.T, dir string, conn *websocket.Conn) *Worker {
	t.Helper()
	w := &Worker{
		cfg:    Config{Dir: dir},
		logger: slog.Default(),
	}
	w.mu.Lock()
	w.conn = conn
	w.mu.Unlock()
	return w
}

// readUploadAck reads one AgentFileUploadAckMessage from serverConn.
func readUploadAck(t *testing.T, serverConn *websocket.Conn) protocol.AgentFileUploadAckMessage {
	t.Helper()
	var ack protocol.AgentFileUploadAckMessage
	if err := serverConn.ReadJSON(&ack); err != nil {
		t.Fatalf("read upload ack: %v", err)
	}
	return ack
}

func TestHandleFileUpload_Success(t *testing.T) {
	workerConn, serverConn := newTestWSPair(t)
	dir := t.TempDir()
	w := newTestWorker(t, dir, workerConn)

	const payload = "hello, plain world"
	msg := protocol.AgentFileUploadMessage{
		Type:          protocol.TypeFileUpload,
		CorrelationID: "corr-success-1",
		FileName:      "hello.txt",
		MimeType:      "text/plain",
		Data:          []byte(payload),
	}

	go w.handleFileUpload(msg)

	ack := readUploadAck(t, serverConn)

	if ack.CorrelationID != msg.CorrelationID {
		t.Errorf("CorrelationID = %q, want %q", ack.CorrelationID, msg.CorrelationID)
	}
	if ack.Error != "" {
		t.Errorf("unexpected error in ack: %s", ack.Error)
	}
	if ack.ResolvedPath == "" {
		t.Fatal("ResolvedPath is empty")
	}

	data, err := os.ReadFile(ack.ResolvedPath)
	if err != nil {
		t.Fatalf("read written file: %v", err)
	}
	if string(data) != payload {
		t.Errorf("file content = %q, want %q", data, payload)
	}

	expectedPrefix := filepath.Join(dir, "tmp")
	if !strings.HasPrefix(ack.ResolvedPath, expectedPrefix) {
		t.Errorf("ResolvedPath %q not under %q", ack.ResolvedPath, expectedPrefix)
	}
}

func TestHandleFileUpload_Cleanup(t *testing.T) {
	workerConn, serverConn := newTestWSPair(t)
	dir := t.TempDir()
	tmpDir := filepath.Join(dir, "tmp")
	if err := os.MkdirAll(tmpDir, 0755); err != nil {
		t.Fatalf("mkdir tmp: %v", err)
	}

	// 8-day-old file — must be deleted during cleanup.
	oldFile := filepath.Join(tmpDir, "stale-file.txt")
	if err := os.WriteFile(oldFile, []byte("stale"), 0644); err != nil {
		t.Fatalf("write stale file: %v", err)
	}
	oldMtime := time.Now().Add(-8 * 24 * time.Hour)
	if err := os.Chtimes(oldFile, oldMtime, oldMtime); err != nil {
		t.Fatalf("chtimes stale: %v", err)
	}

	// 1-day-old file — must survive.
	recentFile := filepath.Join(tmpDir, "recent-file.txt")
	if err := os.WriteFile(recentFile, []byte("fresh"), 0644); err != nil {
		t.Fatalf("write recent file: %v", err)
	}
	recentMtime := time.Now().Add(-1 * 24 * time.Hour)
	if err := os.Chtimes(recentFile, recentMtime, recentMtime); err != nil {
		t.Fatalf("chtimes recent: %v", err)
	}

	w := newTestWorker(t, dir, workerConn)
	msg := protocol.AgentFileUploadMessage{
		Type:          protocol.TypeFileUpload,
		CorrelationID: "corr-cleanup-1",
		FileName:      "trigger.txt",
		Data:          []byte("trigger"),
	}

	go w.handleFileUpload(msg)

	ack := readUploadAck(t, serverConn)
	if ack.Error != "" {
		t.Fatalf("unexpected error ack: %s", ack.Error)
	}

	if _, err := os.Stat(oldFile); !os.IsNotExist(err) {
		t.Errorf("stale file should have been deleted; stat err = %v", err)
	}

	if _, err := os.Stat(recentFile); err != nil {
		t.Errorf("recent file should still exist: %v", err)
	}
}

func TestHandleFileUpload_OversizedPayload(t *testing.T) {
	prevLimit := maxUploadBytes
	maxUploadBytes = 1024
	t.Cleanup(func() { maxUploadBytes = prevLimit })

	workerConn, serverConn := newTestWSPair(t)
	dir := t.TempDir()
	w := newTestWorker(t, dir, workerConn)

	// One byte over the lowered test limit — avoids allocating ~100 MB.
	largeData := make([]byte, 1025)
	msg := protocol.AgentFileUploadMessage{
		Type:          protocol.TypeFileUpload,
		CorrelationID: "corr-oversize-1",
		FileName:      "huge.bin",
		Data:          largeData,
	}

	go w.handleFileUpload(msg)

	ack := readUploadAck(t, serverConn)

	if ack.Error == "" {
		t.Error("expected error ack for oversized payload, got none")
	}
	if ack.ResolvedPath != "" {
		t.Errorf("ResolvedPath should be empty for oversized payload, got %q", ack.ResolvedPath)
	}

	// No files should have been written to the tmp directory.
	tmpDir := filepath.Join(dir, "tmp")
	entries, err := os.ReadDir(tmpDir)
	if err == nil && len(entries) > 0 {
		t.Errorf("tmp dir should be empty after oversized rejection, has %d entries", len(entries))
	}
}

// ── Unit tests for helper functions ──────────────────────────────────────────

func TestSanitizeUploadFileName(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"hello.txt", "hello.txt"},
		{"../etc/passwd", "..etcpasswd"},
		{"foo/bar/baz.pdf", "foobarbaz.pdf"},
		{`C:\Windows\System32\evil.exe`, "C:WindowsSystem32evil.exe"},
		{"", "upload"},
		{"   ", "   "},
		{strings.Repeat("a", 300), strings.Repeat("a", 200)},
	}
	for _, tc := range cases {
		got := sanitizeUploadFileName(tc.in)
		if got != tc.want {
			t.Errorf("sanitizeUploadFileName(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}
