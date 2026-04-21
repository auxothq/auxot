package browser

import (
	"fmt"
	"io"
	"log/slog"
	"strings"
	"testing"
)

func TestSidecar_PortSelection(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	s, err := NewSidecar(0, logger)
	if err != nil {
		t.Fatalf("NewSidecar(0, ...) error: %v", err)
	}
	if s.port == 0 {
		t.Error("expected a non-zero port to be chosen, got 0")
	}
	wantURL := fmt.Sprintf("http://127.0.0.1:%d", s.port)
	if s.BaseURL() != wantURL {
		t.Errorf("BaseURL() = %q, want %q", s.BaseURL(), wantURL)
	}
	if !strings.HasPrefix(s.BaseURL(), "http://127.0.0.1:") {
		t.Errorf("BaseURL() %q should start with http://127.0.0.1:", s.BaseURL())
	}
}
