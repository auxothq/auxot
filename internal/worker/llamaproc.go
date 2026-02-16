package worker

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"
)

// LlamaProcess manages the llama.cpp server subprocess.
type LlamaProcess struct {
	cmd     *exec.Cmd
	mu      sync.Mutex
	running bool
	port    int
	opts    LlamaOpts
	logger  *slog.Logger

	// Crash notification
	onCrash func(exitCode int, err error)
}

// LlamaOpts holds the llama.cpp launch parameters.
type LlamaOpts struct {
	BinaryPath  string
	ModelPath   string
	ContextSize int // Per-slot context size (will be multiplied by Parallelism)
	Parallelism int
	Port        int
	Host        string
	GPULayers   int
	Threads     int
}

// NewLlamaProcess creates a LlamaProcess manager.
func NewLlamaProcess(opts LlamaOpts, logger *slog.Logger) *LlamaProcess {
	if opts.Host == "" {
		opts.Host = "127.0.0.1"
	}
	return &LlamaProcess{
		opts:   opts,
		port:   opts.Port,
		logger: logger,
	}
}

// OnCrash registers a callback invoked when llama.cpp exits unexpectedly.
func (lp *LlamaProcess) OnCrash(fn func(exitCode int, err error)) {
	lp.onCrash = fn
}

// Start spawns the llama.cpp server process.
func (lp *LlamaProcess) Start() error {
	lp.mu.Lock()
	defer lp.mu.Unlock()

	if lp.running {
		return fmt.Errorf("llama.cpp already running")
	}

	// CRITICAL: llama.cpp divides ctx-size among parallel slots.
	// So we must multiply contextSize * parallelism to get the desired per-slot context.
	totalCtx := lp.opts.ContextSize * lp.opts.Parallelism

	args := []string{
		"--model", lp.opts.ModelPath,
		"--ctx-size", strconv.Itoa(totalCtx),
		"--parallel", strconv.Itoa(lp.opts.Parallelism),
		"--port", strconv.Itoa(lp.opts.Port),
		"--host", lp.opts.Host,
		"--batch-size", "512",
		"--jinja", // Enable Jinja templating for tool calling
	}

	if lp.opts.Threads > 0 {
		args = append(args, "--threads", strconv.Itoa(lp.opts.Threads))
	}
	if lp.opts.GPULayers > 0 {
		args = append(args, "--n-gpu-layers", strconv.Itoa(lp.opts.GPULayers))
	}

	lp.logger.Info("spawning llama.cpp",
		"binary", lp.opts.BinaryPath,
		"model", lp.opts.ModelPath,
		"ctx_size", totalCtx,
		"parallelism", lp.opts.Parallelism,
		"port", lp.opts.Port,
	)

	lp.cmd = exec.Command(lp.opts.BinaryPath, args...)
	lp.cmd.Env = os.Environ()

	// Suppress llama.cpp output unless --debug 2.
	// At debug level 2, pipe directly to the terminal.
	// Otherwise, capture and only log critical errors from stderr.
	if DebugLevel() >= 2 {
		lp.cmd.Stdout = os.Stdout
		lp.cmd.Stderr = os.Stderr
	} else {
		lp.cmd.Stdout = &llamaOutputFilter{logger: lp.logger, critical: false}
		lp.cmd.Stderr = &llamaOutputFilter{logger: lp.logger, critical: true}
	}

	if err := lp.cmd.Start(); err != nil {
		return fmt.Errorf("starting llama.cpp: %w", err)
	}

	lp.running = true

	// Monitor process in background
	go lp.monitor()

	return nil
}

// monitor waits for the process to exit and fires the crash callback if unexpected.
func (lp *LlamaProcess) monitor() {
	err := lp.cmd.Wait()

	lp.mu.Lock()
	wasRunning := lp.running
	lp.running = false
	lp.mu.Unlock()

	if !wasRunning {
		// We called Stop() — this is expected
		return
	}

	exitCode := -1
	if lp.cmd.ProcessState != nil {
		exitCode = lp.cmd.ProcessState.ExitCode()
	}

	lp.logger.Error("llama.cpp exited unexpectedly",
		"exit_code", exitCode,
		"error", err,
	)

	if lp.onCrash != nil {
		lp.onCrash(exitCode, err)
	}
}

