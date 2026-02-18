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

	// Stderr capture for crash diagnostics
	stderrCapture *llamaStderrCapture

	// Crash notification
	onCrash func(exitCode int, err error)
}

// LlamaOpts holds the llama.cpp launch parameters.
type LlamaOpts struct {
	BinaryPath       string
	ModelPath        string
	ContextSize      int // Per-slot context size (will be multiplied by Parallelism)
	Parallelism      int
	Port             int
	Host             string
	GPULayers        int
	Threads          int
	ReasoningFormat  string // "deepseek" to enable thinking token extraction, "" to disable
	ChatTemplateFile string // Path to a patched Jinja template file (overrides model's built-in template)
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
		"--jinja",                    // Enable Jinja templating for tool calling
		"--reasoning-format", "deepseek", // Always extract <think> blocks into reasoning_content (harmless if model doesn't think)
	}

	if lp.opts.Threads > 0 {
		args = append(args, "--threads", strconv.Itoa(lp.opts.Threads))
	}
	if lp.opts.GPULayers > 0 {
		args = append(args, "--n-gpu-layers", strconv.Itoa(lp.opts.GPULayers))
	}
	if lp.opts.ChatTemplateFile != "" {
		args = append(args, "--chat-template-file", lp.opts.ChatTemplateFile)
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

	// Capture stderr for crash diagnostics and error logging.
	// At debug level 2, also tee directly to the terminal.
	lp.stderrCapture = newLlamaStderrCapture(lp.logger)
	if DebugLevel() >= 2 {
		lp.cmd.Stdout = os.Stdout
		lp.cmd.Stderr = io.MultiWriter(os.Stderr, lp.stderrCapture)
	} else {
		lp.cmd.Stdout = io.Discard
		lp.cmd.Stderr = lp.stderrCapture
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

	// Include recent stderr output in the crash log so we can see WHY it died
	recentStderr := ""
	if lp.stderrCapture != nil {
		recentStderr = lp.stderrCapture.RecentLines(20)
	}

	attrs := []any{
		"exit_code", exitCode,
		"error", err,
	}
	if recentStderr != "" {
		attrs = append(attrs, "stderr", recentStderr)
	}
	lp.logger.Error("llama.cpp exited unexpectedly", attrs...)

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

// reasoningShim is a Jinja no-op prepended to chat templates that don't preserve
// reasoning_content in multi-turn history. It tricks llama.cpp's template capability
// test into setting supports_preserve_reasoning=true by reading (but not rendering)
// the reasoning_content field from each assistant message.
//
// This is generic — works with any template, produces zero output, and is harmless
// on templates that already support reasoning.
const reasoningShim = `{%- for m in messages -%}{%- if m.reasoning_content is defined %}{% set _ = m.reasoning_content %}{% endif -%}{%- endfor -%}
`

// CheckReasoningSupport queries /props to check if the template supports reasoning
// extraction natively. If not, it fetches the template, applies fixes, and writes
// the patched template to a temp file.
//
// Two fixes may be applied:
//  1. Shim: prepends a no-op loop that reads reasoning_content so llama.cpp detects
//     supports_preserve_reasoning=true.
//  2. Suffix strip: removes a trailing "<think>" from the generation prompt so the
//     model generates it as its first token, allowing llama.cpp's --reasoning-format
//     deepseek parser to detect both opening and closing tags. (Needed for models like
//     Kimi K2.5 whose llama.cpp handler doesn't set thinking_forced_open.)
//
// Returns the path to the patched template file (caller should restart llama.cpp
// with --chat-template-file), or "" if no patching is needed.
func (lp *LlamaProcess) CheckReasoningSupport() (string, error) {
	baseURL := lp.URL()

	resp, err := http.Get(baseURL + "/props")
	if err != nil {
		return "", fmt.Errorf("querying /props: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("/props returned status %d", resp.StatusCode)
	}

	var props struct {
		ChatTemplate     string `json:"chat_template"`
		ChatTemplateCaps struct {
			SupportsPreserveReasoning bool `json:"supports_preserve_reasoning"`
		} `json:"chat_template_caps"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&props); err != nil {
		return "", fmt.Errorf("decoding /props: %w", err)
	}

	if props.ChatTemplate == "" {
		lp.logger.Info("no chat template found, skipping reasoning check")
		return "", nil
	}

	needsShim := !props.ChatTemplateCaps.SupportsPreserveReasoning
	needsSuffixStrip := templateEndsWithForcedThink(props.ChatTemplate)

	if !needsShim && !needsSuffixStrip {
		lp.logger.Info("template supports reasoning natively")
		return "", nil
	}

	patched := props.ChatTemplate

	if needsShim {
		lp.logger.Info("template does not preserve reasoning, applying shim")
		patched = reasoningShim + patched
	}

	if needsSuffixStrip {
		lp.logger.Info("template forces <think> in suffix, stripping so model generates it")
		patched = stripForcedThinkSuffix(patched)
	}

	tmpFile, err := os.CreateTemp("", "auxot-chat-template-*.jinja")
	if err != nil {
		return "", fmt.Errorf("creating temp template file: %w", err)
	}

	if _, err := tmpFile.WriteString(patched); err != nil {
		tmpFile.Close()
		os.Remove(tmpFile.Name())
		return "", fmt.Errorf("writing patched template: %w", err)
	}
	tmpFile.Close()

	lp.logger.Info("patched template saved", "path", tmpFile.Name())
	return tmpFile.Name(), nil
}

// templateEndsWithForcedThink checks if a Jinja chat template forces a <think>
// tag in its generation prompt (the assistant suffix). This is detected by looking
// for the pattern where the template ends with an unconditional <think> after the
// add_generation_prompt guard.
//
// Templates that do this (like Kimi K2.5) cause issues because llama.cpp's Kimi
// handler doesn't set thinking_forced_open=true, so the deepseek reasoning parser
// doesn't know the generation starts inside a think block.
func templateEndsWithForcedThink(template string) bool {
	// Look for the common pattern at the end of templates:
	//   {%- else -%}
	//   <think>
	//   {%- endif -%}
	//   {%- endif -%}
	// This indicates the template unconditionally forces <think> when thinking is enabled.
	lines := strings.Split(template, "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		trimmed := strings.TrimSpace(lines[i])
		if trimmed == "" {
			continue
		}
		// Check if one of the last meaningful lines is a standalone <think>
		// between else/endif guards (the generation prompt pattern)
		if trimmed == "<think>" {
			// Verify it's in the add_generation_prompt block by checking nearby lines
			for j := i - 1; j >= 0 && j >= i-5; j-- {
				prev := strings.TrimSpace(lines[j])
				if strings.Contains(prev, "else") || strings.Contains(prev, "add_generation_prompt") {
					return true
				}
			}
		}
		// Only check the last block of the template
		if strings.Contains(trimmed, "endif") {
			continue
		}
		if !strings.HasPrefix(trimmed, "{%") && trimmed != "<think>" && trimmed != "<think></think>" {
			break
		}
	}
	return false
}

// stripForcedThinkSuffix modifies a Jinja template to remove the unconditional
// <think> from the generation prompt. The model will then generate <think> as its
// first token, allowing llama.cpp's reasoning parser to detect it properly.
//
// Before:
//
//	{%- if thinking is defined and thinking is false -%}
//	<think></think>
//	{%- else -%}
//	<think>
//	{%- endif -%}
//
// After:
//
//	{%- if thinking is defined and thinking is false -%}
//	<think></think>
//	{%- endif -%}
func stripForcedThinkSuffix(template string) string {
	lines := strings.Split(template, "\n")
	result := make([]string, 0, len(lines))

	// Walk backwards to find the generation prompt block and strip the
	// {%- else -%}\n  <think> portion, leaving the thinking=false guard intact.
	skipElseBlock := false
	for i := len(lines) - 1; i >= 0; i-- {
		trimmed := strings.TrimSpace(lines[i])

		if !skipElseBlock {
			// Look for the standalone <think> line near the end
			if trimmed == "<think>" {
				// Check if previous line is an else block
				for j := i - 1; j >= 0; j-- {
					prevTrimmed := strings.TrimSpace(lines[j])
					if prevTrimmed == "" {
						continue
					}
					if strings.Contains(prevTrimmed, "else") {
						// Found the pattern! Skip both the <think> and the else line
						skipElseBlock = true
						lines[i] = "" // Remove <think>
						lines[j] = "" // Remove else
					}
					break
				}
				if skipElseBlock {
					continue
				}
			}
		}
	}

	for _, line := range lines {
		// Skip blank lines that were cleared
		if strings.TrimSpace(line) == "" && skipElseBlock {
			// Keep the line but check if it was one we cleared
			result = append(result, line)
		} else {
			result = append(result, line)
		}
	}

	return strings.Join(result, "\n")
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

// llamaStderrCapture captures llama.cpp stderr output for diagnostics.
// It keeps a rolling buffer of recent lines and logs critical ones via slog.
type llamaStderrCapture struct {
	logger *slog.Logger
	mu     sync.Mutex
	buf    []byte
	lines  []string // Rolling buffer of recent lines
	maxLines int
}

func newLlamaStderrCapture(logger *slog.Logger) *llamaStderrCapture {
	return &llamaStderrCapture{
		logger:   logger,
		maxLines: 50, // Keep last 50 lines for crash diagnostics
	}
}

func (c *llamaStderrCapture) Write(p []byte) (int, error) {
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
			// Keep buffer bounded even without newlines
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

		// Store in rolling buffer
		c.lines = append(c.lines, line)
		if len(c.lines) > c.maxLines {
			c.lines = c.lines[len(c.lines)-c.maxLines:]
		}

		// Log critical lines immediately
		lower := strings.ToLower(line)
		if strings.Contains(lower, "error") ||
			strings.Contains(lower, "fatal") ||
			strings.Contains(lower, "abort") ||
			strings.Contains(lower, "crash") ||
			strings.Contains(lower, "failed") ||
			strings.Contains(lower, "library not loaded") ||
			strings.Contains(lower, "dyld") {
			c.logger.Warn("llama.cpp stderr", "line", line)
		}
	}

	return len(p), nil
}

// RecentLines returns the last n lines of captured stderr output,
// joined with newlines. Useful for crash diagnostics.
func (c *llamaStderrCapture) RecentLines(n int) string {
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
