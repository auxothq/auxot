//go:build integration

package browser

// Integration tests for the full Go executor path:
//   PerToolExecutor.Execute()  →  Registry.Call()  →  real playwright-mcp sidecar
//
// Run with:
//   PLAYWRIGHT_MCP_URL=http://localhost:19999 go test -tags integration -v ./pkg/tools/browser/
//
// Requires PLAYWRIGHT_MCP_URL pointing at a running sidecar (see Dockerfile.tools).
// All tests share a single Registry so sessions are properly reused and isolated
// via distinct thread IDs — exactly as in production.

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/auxothq/auxot/pkg/tools"
)

// integRegistry is a single shared Registry for all integration tests.
// Using one Registry means sessions are handled the same way as production
// (one Registry per tools-worker process, reused across concurrent calls).
var (
	integOnce sync.Once
	integReg  *Registry
)

func sharedRegistry(t *testing.T) *Registry {
	t.Helper()
	integOnce.Do(func() {
		u := os.Getenv("PLAYWRIGHT_MCP_URL")
		if u == "" {
			u = "http://localhost:19999"
		}
		logger := slog.New(slog.NewTextHandler(io.Discard, nil))
		sc := &Sidecar{
			port:    19999,
			baseURL: u,
			logger:  logger,
		}
		integReg = &Registry{
			sidecar:  sc,
			sessions: make(map[string]*Session),
			now:      time.Now,
			logger:   logger,
		}
	})
	return integReg
}

// threadExec returns executor and context for a named integration thread.
// Each test uses a unique thread name so sessions are isolated from each other.
func threadExec(t *testing.T, tool, thread string) (*PerToolExecutor, context.Context) {
	t.Helper()
	return NewPerToolExecutor(tool, sharedRegistry(t)),
		tools.WithThreadID(context.Background(), "integ-"+thread)
}

// TestIntegration_BrowserNavigate calls browser_navigate through the full Go
// executor path and expects a non-error response from the real sidecar.
func TestIntegration_BrowserNavigate(t *testing.T) {
	exec, ctx := threadExec(t, "browser_navigate", "navigate")
	defer closeThread(t, "integ-navigate")

	args, _ := json.Marshal(map[string]string{"url": "about:blank"})
	result, err := exec.Execute(ctx, args)
	if err != nil {
		t.Fatalf("browser_navigate about:blank: %v", err)
	}
	t.Logf("result output: %.200s", result.Output)
}

// TestIntegration_BrowserNavigateRealSite navigates to example.com and verifies
// the snapshot contains identifiable page content — exercises the full browser
// toolchain: navigate → snapshot → verify content.
func TestIntegration_BrowserNavigateRealSite(t *testing.T) {
	const thread = "integ-realsite"
	defer closeThread(t, thread)

	ctx := tools.WithThreadID(context.Background(), thread)
	reg := sharedRegistry(t)
	navExec := NewPerToolExecutor("browser_navigate", reg)
	snapExec := NewPerToolExecutor("browser_snapshot", reg)

	navArgs, _ := json.Marshal(map[string]string{"url": "https://example.com"})
	if _, err := navExec.Execute(ctx, navArgs); err != nil {
		t.Fatalf("browser_navigate example.com: %v", err)
	}

	snapResult, err := snapExec.Execute(ctx, json.RawMessage("{}"))
	if err != nil {
		t.Fatalf("browser_snapshot: %v", err)
	}

	var snapText string
	if err := json.Unmarshal(snapResult.Output, &snapText); err != nil {
		t.Fatalf("browser_snapshot output not a JSON string: %v (raw=%s)", err, snapResult.Output)
	}
	t.Logf("browser_snapshot excerpt: %.200s", snapText)

	if !strings.Contains(strings.ToLower(snapText), "example") {
		t.Errorf("snapshot does not mention 'example'; got: %.300s", snapText)
	}
}

// TestIntegration_BrowserTakeScreenshot calls browser_take_screenshot and verifies
// a valid PNG image is returned in tools.Result.ImageParts.
func TestIntegration_BrowserTakeScreenshot(t *testing.T) {
	const thread = "integ-screenshot"
	defer closeThread(t, thread)

	ctx := tools.WithThreadID(context.Background(), thread)
	reg := sharedRegistry(t)
	navExec := NewPerToolExecutor("browser_navigate", reg)
	shotExec := NewPerToolExecutor("browser_take_screenshot", reg)

	navArgs, _ := json.Marshal(map[string]string{"url": "https://example.com"})
	if _, err := navExec.Execute(ctx, navArgs); err != nil {
		t.Fatalf("browser_navigate example.com: %v", err)
	}

	shotResult, err := shotExec.Execute(ctx, json.RawMessage("{}"))
	if err != nil {
		t.Fatalf("browser_take_screenshot: %v", err)
	}
	if len(shotResult.ImageParts) == 0 {
		t.Fatal("browser_take_screenshot returned no ImageParts")
	}

	img := shotResult.ImageParts[0]
	if img.MIMEType != "image/png" {
		t.Errorf("ImageParts[0].MIMEType = %q, want image/png", img.MIMEType)
	}
	if len(img.Data) < 100 {
		t.Errorf("ImageParts[0].Data suspiciously small: %d bytes", len(img.Data))
	}
	// PNG magic bytes: 0x89 0x50 0x4E 0x47
	if img.Data[0] != 0x89 || img.Data[1] != 0x50 {
		t.Errorf("ImageParts[0].Data does not look like a PNG (magic=%02x %02x)", img.Data[0], img.Data[1])
	}
	t.Logf("browser_take_screenshot: mimeType=%s size=%d bytes", img.MIMEType, len(img.Data))
}

