// Command auxot-worker is the GPU inference worker binary.
//
// Lifecycle:
//  1. Connect to router → authenticate → receive policy (model, quant, ctx, parallelism)
//  2. Download model from HuggingFace (cached to ~/.auxot/models/)
//  3. Download llama.cpp binary from GitHub releases (cached to ~/.auxot/llama-server/)
//  4. Detect GPU hardware
//  5. Spawn llama.cpp as subprocess
//  6. Discover capabilities from running llama.cpp
//  7. Reconnect to router permanently → send capabilities → process jobs
package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/joho/godotenv"

	"github.com/auxothq/auxot/internal/cliworker"
	"github.com/auxothq/auxot/internal/worker"
	"github.com/auxothq/auxot/pkg/gpudetect"
	"github.com/auxothq/auxot/pkg/llamabin"
	"github.com/auxothq/auxot/pkg/logutil"
	"github.com/auxothq/auxot/pkg/modeldown"
	"github.com/auxothq/auxot/pkg/protocol"
	"github.com/auxothq/auxot/pkg/registry"
	"github.com/auxothq/auxot/pkg/sdbin"
)

// pendingToolCall holds the result channel for an in-flight live-MCP tool call.
type pendingToolCall struct {
	ch chan liveToolResult
}

type liveToolResult struct {
	Result  string
	IsError bool
}

// appendErrorDetails appends truncated stderr or other detail text for job errors.
func appendErrorDetails(errMsg, details string) string {
	if details == "" {
		return errMsg
	}
	const max = 800
	d := strings.TrimSpace(details)
	if len(d) > max {
		d = d[:max] + "…"
	}
	return errMsg + " — " + d
}

func main() {
	// MCP stdio mode: when spawned by the claude CLI as an MCP subprocess.
	// The AUXOT_MCP_TOOLS_FILE env var is set by setupMCP() in cliworker/claude.go.
	// AUXOT_MCP_TOOL_PROXY is set in live-continuation mode and routes tool calls
	// to the in-worker HTTP proxy rather than returning stub errors.
	if toolsFile := os.Getenv("AUXOT_MCP_TOOLS_FILE"); toolsFile != "" {
		proxyURL := os.Getenv("AUXOT_MCP_TOOL_PROXY")
		jobID := os.Getenv("AUXOT_MCP_JOB_ID")
		var mcpErr error
		if proxyURL != "" && jobID != "" {
			mcpErr = cliworker.RunMCPStdioLive(toolsFile, proxyURL, jobID)
		} else {
			mcpErr = cliworker.RunMCPStdio(toolsFile)
		}
		if mcpErr != nil {
			fmt.Fprintf(os.Stderr, "mcp: %v\n", mcpErr)
			os.Exit(1)
		}
		return
	}

	_ = godotenv.Load()

	// Manual argument parsing (--debug has optional value that flag package can't handle)
	var flags worker.CLIFlags
	debugLevel := 0

	args := os.Args[1:]

	// Daemon management subcommands — handled before flag parsing.
	if len(args) > 0 {
		switch args[0] {
		case "install":
			cmdInstall(args[1:])
			return
		case "list":
			cmdList()
			return
		case "uninstall":
			cmdUninstall(args[1:])
			return
		}
	}

	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "version":
			fmt.Println("auxot-worker v0.1.0")
			return
		case "help", "--help", "-h":
			printHelp()
			return
		case "--gpu-key":
			if i+1 < len(args) {
				i++
				flags.GPUKey = args[i]
			}
		case "--router-url":
			if i+1 < len(args) {
				i++
				flags.RouterURL = args[i]
			}
		case "--model-path":
			if i+1 < len(args) {
				i++
				flags.ModelPath = args[i]
			}
		case "--llama-server-path":
			if i+1 < len(args) {
				i++
				flags.LlamaServerPath = args[i]
			}
		case "--debug":
			// --debug (default 1) or --debug 1 or --debug 2
			debugLevel = 1
			if i+1 < len(args) {
				if n, err := strconv.Atoi(args[i+1]); err == nil && (n == 1 || n == 2) {
					debugLevel = n
					i++
				}
			}
		}
	}

	// Set debug level BEFORE anything else
	worker.SetDebugLevel(debugLevel)

	// All output is JSON via slog. Pretty-printed when stderr is a TTY.
	logger := slog.New(slog.NewJSONHandler(
		logutil.Output(os.Stderr),
		&slog.HandlerOptions{Level: slogLevel()},
	))
	slog.SetDefault(logger)

	cfg, err := worker.LoadConfig(flags)
	if err != nil {
		logger.Error("configuration error", "error", err.Error())
		os.Exit(1)
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	if err := run(ctx, cfg, logger); err != nil {
		logger.Error("fatal", "error", err.Error())
		os.Exit(1)
	}
}

