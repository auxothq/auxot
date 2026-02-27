// Package worker manages the stable-diffusion.cpp sd-server subprocess for
// image generation models (FLUX, SD3.5, etc.).
package worker

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"
)

// SDProcess manages the stable-diffusion.cpp sd-server subprocess.
type SDProcess struct {
	cmd     *exec.Cmd
	mu      sync.Mutex
	running bool
	port    int
	opts    SDOpts
	logger  *slog.Logger

	stderrCapture *sdStderrCapture
	onCrash       func(exitCode int, err error)
}

// SDOpts holds the sd-server launch parameters.
type SDOpts struct {
	BinaryPath      string
	DiffusionModel  string // Path to diffusion model GGUF (required for FLUX, SD3.5, etc.)
	VAEPath         string // Optional: path to VAE (required for FLUX.2-klein, FLUX.1-schnell)
	LLMPath         string // Optional: path to LLM text encoder (required for FLUX.2-klein)
	ClipLPath       string // Optional: path to clip_l (required for FLUX.1-schnell)
	T5xxlPath       string // Optional: path to t5xxl (required for FLUX.1-schnell)
	Port            int
	Host            string
	Prediction      string // e.g. "flux2_flow" for FLUX.2-klein, "flux_flow" for FLUX.1-schnell
	OffloadToCPU    bool
	DiffusionFlash  bool
}

// NewSDProcess creates an SDProcess manager.
func NewSDProcess(opts SDOpts, logger *slog.Logger) *SDProcess {
	if opts.Host == "" {
		opts.Host = "127.0.0.1"
	}
	return &SDProcess{
		opts:   opts,
		port:   opts.Port,
		logger: logger,
	}
}

// OnCrash registers a callback invoked when sd-server exits unexpectedly.
func (sd *SDProcess) OnCrash(fn func(exitCode int, err error)) {
	sd.onCrash = fn
}

// Start spawns the sd-server process.
func (sd *SDProcess) Start() error {
	sd.mu.Lock()
	defer sd.mu.Unlock()

	if sd.running {
		return fmt.Errorf("sd-server already running")
	}

	args := []string{
		"--diffusion-model", sd.opts.DiffusionModel,
		"--listen-port", strconv.Itoa(sd.opts.Port),
		"--listen-ip", sd.opts.Host,
	}
	if sd.opts.VAEPath != "" {
		args = append(args, "--vae", sd.opts.VAEPath)
	}
	if sd.opts.LLMPath != "" {
		args = append(args, "--llm", sd.opts.LLMPath)
	}
	if sd.opts.ClipLPath != "" {
		args = append(args, "--clip_l", sd.opts.ClipLPath)
	}
	if sd.opts.T5xxlPath != "" {
		args = append(args, "--t5xxl", sd.opts.T5xxlPath)
	}
	if sd.opts.Prediction != "" {
		args = append(args, "--prediction", sd.opts.Prediction)
	}
	if sd.opts.OffloadToCPU {
		args = append(args, "--offload-to-cpu")
	}
	if sd.opts.DiffusionFlash {
		args = append(args, "--diffusion-fa")
	}

	sd.logger.Info("spawning sd-server",
		"binary", sd.opts.BinaryPath,
		"diffusion_model", sd.opts.DiffusionModel,
		"port", sd.opts.Port,
	)

	sd.cmd = exec.Command(sd.opts.BinaryPath, args...)
	env := os.Environ()
	// macOS: stable-diffusion.cpp release has @rpath baked to the build machine.
	// Prepend the binary's directory to DYLD_LIBRARY_PATH so libstable-diffusion.dylib is found.
	if runtime.GOOS == "darwin" {
		binDir := filepath.Dir(sd.opts.BinaryPath)
		prefix := "DYLD_LIBRARY_PATH=" + binDir
		for i, e := range env {
			if strings.HasPrefix(e, "DYLD_LIBRARY_PATH=") {
				prefix += ":" + strings.TrimPrefix(e, "DYLD_LIBRARY_PATH=")
				env = append(env[:i], env[i+1:]...)
				break
			}
		}
		env = append([]string{prefix}, env...)
	}
	sd.cmd.Env = env

	sd.stderrCapture = newSDStderrCapture(sd.logger)
	if DebugLevel() >= 2 {
		sd.cmd.Stdout = os.Stdout
		sd.cmd.Stderr = io.MultiWriter(os.Stderr, sd.stderrCapture)
	} else {
		sd.cmd.Stdout = io.Discard
		sd.cmd.Stderr = sd.stderrCapture
	}

	if err := sd.cmd.Start(); err != nil {
		return fmt.Errorf("starting sd-server: %w", err)
	}

	sd.running = true
	go sd.monitor()
	return nil
}

