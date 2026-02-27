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

	"github.com/auxothq/auxot/internal/worker"
	"github.com/auxothq/auxot/pkg/gpudetect"
	"github.com/auxothq/auxot/pkg/llamabin"
	"github.com/auxothq/auxot/pkg/logutil"
	"github.com/auxothq/auxot/pkg/modeldown"
	"github.com/auxothq/auxot/pkg/protocol"
	"github.com/auxothq/auxot/pkg/registry"
	"github.com/auxothq/auxot/pkg/sdbin"
)

func main() {
	_ = godotenv.Load()

	// Manual argument parsing (--debug has optional value that flag package can't handle)
	var flags worker.CLIFlags
	debugLevel := 0

	args := os.Args[1:]
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
		"model", policy.ModelName,
		"quantization", policy.Quantization,
		"context_size", policy.ContextSize,
		"max_parallelism", policy.MaxParallelism,
	)

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
	// Phase 2: Download model
	// ----------------------------------------------------------------
	modelResult, err := modeldown.Ensure(ctx, policy, cfg.ModelsDir, cfg.ModelFile, logger)
	if err != nil {
		return fmt.Errorf("ensuring model: %w", err)
	}
	modelPath := modelResult.ModelPath
	mmprojPath := modelResult.MmprojPath
	logger.Info("model ready", "path", modelPath, "mmproj", mmprojPath != "")

	// Check if this is an image generation model — use stable-diffusion.cpp path
	reg, err := registry.Load()
	if err != nil {
		return fmt.Errorf("loading registry: %w", err)
	}
	isImageGen := false
	var regModel *registry.Model
	if m := reg.FindByNameAndQuant(policy.ModelName, policy.Quantization); m != nil {
		for _, cap := range m.Capabilities {
			if cap == "image_generation" {
				isImageGen = true
				regModel = m
				break
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

	defer llama.Stop()

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
	executor := worker.NewExecutor(llama.URL(), cfg.JobTimeout, logger)
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
				return conn.SendComplete(job.JobID, fullResponse, reasoningContent, durationMS, cacheTokens, inputTokens, outputTokens, reasoningTokens, toolCalls)
			},
			func(errMsg, details string) error {
				return conn.SendError(job.JobID, errMsg)
			},
		)
	})

	conn.OnCancel(func(jobID string) {
		if cancel, ok := activeJobs.Load(jobID); ok {
			cancel.(context.CancelFunc)()
		}
	})

	// Handle llama.cpp crash → auto-restart
	llama.OnCrash(func(exitCode int, err error) {
		logger.Error("llama.cpp crashed", "exit_code", exitCode, "error", err)
		logger.Info("restarting llama.cpp")
		time.Sleep(2 * time.Second)

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
				return conn.SendComplete(job.JobID, fullResponse, reasoningContent, durationMS, cacheTokens, inputTokens, outputTokens, reasoningTokens, toolCalls)
			},
			func(errMsg, details string) error { return conn.SendError(job.JobID, errMsg) },
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

	executor := worker.NewExecutor(externalURL, cfg.JobTimeout, logger)
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
				return conn.SendComplete(job.JobID, fullResponse, reasoningContent, durationMS, cacheTokens, inputTokens, outputTokens, reasoningTokens, toolCalls)
			},
			func(errMsg, details string) error { return conn.SendError(job.JobID, errMsg) },
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
  auxot-worker version        Print version
  auxot-worker help           Print this help

Flags:
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
  AUXOT_GPU_KEY               Admin key (required, adm_... from router setup)
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
      --gpu-key adm_xxx \
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