func run(ctx context.Context, cfg *worker.Config, logger *slog.Logger) error {
	logger.Info("auxot-worker starting", "version", "0.1.0")

	// ----------------------------------------------------------------
	// Phase 1: Fetch policy from router (temporary connection)
	// ----------------------------------------------------------------
	conn := worker.NewConnection(cfg.RouterURL, cfg.AdminKey, cfg, logger)

	policy, err := conn.FetchPolicy()
	if err != nil {
		return fmt.Errorf("fetching policy: %w", err)
	}
	logger.Info("authenticated", "gpu_id", conn.GPUID())
	logger.Info("policy",
		"worker_type", policy.WorkerType,
		"cli_type", policy.CLIType,
		"model", policy.ModelName,
		"quantization", policy.Quantization,
		"context_size", policy.ContextSize,
		"max_parallelism", policy.MaxParallelism,
	)

	// ----------------------------------------------------------------
	// CLI worker branch — no llama.cpp, no model download
	// ----------------------------------------------------------------
	if policy.WorkerType == "cli" {
		return runCLIWorker(ctx, cfg, conn, policy, logger)
	}

	// ----------------------------------------------------------------
	// External llama.cpp shortcut (AUXOT_LLAMA_URL)
	// ----------------------------------------------------------------
	// When AUXOT_LLAMA_URL is set the worker skips model download, binary
	// download, and process spawning — it connects directly to the already-
	// running llama.cpp server at that URL. Useful for local dev where you
	// have llama.cpp running separately (e.g. http://localhost:9002).
	if cfg.LlamaURL != "" {
		logger.Info("using external llama.cpp", "url", cfg.LlamaURL)
		return runWithExternalLlama(ctx, cfg, conn, cfg.LlamaURL, logger)
	}

	// ----------------------------------------------------------------
	// Registry lookup (before model download — image-generation detection)
	// ----------------------------------------------------------------
	// Load the registry early so we can tell stable-diffusion models from GGUF
	// text models before the download phase completes.
	reg, registryErr := registry.Load()
	var regEntry *registry.Model
	if registryErr == nil {
		regEntry = reg.FindByNameAndQuant(policy.ModelName, policy.Quantization)
	} else {
		logger.Warn("loading registry", "error", registryErr)
	}

	// ----------------------------------------------------------------
	// Phase 2: Download model (GGUF, for llama.cpp and stable-diffusion paths)
	// ----------------------------------------------------------------
	modelResult, err := modeldown.Ensure(ctx, policy, cfg.ModelsDir, cfg.ModelFile, logger)
	if err != nil {
		return fmt.Errorf("ensuring model: %w", err)
	}
	modelPath := modelResult.ModelPath
	mmprojPath := modelResult.MmprojPath
	logger.Info("model ready", "path", modelPath, "mmproj", mmprojPath != "")

	// Check if this is an image generation model — use stable-diffusion.cpp path.
	// regEntry may already be set from above; if registry failed we try again here
	// using the model we already downloaded (non-fatal if registry still fails).
	isImageGen := false
	var regModel *registry.Model
	if regEntry != nil {
		for _, cap := range regEntry.Capabilities {
			if cap == "image_generation" {
				isImageGen = true
				regModel = regEntry
				break
			}
		}
	} else if registryErr != nil {
		// Second attempt after download (registry is embedded, so this rarely
		// differs — but keep the same behaviour as before the restructure).
		if reg2, err2 := registry.Load(); err2 == nil {
			if m := reg2.FindByNameAndQuant(policy.ModelName, policy.Quantization); m != nil {
				for _, cap := range m.Capabilities {
					if cap == "image_generation" {
						isImageGen = true
						regModel = m
						break
					}
				}
			}
		}
	}

	if isImageGen {
		return runWithStableDiffusion(ctx, cfg, conn, policy, modelPath, regModel, logger)
	}

	// ----------------------------------------------------------------
	// Phase 3: Download llama.cpp binary + detect GPU
	// ----------------------------------------------------------------
	binaryPath := cfg.LlamaBinaryPath
	if binaryPath == "" {
		bp, err := llamabin.Ensure(cfg.LlamaCacheDir, logger)
		if err != nil {
			return fmt.Errorf("ensuring llama.cpp binary: %w", err)
		}
		binaryPath = bp
	}
	logger.Info("llama.cpp binary", "path", binaryPath)

	gpuResult := gpudetect.Detect()
	logger.Info("gpu detected",
		"backend", gpuResult.Backend,
		"detected", gpuResult.Detected,
	)
	if gpuResult.Warning != "" {
		logger.Warn("gpu", "warning", gpuResult.Warning)
	}

	// Find a free port for llama.cpp
	llamaPort, err := worker.FindFreePort()
	if err != nil {
		return fmt.Errorf("finding free port: %w", err)
	}

	// ----------------------------------------------------------------
	// Phase 4: Spawn llama.cpp (with parallel dial-down on failure)
	// ----------------------------------------------------------------
	// The router requests max_parallelism, but the worker may not have enough
	// memory to run that many concurrent slots. We try the requested parallelism
	// first, and on failure (OOM, crash), dial down until we find one that works.
	parallelism := policy.MaxParallelism
	var llama *worker.LlamaProcess

	for parallelism >= 1 {
		llama = worker.NewLlamaProcess(worker.LlamaOpts{
			BinaryPath:  binaryPath,
			ModelPath:   modelPath,
			MmprojPath:  mmprojPath,
			ContextSize: policy.ContextSize,
			Parallelism: parallelism,
			Port:        llamaPort,
			Host:        "127.0.0.1",
			GPULayers:   cfg.GPULayers,
			Threads:     cfg.Threads,
		}, logger)

		if err := llama.Start(); err != nil {
			if parallelism > 1 {
				logger.Warn("llama.cpp failed to start, reducing parallelism",
					"parallelism", parallelism,
					"next", parallelism-1,
					"error", err.Error(),
				)
				parallelism--
				continue
			}
			return fmt.Errorf("starting llama.cpp: %w", err)
		}

		logger.Info("llama.cpp started", "port", llamaPort, "parallelism", parallelism)
		logger.Info("waiting for llama.cpp")

		if err := llama.WaitForReady(ctx, 10*time.Minute); err != nil {
			llama.Stop()
			if parallelism > 1 {
				logger.Warn("llama.cpp failed to become ready, reducing parallelism",
					"parallelism", parallelism,
					"next", parallelism-1,
					"error", err.Error(),
				)
				parallelism--
				continue
			}
			return fmt.Errorf("llama.cpp not ready: %w", err)
		}

		// Success — llama.cpp is up and running
		break
	}

	defer func() { llama.Stop() }()

	if parallelism < policy.MaxParallelism {
		logger.Info("parallelism reduced from router policy",
			"requested", policy.MaxParallelism,
			"actual", parallelism,
		)
	}
	logger.Info("llama.cpp ready", "parallelism", parallelism)

	llama.Warmup()
	logger.Info("model warmed up")

	// ----------------------------------------------------------------
	// Phase 4b: Patch template for reasoning support if needed
	// ----------------------------------------------------------------
	// Some models (e.g. Kimi K2.5) have templates that don't preserve
	// reasoning_content in multi-turn history, which prevents llama.cpp
	// from extracting thinking tokens. We detect this and patch the
	// template with a no-op shim, then restart llama.cpp.
	patchedTemplatePath, err := llama.CheckReasoningSupport()
	if err != nil {
		logger.Warn("reasoning support check failed", "error", err.Error())
	}
	if patchedTemplatePath != "" {
		defer os.Remove(patchedTemplatePath) // Clean up temp file on exit

		logger.Info("restarting llama.cpp with patched template")
		llama.Stop()

		// Recreate with the patched template file
		llama = worker.NewLlamaProcess(worker.LlamaOpts{
			BinaryPath:       binaryPath,
			ModelPath:        modelPath,
			MmprojPath:       mmprojPath,
			ContextSize:      policy.ContextSize,
			Parallelism:      parallelism,
			Port:             llamaPort,
			Host:             "127.0.0.1",
			GPULayers:        cfg.GPULayers,
			Threads:          cfg.Threads,
			ChatTemplateFile: patchedTemplatePath,
		}, logger)

		if err := llama.Start(); err != nil {
			return fmt.Errorf("restarting llama.cpp with patched template: %w", err)
		}
		if err := llama.WaitForReady(ctx, 10*time.Minute); err != nil {
			llama.Stop()
			return fmt.Errorf("patched llama.cpp not ready: %w", err)
		}
		llama.Warmup()
		logger.Info("llama.cpp restarted with reasoning support")
	}

	// ----------------------------------------------------------------
	// Phase 5: Discover capabilities
	// ----------------------------------------------------------------
	caps, err := llama.DiscoverCapabilities()
	if err != nil {
		return fmt.Errorf("discovering capabilities: %w", err)
	}

	logger.Info("capabilities",
		"model", caps.Model,
		"backend", caps.Backend,
		"ctx_size", caps.CtxSize,
		"total_slots", caps.TotalSlots,
		"vram_gb", caps.VRAMGB,
		"parameters", caps.Parameters,
	)

	// ----------------------------------------------------------------
	// Phase 6: Permanent connection
	// ----------------------------------------------------------------
	if err := conn.ConnectPermanent(caps); err != nil {
		return fmt.Errorf("permanent connection: %w", err)
	}
	defer conn.Close()

	// CRITICAL: When context is cancelled (SIGINT), close the WebSocket
	// connection to unblock ReadMessage(). Without this, Ctrl+C hangs forever.
	go func() {
		<-ctx.Done()
		conn.Close()
	}()

	logger.Info("worker ready", "gpu_id", conn.GPUID())

	// ----------------------------------------------------------------
	// Phase 7: Process jobs
	// ----------------------------------------------------------------
	executor := worker.NewExecutor(llama.URL(), cfg.JobTimeout, mmprojPath != "", "", logger)
	activeJobs := &sync.Map{}

	conn.OnJob(func(job protocol.JobMessage) {
		abortCtx, cancel := context.WithCancel(ctx)
		activeJobs.Store(job.JobID, cancel)
		defer func() {
			activeJobs.Delete(job.JobID)
			cancel()
		}()

		executor.Execute(
			abortCtx,
			job,
			func(token string) error {
				return conn.SendToken(job.JobID, token)
			},
			func(token string) error {
				return conn.SendReasoningToken(job.JobID, token)
			},
			func() error {
				return conn.SendToolGenerating(job.JobID)
			},
			func(fullResponse, reasoningContent string, cacheTokens, inputTokens, outputTokens, reasoningTokens int, durationMS int64, toolCalls []protocol.ToolCall) error {
				return conn.SendComplete(job.JobID, fullResponse, "", reasoningContent, durationMS, cacheTokens, inputTokens, outputTokens, reasoningTokens, 0, "", "", 0, "", toolCalls, nil)
			},
			func(errMsg, details string) error {
				return conn.SendError(job.JobID, appendErrorDetails(errMsg, details))
			},
			func(total, cached, processed int) error {
				return conn.SendPromptProgress(job.JobID, total, cached, processed)
			},
		)
	})

	conn.OnCancel(func(jobID string) {
		if cancel, ok := activeJobs.Load(jobID); ok {
			cancel.(context.CancelFunc)()
		}
	})

	// Handle llama.cpp crash → auto-restart
	var llamaMu sync.Mutex

	llama.OnCrash(func(exitCode int, err error) {
		logger.Error("llama.cpp crashed", "exit_code", exitCode, "error", err)
		logger.Info("restarting llama.cpp")
		time.Sleep(2 * time.Second)

		llamaMu.Lock()
		defer llamaMu.Unlock()

		if restartErr := llama.Start(); restartErr != nil {
			logger.Error("failed to restart llama.cpp", "error", restartErr)
			return
		}
		if err := llama.WaitForReady(ctx, 10*time.Minute); err != nil {
			logger.Error("restarted llama.cpp not ready", "error", err)
			return
		}
		logger.Info("llama.cpp recovered")
	})

	// Handle policy_update from the server: restart llama.cpp with new settings.
	// If the model itself changed we exit cleanly so the daemon manager relaunches
	// the worker and FetchPolicy picks up the new model. For context_size /
	// max_parallelism changes we restart llama.cpp in place.
	conn.OnPolicyUpdate(func(newPolicy *protocol.Policy) {
		if newPolicy.ModelName != policy.ModelName || newPolicy.Quantization != policy.Quantization {
			logger.Info("policy_update: model changed, exiting for clean restart",
				"old_model", policy.ModelName, "new_model", newPolicy.ModelName,
				"old_quant", policy.Quantization, "new_quant", newPolicy.Quantization,
			)
			os.Exit(0)
			return
		}

		if newPolicy.ContextSize == policy.ContextSize && newPolicy.MaxParallelism == policy.MaxParallelism {
			logger.Info("policy_update: no llama.cpp parameters changed, skipping restart")
			return
		}

		logger.Info("policy_update: restarting llama.cpp with new settings",
			"old_ctx", policy.ContextSize, "new_ctx", newPolicy.ContextSize,
			"old_par", policy.MaxParallelism, "new_par", newPolicy.MaxParallelism,
		)

		llamaMu.Lock()
		defer llamaMu.Unlock()

		llama.Stop()

		newOpts := worker.LlamaOpts{
			BinaryPath:       binaryPath,
			ModelPath:        modelPath,
			MmprojPath:       mmprojPath,
			ContextSize:      newPolicy.ContextSize,
			Parallelism:      newPolicy.MaxParallelism,
			Port:             llamaPort,
			Host:             "127.0.0.1",
			GPULayers:        cfg.GPULayers,
			Threads:          cfg.Threads,
			ChatTemplateFile: patchedTemplatePath,
		}
		llama = worker.NewLlamaProcess(newOpts, logger)

		if startErr := llama.Start(); startErr != nil {
			logger.Error("policy_update: failed to restart llama.cpp", "error", startErr)
			return
		}
		if readyErr := llama.WaitForReady(ctx, 10*time.Minute); readyErr != nil {
			logger.Error("policy_update: llama.cpp not ready", "error", readyErr)
			return
		}
		llama.Warmup()

		// Re-register crash handler for the new process.
		llama.OnCrash(func(exitCode int, err error) {
			logger.Error("llama.cpp crashed after policy update", "exit_code", exitCode, "error", err)
			time.Sleep(2 * time.Second)
			llamaMu.Lock()
			defer llamaMu.Unlock()
			if restartErr := llama.Start(); restartErr != nil {
				logger.Error("failed to restart llama.cpp", "error", restartErr)
				return
			}
			if readyErr := llama.WaitForReady(ctx, 10*time.Minute); readyErr != nil {
				logger.Error("restarted llama.cpp not ready", "error", readyErr)
				return
			}
			logger.Info("llama.cpp recovered after policy update crash")
		})

		policy = newPolicy
		logger.Info("policy_update applied",
			"ctx_size", newPolicy.ContextSize, "parallelism", newPolicy.MaxParallelism)
	})

	// Block on message loop (reconnects on disconnect)
	for {
		err := conn.RunMessageLoop()
		if ctx.Err() != nil {
			logger.Info("shutting down")
			return nil
		}
		if err != nil {
			logger.Warn("disconnected", "error", err.Error())
		}

		// Reconnect with backoff
		delay := cfg.ReconnectDelay
		for {
			if ctx.Err() != nil {
				return nil
			}

			logger.Info("reconnecting", "delay", delay.String())
			select {
			case <-ctx.Done():
				return nil
			case <-time.After(delay):
			}

			if err := conn.ConnectPermanent(caps); err != nil {
				logger.Warn("reconnect failed", "error", err.Error())
				delay *= 2
				if delay > cfg.ReconnectMaxDelay {
					delay = cfg.ReconnectMaxDelay
				}
				continue
			}
			logger.Info("reconnected", "gpu_id", conn.GPUID())
			break
		}
	}
}

