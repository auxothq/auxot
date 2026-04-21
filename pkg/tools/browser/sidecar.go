// Package browser manages a long-lived Playwright MCP sidecar process and a
// per-thread SSE session registry with idle-TTL eviction.
package browser

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/exec"
	"strconv"
	"sync"
	"time"

)

// Sidecar manages a single long-lived @playwright/mcp HTTP process.
type Sidecar struct {
	port    int
	cmd     *exec.Cmd
	baseURL string
	logger  *slog.Logger
	mu      sync.Mutex
}

// NewSidecar creates (but does not start) a sidecar that will listen on port.
// If port is 0 a free TCP port is selected automatically.
func NewSidecar(port int, logger *slog.Logger) (*Sidecar, error) {
	if port == 0 {
		ln, err := net.Listen("tcp", ":0")
		if err != nil {
			return nil, fmt.Errorf("browser sidecar: selecting free port: %w", err)
		}
		port = ln.Addr().(*net.TCPAddr).Port
		if err := ln.Close(); err != nil {
			return nil, fmt.Errorf("browser sidecar: closing probe listener: %w", err)
		}
	}
	// Use localhost (not 127.0.0.1) so the readiness probe resolves correctly
	// regardless of whether the process binds to IPv4 or IPv6 loopback.
	return &Sidecar{
		port:    port,
		baseURL: "http://localhost:" + strconv.Itoa(port),
		logger:  logger,
	}, nil
}

// Start launches the Playwright MCP process and waits until it is ready (up to 30 s).
// Ready means a GET <baseURL>/sse returns HTTP 200.
func (s *Sidecar) Start(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	// --browser chromium: use the Playwright-managed Chromium build from
	//   PLAYWRIGHT_BROWSERS_PATH (pre-installed in the Docker image).
	// --headless: required in Docker (no display server).
	// --no-sandbox: required when running as non-root inside a container.
	// --isolated: each HTTP client (MCP session) gets an ephemeral browser
	//   context in memory — no persistent user-data-dir, no SingletonLock
	//   conflicts between concurrent sessions.
	// Run playwright-mcp under the real Node.js binary (not Bun).
	// Playwright's internal `ws` npm package has a WebSocket hang bug when
	// executed inside Bun's Node.js compatibility layer; real Node.js is fine.
	// @playwright/mcp is globally installed via npm in Dockerfile.tools.
	//
	// Flags:
	//   --browser chromium: use Playwright's own Chromium from PLAYWRIGHT_BROWSERS_PATH.
	//   --headless: required in Docker (no display server).
	//   --no-sandbox: required when running as non-root inside a container.
	//   --isolated: each MCP session gets its own ephemeral Chrome process + temp
	//     profile dir; prevents "browser already in use" conflicts between threads.
	//   --host 127.0.0.1: bind to IPv4 loopback only; keeps the CSRF host-check
	//     simple and avoids IPv6/IPv4 ambiguity when clients connect via localhost.
	cmd := exec.CommandContext(ctx,
		"node",
		"/usr/local/lib/node_modules/@playwright/mcp/cli.js",
		"--browser", "chromium",
		"--headless",
		"--no-sandbox",
		"--isolated",
		"--host", "127.0.0.1",
		"--port", strconv.Itoa(s.port),
	)
	// Inherit the full host environment so PLAYWRIGHT_CHROMIUM_EXECUTABLE_PATH
	// and any other vars set in the Docker image are visible to the subprocess.
	cmd.Env = os.Environ()

	stdoutPipe, err := cmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("browser sidecar: stdout pipe: %w", err)
	}
	stderrPipe, err := cmd.StderrPipe()
	if err != nil {
		return fmt.Errorf("browser sidecar: stderr pipe: %w", err)
	}

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("browser sidecar: starting process: %w", err)
	}
	s.cmd = cmd

	go s.pipeToLogger(stdoutPipe, "stdout")
	go s.pipeToLogger(stderrPipe, "stderr")

	// Poll for readiness every 500 ms for up to 30 s.
	// Use ResponseHeaderTimeout (not Timeout) so SSE streams don't stall the
	// probe — we only need the server to respond with any HTTP status.
	probeClient := &http.Client{
		Transport: &http.Transport{
			ResponseHeaderTimeout: 2 * time.Second,
		},
	}
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		// Try /sse (legacy SSE transport) first, then / as a fallback.
		// Any HTTP response — including 404 — confirms the server is up.
		for _, path := range []string{"/sse", "/"} {
			resp, err := probeClient.Get(s.baseURL + path)
			if err == nil {
				_ = resp.Body.Close()
				s.logger.Info("browser sidecar ready", "port", s.port, "status", resp.StatusCode)
				return nil
			}
		}
		select {
		case <-ctx.Done():
			_ = cmd.Process.Kill()
			return fmt.Errorf("browser sidecar: startup cancelled: %w", ctx.Err())
		case <-time.After(500 * time.Millisecond):
		}
	}

	_ = cmd.Process.Kill()
	return fmt.Errorf("browser sidecar: did not become ready within 30 s on port %d", s.port)
}

// BaseURL returns "http://127.0.0.1:<port>" so callers can build /sse and /messages URLs.
func (s *Sidecar) BaseURL() string { return s.baseURL }

// Stop sends SIGINT (graceful shutdown signal) to the process and waits up to
// 10 s for it to exit, then kills it forcefully.
func (s *Sidecar) Stop() {
	s.mu.Lock()
	cmd := s.cmd
	s.mu.Unlock()

	if cmd == nil || cmd.Process == nil {
		return
	}

	_ = cmd.Process.Signal(os.Interrupt)

	exited := make(chan struct{})
	go func() {
		_ = cmd.Wait()
		close(exited)
	}()

	select {
	case <-exited:
	case <-time.After(10 * time.Second):
		_ = cmd.Process.Kill()
	}
}

func (s *Sidecar) pipeToLogger(r io.Reader, stream string) {
	scanner := bufio.NewScanner(r)
	for scanner.Scan() {
		s.logger.Debug("browser sidecar output", "stream", stream, "msg", scanner.Text())
	}
}