func (sd *SDProcess) monitor() {
	err := sd.cmd.Wait()

	sd.mu.Lock()
	wasRunning := sd.running
	sd.running = false
	sd.mu.Unlock()

	if !wasRunning {
		return
	}

	exitCode := -1
	if sd.cmd.ProcessState != nil {
		exitCode = sd.cmd.ProcessState.ExitCode()
	}

	recentStderr := ""
	if sd.stderrCapture != nil {
		recentStderr = sd.stderrCapture.RecentLines(20)
	}

	attrs := []any{"exit_code", exitCode, "error", err}
	if recentStderr != "" {
		attrs = append(attrs, "stderr", recentStderr)
	}
	sd.logger.Error("sd-server exited unexpectedly", attrs...)

	if sd.onCrash != nil {
		sd.onCrash(exitCode, err)
	}
}

// Stop gracefully stops the sd-server process.
func (sd *SDProcess) Stop() error {
	sd.mu.Lock()
	defer sd.mu.Unlock()

	if !sd.running || sd.cmd == nil || sd.cmd.Process == nil {
		return nil
	}

	sd.running = false
	sd.logger.Info("stopping sd-server")

	if err := sd.cmd.Process.Signal(os.Interrupt); err != nil {
		return nil
	}

	done := make(chan struct{})
	go func() {
		_ = sd.cmd.Wait()
		close(done)
	}()

	select {
	case <-done:
		return nil
	case <-time.After(5 * time.Second):
		sd.logger.Warn("sd-server did not exit gracefully, killing")
		return sd.cmd.Process.Kill()
	}
}

// IsRunning reports whether the sd-server process is alive.
func (sd *SDProcess) IsRunning() bool {
	sd.mu.Lock()
	defer sd.mu.Unlock()
	return sd.running
}

// URL returns the HTTP URL of the running sd-server.
func (sd *SDProcess) URL() string {
	return fmt.Sprintf("http://%s:%d", sd.opts.Host, sd.opts.Port)
}

// WaitForReady polls sd-server until it responds to /v1/models or timeout.
func (sd *SDProcess) WaitForReady(ctx context.Context, timeout time.Duration) error {
	deadline := time.After(timeout)
	ticker := time.NewTicker(1 * time.Second)
	defer ticker.Stop()

	url := sd.URL() + "/v1/models"

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-deadline:
			return fmt.Errorf("sd-server did not become ready within %v", timeout)
		case <-ticker.C:
			if !sd.IsRunning() {
				return fmt.Errorf("sd-server process exited before becoming ready")
			}

			resp, err := http.Get(url)
			if err != nil {
				continue
			}

			ct := resp.Header.Get("Content-Type")
			resp.Body.Close()

			if resp.StatusCode == http.StatusOK && strings.Contains(ct, "application/json") {
				return nil
			}
		}
	}
}

// DiscoverCapabilities returns capabilities for the image-gen worker.
// sd-server exposes /v1/models with a minimal response; we build caps from policy.
func (sd *SDProcess) DiscoverCapabilities(modelName, parameters string) *DiscoveredCaps {
	return &DiscoveredCaps{
		Backend:    "stable-diffusion.cpp",
		Model:      modelName,
		CtxSize:    4096, // Not used for image-gen, but protocol expects it
		TotalSlots: 1,    // Image gen is typically single-slot
		Parameters: parameters,
	}
}

type sdStderrCapture struct {
	logger   *slog.Logger
	mu       sync.Mutex
	buf      []byte
	lines    []string
	maxLines int
}

func newSDStderrCapture(logger *slog.Logger) *sdStderrCapture {
	return &sdStderrCapture{
		logger:   logger,
		maxLines: 50,
	}
}

func (c *sdStderrCapture) Write(p []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.buf = append(c.buf, p...)

	for {
		idx := -1
		for i, b := range c.buf {
			if b == '\n' {
				idx = i
				break
			}
		}
		if idx < 0 {
			if len(c.buf) > 4096 {
				c.buf = c.buf[len(c.buf)-2048:]
			}
			break
		}

		line := strings.TrimSpace(string(c.buf[:idx]))
		c.buf = c.buf[idx+1:]

		if line == "" {
			continue
		}

		c.lines = append(c.lines, line)
		if len(c.lines) > c.maxLines {
			c.lines = c.lines[len(c.lines)-c.maxLines:]
		}

		lower := strings.ToLower(line)
		if strings.Contains(lower, "error") ||
			strings.Contains(lower, "fatal") ||
			strings.Contains(lower, "abort") ||
			strings.Contains(lower, "failed") ||
			strings.Contains(lower, "library not loaded") {
			c.logger.Warn("sd-server stderr", "line", line)
		}
	}

	return len(p), nil
}

func (c *sdStderrCapture) RecentLines(n int) string {
	c.mu.Lock()
	defer c.mu.Unlock()

	if len(c.lines) == 0 {
		return ""
	}
	start := 0
	if len(c.lines) > n {
		start = len(c.lines) - n
	}
	return strings.Join(c.lines[start:], "\n")
}