// Stop gracefully stops the llama.cpp process.
func (lp *LlamaProcess) Stop() error {
	lp.mu.Lock()
	defer lp.mu.Unlock()

	if !lp.running || lp.cmd == nil || lp.cmd.Process == nil {
		return nil
	}

	lp.running = false // Mark as intentional stop
	lp.logger.Info("stopping llama.cpp")

	// Try SIGTERM first
	if err := lp.cmd.Process.Signal(os.Interrupt); err != nil {
		return nil
	}

	// Wait up to 5 seconds for graceful shutdown
	done := make(chan struct{})
	go func() {
		_ = lp.cmd.Wait()
		close(done)
	}()

	select {
	case <-done:
		return nil
	case <-time.After(5 * time.Second):
		lp.logger.Warn("llama.cpp did not exit gracefully, killing")
		return lp.cmd.Process.Kill()
	}
}

// IsRunning reports whether the llama.cpp process is alive.
func (lp *LlamaProcess) IsRunning() bool {
	lp.mu.Lock()
	defer lp.mu.Unlock()
	return lp.running
}

// URL returns the HTTP URL of the running llama.cpp server.
func (lp *LlamaProcess) URL() string {
	return fmt.Sprintf("http://%s:%d", lp.opts.Host, lp.opts.Port)
}