// runCLIWorker handles the CLI worker path (worker_type = "cli").
// Unlike the GPU path there is no model download, no binary download, no llama.cpp.
// The worker connects permanently and dispatches jobs straight to the local CLI tool.
func runCLIWorker(ctx context.Context, cfg *worker.Config, conn *worker.Connection, policy *protocol.Policy, logger *slog.Logger) error {
	cliType := policy.CLIType
	if cliType == "" {
		cliType = "claude"
	}

	// For now only "claude" is implemented; "cursor" and "codex" are reserved.
	switch cliType {
	case "claude":
		// OK
	default:
		return fmt.Errorf("unsupported cli_type %q — only \"claude\" is implemented", cliType)
	}

	// Resolve the claude binary path. Workers can override via CLAUDE_PATH env.
	claudePath := os.Getenv("CLAUDE_PATH")
	if claudePath == "" {
		claudePath = "claude"
	}

	modelName := policy.ModelName // e.g. "claude-sonnet-4-5" — passed as --model flag
	builtinTools := policy.BuiltinTools
	logger.Info("cli worker ready",
		"cli_type", cliType,
		"claude_path", claudePath,
		"model", modelName,
		"builtin_tools", builtinTools,
	)

	// Build synthetic capabilities to report back to the server.
	caps := &worker.DiscoveredCaps{
		Backend: "cli/" + cliType,
		Model:   modelName,
		CtxSize: policy.ContextSize,
	}

	if err := conn.ConnectPermanent(caps); err != nil {
		return fmt.Errorf("cli worker: permanent connection: %w", err)
	}
	defer conn.Close()

	go func() {
		<-ctx.Done()
		conn.Close()
	}()

	logger.Info("cli worker connected", "gpu_id", conn.GPUID())

	// pendingLiveTools maps callID → pendingToolCall for live-MCP tool execution.
	// When a tool call arrives from the MCP subprocess via the live proxy, we
	// send it to the server and park a channel here; when the server replies with
	// the result, we deliver it to the waiting HTTP proxy handler.
	var pendingLiveTools sync.Map

	// Route TypeJobToolCallResult messages from the server to the waiting channel.
	conn.OnToolCallResult(func(msg protocol.JobToolCallResultMessage) {
		logger.Info("live_tool_result_received",
			"job_id", msg.JobID,
			"call_id", msg.CallID,
			"is_error", msg.IsError)
		if v, ok := pendingLiveTools.LoadAndDelete(msg.CallID); ok {
			pending := v.(pendingToolCall)
			pending.ch <- liveToolResult{Result: msg.Result, IsError: msg.IsError}
		} else {
			logger.Warn("live_tool_result_no_waiter", "call_id", msg.CallID)
		}
	})

	activeJobs := &sync.Map{}

	conn.OnJob(func(job protocol.JobMessage) {
		logger.Info("JOB_RECEIVED",
			"job_id", job.JobID,
			"tools", len(job.Tools),
			"messages", len(job.Messages),
		)
		// File-based debug log — bypasses all stdout/stderr routing.
		if df, err := os.OpenFile("/tmp/auxot-debug.log", os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0644); err == nil {
			fmt.Fprintf(df, "%s JOB_RECEIVED job_id=%s tools=%d msgs=%d\n",
				time.Now().Format(time.RFC3339), job.JobID, len(job.Tools), len(job.Messages))
			df.Close()
		}
		abortCtx, cancel := context.WithCancel(ctx)
		activeJobs.Store(job.JobID, cancel)
		defer func() {
			if r := recover(); r != nil {
				logger.Error("PANIC in RunJob", "job_id", job.JobID, "panic", fmt.Sprintf("%v", r))
			}
			activeJobs.Delete(job.JobID)
			cancel()
		}()

		// onToolCall bridges live-MCP tool execution between the in-process proxy
		// and the server. It sends TypeJobToolCallRequest to the server via WebSocket
		// and blocks until TypeJobToolCallResult arrives (via OnToolCallResult above).
		onToolCall := func(jobID, callID, toolName, arguments string) (result string, isError bool, err error) {
			ch := make(chan liveToolResult, 1)
			pendingLiveTools.Store(callID, pendingToolCall{ch: ch})
			defer pendingLiveTools.Delete(callID)

			if sendErr := conn.SendToolCallRequest(jobID, callID, toolName, arguments); sendErr != nil {
				return "", false, fmt.Errorf("sending tool call request: %w", sendErr)
			}

			select {
			case res := <-ch:
				return res.Result, res.IsError, nil
			case <-abortCtx.Done():
				return "", false, abortCtx.Err()
			}
		}

		cliworker.RunJob(
			abortCtx,
			job,
			cliworker.JobConfig{
				ClaudePath:   claudePath,
				Model:        modelName,
				BuiltinTools: builtinTools,
				// Live-MCP mode executes server tools in-band via the MCP subprocess.
			// Disabled when the job has caller-defined tools (API proxy jobs)
			// because those tools have no server-side executor — they must be
			// returned as unresolved tool_calls via the deny-kill path so the
			// coordinator fan-in can route them back to the HTTP caller.
			// API proxy jobs spawn a fresh Claude invocation per turn (no
			// --resume), so the original deny-kill bug does not apply.
			LiveMCP: len(job.Tools) > 0 && len(job.CallerTools) == 0,
				OnToolCall:   onToolCall,
			},
			func(token string) error { return conn.SendToken(job.JobID, token) },
			func(token string) error { return conn.SendReasoningToken(job.JobID, token) },
			func(id, name, args, result string) error {
				return conn.SendBuiltinTool(job.JobID, id, name, args, result)
			},
			func(preToolContent, postToolContent, reasoningContent string, cacheTokens, inputTokens, outputTokens, reasoningTokens int, durationMS int64, totalCostUSD float64, sessionID string, rateLimitStatus string, rateLimitResetsAt int64, rateLimitType string, toolCalls []protocol.ToolCall, builtinToolUses []protocol.BuiltinToolUse) error {
				return conn.SendComplete(job.JobID, preToolContent, postToolContent, reasoningContent, durationMS, cacheTokens, inputTokens, outputTokens, reasoningTokens, totalCostUSD, sessionID, rateLimitStatus, rateLimitResetsAt, rateLimitType, toolCalls, builtinToolUses)
			},
			func(errMsg, details string) error {
				return conn.SendError(job.JobID, appendErrorDetails(errMsg, details))
			},
			func(retryAfterSecs int, resetsAt int64, status, rateLimitType string) error {
				return conn.SendOverload(job.JobID, retryAfterSecs, resetsAt, status, rateLimitType)
			},
		)
	})

	conn.OnCancel(func(jobID string) {
		if cancel, ok := activeJobs.Load(jobID); ok {
			cancel.(context.CancelFunc)()
		}
	})

	for {
		err := conn.RunMessageLoop()
		if ctx.Err() != nil {
			logger.Info("cli worker shutting down")
			return nil
		}
		if err != nil {
			logger.Warn("cli worker disconnected", "error", err.Error())
		}

		delay := cfg.ReconnectDelay
		for {
			if ctx.Err() != nil {
				return nil
			}
			logger.Info("cli worker reconnecting", "delay", delay.String())
			select {
			case <-ctx.Done():
				return nil
			case <-time.After(delay):
			}
			if err := conn.ConnectPermanent(caps); err != nil {
				logger.Warn("cli worker reconnect failed", "error", err.Error())
				delay *= 2
				if delay > cfg.ReconnectMaxDelay {
					delay = cfg.ReconnectMaxDelay
				}
				continue
			}
			logger.Info("cli worker reconnected")
			break
		}
	}
}