// TestIntegration_AllowedToolsExistInSidecar asks the sidecar for its tools/list
// and verifies every tool in AllowedTools is actually offered by the real sidecar.
// This catches mismatches between our allowlist and whatever @playwright/mcp ships.
func TestIntegration_AllowedToolsExistInSidecar(t *testing.T) {
	const thread = "integ-toolslist"
	defer closeThread(t, thread)

	reg := sharedRegistry(t)
	ctx := tools.WithThreadID(context.Background(), thread)

	sess, err := reg.GetOrCreate(thread)
	if err != nil {
		t.Fatalf("GetOrCreate: %v", err)
	}

	raw, err := reg.Call(ctx, sess, "tools/list", map[string]any{})
	if err != nil {
		t.Fatalf("tools/list: %v", err)
	}

	var listResult struct {
		Tools []struct {
			Name string `json:"name"`
		} `json:"tools"`
	}
	if err := json.Unmarshal(raw, &listResult); err != nil {
		t.Fatalf("parsing tools/list response: %v", err)
	}

	sidecarTools := make(map[string]bool, len(listResult.Tools))
	for _, tool := range listResult.Tools {
		sidecarTools[tool.Name] = true
	}
	t.Logf("sidecar has %d tools", len(listResult.Tools))

	var missing []string
	for _, name := range AllowedToolNames() {
		if !sidecarTools[name] {
			missing = append(missing, name)
		}
	}
	if len(missing) > 0 {
		t.Errorf("AllowedTools names not found in real sidecar tools/list: %v", missing)
	}
}

// TestIntegration_SessionIsolation verifies that two different thread IDs get
// independent browser state — navigating one does not affect the other.
func TestIntegration_SessionIsolation(t *testing.T) {
	const threadA = "integ-iso-A"
	const threadB = "integ-iso-B"
	defer closeThread(t, threadA)
	defer closeThread(t, threadB)

	reg := sharedRegistry(t)
	ctxA := tools.WithThreadID(context.Background(), threadA)
	ctxB := tools.WithThreadID(context.Background(), threadB)

	navExec := NewPerToolExecutor("browser_navigate", reg)
	snapExec := NewPerToolExecutor("browser_snapshot", reg)

	// Navigate thread A to example.com.
	navArgs, _ := json.Marshal(map[string]string{"url": "https://example.com"})
	if _, err := navExec.Execute(ctxA, navArgs); err != nil {
		t.Fatalf("thread A browser_navigate: %v", err)
	}

	// Navigate thread B to about:blank (different page).
	blankArgs, _ := json.Marshal(map[string]string{"url": "about:blank"})
	if _, err := navExec.Execute(ctxB, blankArgs); err != nil {
		t.Fatalf("thread B browser_navigate: %v", err)
	}

	// Snapshot thread A — should mention "example".
	snapA, err := snapExec.Execute(ctxA, json.RawMessage("{}"))
	if err != nil {
		t.Fatalf("thread A browser_snapshot: %v", err)
	}
	var textA string
	_ = json.Unmarshal(snapA.Output, &textA)
	t.Logf("thread A snapshot excerpt: %.150s", textA)

	// Snapshot thread B — should NOT mention "Example Domain" (it's on about:blank).
	snapB, err := snapExec.Execute(ctxB, json.RawMessage("{}"))
	if err != nil {
		t.Fatalf("thread B browser_snapshot: %v", err)
	}
	var textB string
	_ = json.Unmarshal(snapB.Output, &textB)
	t.Logf("thread B snapshot excerpt: %.150s", textB)

	if !strings.Contains(strings.ToLower(textA), "example") {
		t.Error("thread A snapshot should contain 'example' (navigated to example.com)")
	}
	if strings.Contains(strings.ToLower(textB), "example domain") {
		t.Error("thread B snapshot should NOT contain 'Example Domain' — session leaked from thread A")
	}
}

// closeThread removes a session from the shared registry after a test completes.
func closeThread(t *testing.T, threadID string) {
	t.Helper()
	reg := sharedRegistry(t)
	reg.mu.Lock()
	reg.closeSession(threadID)
	reg.mu.Unlock()
}