// WaitForReady polls llama.cpp until it responds to /v1/models or timeout.
func (lp *LlamaProcess) WaitForReady(ctx context.Context, timeout time.Duration) error {
	deadline := time.After(timeout)
	ticker := time.NewTicker(1 * time.Second)
	defer ticker.Stop()

	url := lp.URL() + "/v1/models"

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-deadline:
			return fmt.Errorf("llama.cpp did not become ready within %v", timeout)
		case <-ticker.C:
			if !lp.IsRunning() {
				return fmt.Errorf("llama.cpp process exited before becoming ready")
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

// Warmup sends a minimal inference request to prime the KV cache.
func (lp *LlamaProcess) Warmup() {
	url := lp.URL() + "/v1/chat/completions"
	body := `{"model":"placeholder","messages":[{"role":"user","content":"Hi"}],"max_tokens":1,"stream":false}`

	req, err := http.NewRequest("POST", url, strings.NewReader(body))
	if err != nil {
		return
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return
	}
	defer resp.Body.Close()
	_, _ = io.ReadAll(resp.Body)
}

// DiscoverCapabilities queries the running llama.cpp for its actual capabilities.
func (lp *LlamaProcess) DiscoverCapabilities() (*DiscoveredCaps, error) {
	caps := &DiscoveredCaps{
		Backend: "llama.cpp",
		CtxSize: 4096, // Fallback
	}

	baseURL := lp.URL()

	// 1. Query /v1/models
	modelsResp, err := http.Get(baseURL + "/v1/models")
	if err != nil {
		return nil, fmt.Errorf("querying /v1/models: %w", err)
	}
	defer modelsResp.Body.Close()

	if modelsResp.StatusCode == http.StatusOK {
		var modelsData struct {
			Data []struct {
				ID   string `json:"id"`
				Meta *struct {
					NCtxTrain *int   `json:"n_ctx_train"`
					NParams   *int64 `json:"n_params"`
				} `json:"meta"`
			} `json:"data"`
		}
		if err := json.NewDecoder(modelsResp.Body).Decode(&modelsData); err == nil && len(modelsData.Data) > 0 {
			caps.Model = normalizeModelName(modelsData.Data[0].ID)
			if modelsData.Data[0].Meta != nil && modelsData.Data[0].Meta.NParams != nil {
				p := *modelsData.Data[0].Meta.NParams
				if p >= 1e9 {
					caps.Parameters = fmt.Sprintf("%dB", p/1e9)
				} else if p >= 1e6 {
					caps.Parameters = fmt.Sprintf("%dM", p/1e6)
				}
			}
		}
	}

	// 2. Query /props for runtime context size and slot count
	propsResp, err := http.Get(baseURL + "/props")
	if err == nil {
		defer propsResp.Body.Close()
		if propsResp.StatusCode == http.StatusOK {
			var props struct {
				DefaultGenerationSettings *struct {
					NCtx int `json:"n_ctx"`
				} `json:"default_generation_settings"`
				TotalSlots  int `json:"total_slots"`
				TotalVRAMMB int `json:"total_vram_mb"`
			}
			if err := json.NewDecoder(propsResp.Body).Decode(&props); err == nil {
				if props.DefaultGenerationSettings != nil && props.DefaultGenerationSettings.NCtx > 0 {
					caps.CtxSize = props.DefaultGenerationSettings.NCtx
				}
				if props.TotalSlots > 0 {
					caps.TotalSlots = props.TotalSlots
				}
				if props.TotalVRAMMB > 0 {
					caps.VRAMGB = float64(props.TotalVRAMMB) / 1024
				}
			}
		}
	}

	return caps, nil
}

// DiscoveredCaps holds capabilities discovered from a running llama.cpp instance.
type DiscoveredCaps struct {
	Backend    string
	Model      string
	CtxSize    int
	VRAMGB     float64
	Parameters string
	TotalSlots int
}

// normalizeModelName strips path and quantization suffix from a model ID.
func normalizeModelName(filePath string) string {
	parts := strings.Split(filePath, "/")
	name := parts[len(parts)-1]

	name = strings.TrimSuffix(name, ".gguf")
	name = strings.TrimSuffix(name, ".GGUF")

	// Remove common quantization suffixes
	for _, suffix := range []string{
		"-Q4_K_M", "-Q4_K_S", "-Q5_K_M", "-Q5_K_S",
		"-Q6_K", "-Q8_0", "-Q3_K_M", "-Q3_K_S",
		"-Q2_K", "-Q4_0", "-Q5_0",
		".Q4_K_M", ".Q4_K_S", ".Q5_K_M", ".Q5_K_S",
		".Q6_K", ".Q8_0", ".Q3_K_M", ".Q3_K_S",
	} {
		if strings.HasSuffix(name, suffix) {
			name = strings.TrimSuffix(name, suffix)
			break
		}
	}

	name = strings.TrimRight(name, "-_")
	return name
}

// FindFreePort asks the OS for a random available TCP port.
func FindFreePort() (int, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	port := l.Addr().(*net.TCPAddr).Port
	l.Close()
	return port, nil
}

// llamaOutputFilter is an io.Writer that silently discards llama.cpp output
// unless a line contains a critical keyword (only for stderr when critical=true).
// Critical lines are logged via slog so they appear as proper JSON.
type llamaOutputFilter struct {
	logger   *slog.Logger
	critical bool // true = stderr (check for error keywords), false = stdout (discard)
	buf      []byte
}

func (f *llamaOutputFilter) Write(p []byte) (int, error) {
	if !f.critical {
		// stdout — discard silently
		return len(p), nil
	}

	// stderr — scan for critical keywords
	f.buf = append(f.buf, p...)

	for {
		idx := -1
		for i, b := range f.buf {
			if b == '\n' {
				idx = i
				break
			}
		}
		if idx < 0 {
			// Keep buffer bounded even without newlines
			if len(f.buf) > 4096 {
				f.buf = f.buf[len(f.buf)-2048:]
			}
			break
		}

		line := string(f.buf[:idx])
		f.buf = f.buf[idx+1:]

		lower := strings.ToLower(line)
		if strings.Contains(lower, "error") ||
			strings.Contains(lower, "fatal") ||
			strings.Contains(lower, "crash") ||
			strings.Contains(lower, "failed") {
			f.logger.Warn("llama.cpp", "line", strings.TrimSpace(line))
		}
	}

	return len(p), nil
}