// runWithStableDiffusion runs the image-generation path: downloads sd-server,
// spawns it with the diffusion model, and connects to the router.
func runWithStableDiffusion(ctx context.Context, cfg *worker.Config, conn *worker.Connection, policy *protocol.Policy, modelPath string, regModel *registry.Model, logger *slog.Logger) error {
	// Download stable-diffusion.cpp binary
	sdCacheDir := ""
	if home, err := os.UserHomeDir(); err == nil {
		sdCacheDir = home + "/.auxot/sd-server"
	}
	binaryPath, err := sdbin.Ensure(sdCacheDir, logger)
	if err != nil {
		return fmt.Errorf("ensuring sd-server binary: %w", err)
	}
	logger.Info("sd-server binary", "path", binaryPath)

	sdPort, err := worker.FindFreePort()
	if err != nil {
		return fmt.Errorf("finding free port: %w", err)
	}

	// FLUX models need auxiliary files (VAE + text encoders)
	var vaePath, llmPath, clipLPath, t5xxlPath string
	prediction := "flux2_flow"
	if regModel != nil && strings.Contains(strings.ToLower(regModel.ModelName), "flux.2-klein") {
		is4B := strings.Contains(strings.ToLower(regModel.ModelName), "4b")
		aux, err := modeldown.EnsureFlux2KleinAuxiliary(ctx, cfg.ModelsDir, is4B, logger)
		if err != nil {
			return fmt.Errorf("downloading FLUX.2-klein VAE/LLM: %w", err)
		}
		vaePath = aux.VAEPath
		llmPath = aux.LLMPath
	} else if regModel != nil && strings.Contains(strings.ToLower(regModel.ModelName), "flux.1-schnell") {
		prediction = "flux_flow"
		aux, err := modeldown.EnsureFlux1SchnellAuxiliary(ctx, cfg.ModelsDir, logger)
		if err != nil {
			return fmt.Errorf("downloading FLUX.1-schnell VAE/text encoders: %w", err)
		}
		vaePath = aux.VAEPath
		clipLPath = aux.ClipLPath
		t5xxlPath = aux.T5xxlPath
	}

	sdProc := worker.NewSDProcess(worker.SDOpts{
		BinaryPath:     binaryPath,
		DiffusionModel: modelPath,
		VAEPath:        vaePath,
		LLMPath:        llmPath,
		ClipLPath:      clipLPath,
		T5xxlPath:      t5xxlPath,
		Port:           sdPort,
		Host:           "127.0.0.1",
		Prediction:     prediction,
		OffloadToCPU:   true, // Safer for memory-constrained systems
		DiffusionFlash: true,
	}, logger)

	if err := sdProc.Start(); err != nil {
		return fmt.Errorf("starting sd-server: %w", err)
	}
	defer sdProc.Stop()

	logger.Info("sd-server started", "port", sdPort)
	logger.Info("waiting for sd-server")

	if err := sdProc.WaitForReady(ctx, 10*time.Minute); err != nil {
		return fmt.Errorf("sd-server not ready: %w", err)
	}

	params := "4B"
	if regModel != nil && regModel.Parameters != "" {
		params = regModel.Parameters
	}
	caps := sdProc.DiscoverCapabilities(policy.ModelName, params)

	logger.Info("capabilities",
		"model", caps.Model,
		"backend", caps.Backend,
		"parameters", caps.Parameters,
	)

	if err := conn.ConnectPermanent(caps); err != nil {
		return fmt.Errorf("permanent connection: %w", err)
	}
	defer conn.Close()

	go func() {
		<-ctx.Done()
		conn.Close()
	}()

	logger.Info("worker ready (image generation)", "gpu_id", conn.GPUID())

	executor := worker.NewImageExecutor(sdProc.URL(), cfg.JobTimeout, logger)
	activeJobs := &sync.Map{}

	conn.OnJob(func(job protocol.JobMessage) {
		abortCtx, cancel := context.WithCancel(ctx)
		activeJobs.Store(job.JobID, cancel)
		defer func() {
			activeJobs.Delete(job.JobID)
			cancel()
		}()

		executor.Execute(
			abortCtx,
			job,
			func(token string) error { return conn.SendToken(job.JobID, token) },
			func(token string) error { return conn.SendReasoningToken(job.JobID, token) },
			func() error { return conn.SendToolGenerating(job.JobID) },
			func(fullResponse, reasoningContent string, cacheTokens, inputTokens, outputTokens, reasoningTokens int, durationMS int64, toolCalls []protocol.ToolCall) error {
				return conn.SendComplete(job.JobID, fullResponse, "", reasoningContent, durationMS, cacheTokens, inputTokens, outputTokens, reasoningTokens, 0, "", "", 0, "", toolCalls, nil)
			},
			func(errMsg, details string) error {
				return conn.SendError(job.JobID, appendErrorDetails(errMsg, details))
			},
			nil,
		)
	})

	conn.OnCancel(func(jobID string) {
		if cancel, ok := activeJobs.Load(jobID); ok {
			cancel.(context.CancelFunc)()
		}
	})

	sdProc.OnCrash(func(exitCode int, err error) {
		logger.Error("sd-server crashed", "exit_code", exitCode, "error", err)
		logger.Info("restarting sd-server")
		time.Sleep(2 * time.Second)
		if restartErr := sdProc.Start(); restartErr != nil {
			logger.Error("failed to restart sd-server", "error", restartErr)
			return
		}
		if err := sdProc.WaitForReady(ctx, 10*time.Minute); err != nil {
			logger.Error("restarted sd-server not ready", "error", err)
			return
		}
		logger.Info("sd-server recovered")
	})

	for {
		err := conn.RunMessageLoop()
		if ctx.Err() != nil {
			logger.Info("shutting down")
			return nil
		}
		if err != nil {
			logger.Warn("disconnected", "error", err.Error())
		}

		delay := cfg.ReconnectDelay
		for {
			if ctx.Err() != nil {
				return nil
			}
			logger.Info("reconnecting", "delay", delay.String())
			select {
			case <-ctx.Done():
				return nil
			case <-time.After(delay):
			}

			if err := conn.ConnectPermanent(caps); err != nil {
				logger.Warn("reconnect failed", "error", err.Error())
				delay *= 2
				if delay > cfg.ReconnectMaxDelay {
					delay = cfg.ReconnectMaxDelay
				}
				continue
			}
			logger.Info("reconnected", "gpu_id", conn.GPUID())
			break
		}
	}
}

