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

	"github.com/auxothq/auxot/pkg/tools"
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
	return &Sidecar{
		port:    port,
		baseURL: "http://127.0.0.1:" + strconv.Itoa(port),
		logger:  logger,
	}, nil
}

// Start launches the Playwright MCP process and waits until it is ready (up to 30 s).
// Ready means a GET <baseURL>/sse returns HTTP 200.
func (s *Sidecar) Start(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	cmd := exec.CommandContext(ctx,
		tools.BunBinary(),
		"x", "@playwright/mcp@latest",
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

	// Poll GET /sse every 500 ms for up to 30 s.
	probeClient := &http.Client{Timeout: 2 * time.Second}
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		resp, err := probeClient.Get(s.baseURL + "/sse")
		if err == nil {
			if resp.StatusCode < 200 || resp.StatusCode >= 300 {
				// Non-2xx: sidecar not ready yet — drain and continue polling.
				_ = resp.Body.Close()
			} else {
				_ = resp.Body.Close()
				s.logger.Info("browser sidecar ready", "port", s.port)
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