// runWithExternalLlama skips model/binary download and llama.cpp spawning,
// connecting directly to an already-running llama-server at externalURL.
// This is the code path when AUXOT_LLAMA_URL is set — used in dev setups
// where llama.cpp is managed separately (e.g. via Tilt or a local script).
func runWithExternalLlama(ctx context.Context, cfg *worker.Config, conn *worker.Connection, externalURL string, logger *slog.Logger) error {
	// Discover capabilities from the running server
	extLlama := worker.NewLlamaProcess(worker.LlamaOpts{Port: 0}, logger)
	caps, err := extLlama.DiscoverCapabilitiesFromURL(externalURL)
	if err != nil {
		return fmt.Errorf("discovering capabilities from external llama: %w", err)
	}

	logger.Info("external llama.cpp capabilities",
		"model", caps.Model,
		"backend", caps.Backend,
		"ctx_size", caps.CtxSize,
		"total_slots", caps.TotalSlots,
	)

	// Permanent connection with discovered capabilities
	if err := conn.ConnectPermanent(caps); err != nil {
		return fmt.Errorf("permanent connection: %w", err)
	}
	defer conn.Close()

	go func() {
		<-ctx.Done()
		conn.Close()
	}()

	logger.Info("worker ready (external llama)", "gpu_id", conn.GPUID())

	executor := worker.NewExecutor(externalURL, cfg.JobTimeout, false, "", logger)
	activeJobs := &sync.Map{}

	conn.OnJob(func(job protocol.JobMessage) {
		abortCtx, cancel := context.WithCancel(ctx)
		activeJobs.Store(job.JobID, cancel)
		defer func() {
			activeJobs.Delete(job.JobID)
			cancel()
		}()

		executor.Execute(
			abortCtx,
			job,
			func(token string) error { return conn.SendToken(job.JobID, token) },
			func(token string) error { return conn.SendReasoningToken(job.JobID, token) },
			func() error { return conn.SendToolGenerating(job.JobID) },
			func(fullResponse, reasoningContent string, cacheTokens, inputTokens, outputTokens, reasoningTokens int, durationMS int64, toolCalls []protocol.ToolCall) error {
				return conn.SendComplete(job.JobID, fullResponse, "", reasoningContent, durationMS, cacheTokens, inputTokens, outputTokens, reasoningTokens, 0, "", "", 0, "", toolCalls, nil)
			},
			func(errMsg, details string) error {
				return conn.SendError(job.JobID, appendErrorDetails(errMsg, details))
			},
			func(total, cached, processed int) error {
				return conn.SendPromptProgress(job.JobID, total, cached, processed)
			},
		)
	})

	conn.OnCancel(func(jobID string) {
		if cancel, ok := activeJobs.Load(jobID); ok {
			cancel.(context.CancelFunc)()
		}
	})

	<-ctx.Done()
	return nil
}

func printHelp() {
	fmt.Println(`auxot-worker — GPU inference worker

Usage:
  auxot-worker                Connect to router and start processing jobs
  auxot-worker install        Install as a persistent daemon (runs on boot)
  auxot-worker list           List installed workers
  auxot-worker uninstall      Remove an installed worker
  auxot-worker version        Print version
  auxot-worker help           Print this help

Daemon Management:
  auxot-worker install --name <name> --gpu-key <key> [--router-url <url>] [--always-on]
      Install worker as a system daemon or user-session agent.
      Named installs allow multiple workers with different GPU keys/models.

  auxot-worker list
      Show all installed workers with their status, GPU key (masked), and router URL.

  auxot-worker uninstall <name>
      Stop and remove a named worker install.

  Examples:
    auxot-worker install --name qwen   --gpu-key gpu_abc123
    auxot-worker install --name llama  --gpu-key gpu_xyz789 --always-on
    auxot-worker list
    auxot-worker uninstall qwen

Flags (run mode):
  --gpu-key <key>             Admin key for authentication (overrides AUXOT_GPU_KEY)
  --router-url <url>          Router WebSocket URL (overrides AUXOT_ROUTER_URL)
  --model-path <path>         Path to local GGUF model file (overrides AUXOT_MODEL_FILE)
                              Skips model download — for air-gapped deployments
  --llama-server-path <path>  Path to local llama-server binary (overrides AUXOT_LLAMA_BINARY)
                              Skips binary download — for air-gapped deployments
  --debug [level]             Enable debug logging (level 1 or 2, default: 1)
                              Level 1: WebSocket messages (router <-> worker) - smart/collapsed
                              Level 2: Level 1 + full llama.cpp requests/responses + per-token output

Environment Variables:
  AUXOT_ROUTER_URL            Router WebSocket URL (default: wss://auxot.com/api/gpu/client)
  AUXOT_GPU_KEY               Admin key (required, gpu_... from Auxot dashboard)
  AUXOT_MODEL_FILE            Path to local GGUF file (air-gapped, skip download)
  AUXOT_MODELS_DIR            Model cache dir (default: ~/.auxot/models)
  AUXOT_LLAMA_URL             External llama-server URL (skip download+spawn, e.g. http://localhost:9002)
  AUXOT_LLAMA_CACHE_DIR       llama.cpp cache dir (default: ~/.auxot/llama-server)
  AUXOT_LLAMA_BINARY          Path to llama-server binary (skip download)
  AUXOT_GPU_LAYERS            GPU layers to offload (default: 9999 = all)
  AUXOT_THREADS               CPU threads for llama.cpp (default: auto)
  AUXOT_HEARTBEAT_INTERVAL    Heartbeat interval (default: 10s)
  AUXOT_RECONNECT_DELAY       Initial reconnect delay (default: 2s)
  AUXOT_RECONNECT_MAX_DELAY   Max reconnect backoff (default: 60s)
  AUXOT_JOB_TIMEOUT           Max job duration (default: 5m)
  AUXOT_LOG_LEVEL             Log level: debug, info, warn, error (default: info)

Air-Gapped Deployment:
  For deployments without internet access, provide both a local model file and
  a local llama-server binary. The worker will skip all downloads and use the
  provided paths directly.

  Example:
    auxot-worker \
      --gpu-key gpu_xxx \
      --router-url ws://router.internal:8080/ws \
      --model-path /opt/models/qwen3-8b-Q4_K_M.gguf \
      --llama-server-path /opt/bin/llama-server`)
}

func slogLevel() slog.Level {
	switch os.Getenv("AUXOT_LOG_LEVEL") {
	case "debug":
		return slog.LevelDebug
	case "warn":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}
