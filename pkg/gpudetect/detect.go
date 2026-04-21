// Package gpudetect identifies GPU hardware to determine which llama.cpp
// binary variant to download and which acceleration backend to use.
//
// Detection order:
//   - macOS: Metal (built-in) — always available on Apple Silicon
//   - Linux: NVIDIA (nvidia-smi) → Vulkan → CPU fallback
//   - Windows: NVIDIA CUDA → CPU fallback
//
// Apple Silicon vs Intel Mac distinction:
//
// Both Apple Silicon and Intel Macs report BackendMetal, because Metal is
// supported on all modern macOS hardware. However, only Apple Silicon (arm64)
// supports the MLX inference stack (vllm-mlx / mlx-lm). Intel Macs running
// darwin/amd64 have Metal but lack the Neural Engine and the MLX-specific
// kernels; attempting to run vllm-mlx on Intel would either fail or fall back
// to CPU-only, defeating the purpose. Use IsAppleSilicon() or
// Result.IsAppleSilicon to gate the MLX path.
package gpudetect

import (
	"os/exec"
	"runtime"
	"strings"
	"time"
)

// Backend identifies the GPU acceleration backend for llama.cpp.
type Backend string

const (
	BackendMetal  Backend = "metal"
	BackendCUDA   Backend = "cuda"
	BackendVulkan Backend = "vulkan"
	BackendCPU    Backend = "cpu"
)

// Result holds the detection outcome.
type Result struct {
	Backend        Backend // Which backend to use
	Detected       bool    // Whether an actual GPU was found
	Warning        string  // Non-empty if falling back to CPU
	IsAppleSilicon bool    // True only on darwin/arm64 — required for the MLX path
}

// IsAppleSilicon reports whether the current process is running on Apple
// Silicon (darwin/arm64). This is the guard for the vllm-mlx / mlx-lm
// inference path.
//
// Intel Macs (darwin/amd64) expose Metal and will have Detected=true and
// Backend=BackendMetal, but IsAppleSilicon returns false there. The MLX
// stack is arm64-only and must NOT be selected solely because Metal is
// available — always check IsAppleSilicon() before starting vllm-mlx.
func IsAppleSilicon() bool {
	return runtime.GOOS == "darwin" && runtime.GOARCH == "arm64"
}

// Detect identifies the GPU hardware on the current system.
func Detect() Result {
	switch runtime.GOOS {
	case "darwin":
		return detectMacOS()
	case "linux":
		return detectLinux()
	case "windows":
		return detectWindows()
	default:
		return Result{
			Backend:  BackendCPU,
			Detected: false,
			Warning:  "Unknown platform " + runtime.GOOS + ". Defaulting to CPU.",
		}
	}
}

func detectMacOS() Result {
	// All modern macOS hardware includes Metal (llama.cpp uses it automatically).
	// IsAppleSilicon is true only on arm64 and gates the vllm-mlx path.
	return Result{
		Backend:        BackendMetal,
		Detected:       true,
		IsAppleSilicon: runtime.GOARCH == "arm64",
	}
}

func detectLinux() Result {
	// 1. Try NVIDIA via nvidia-smi
	if commandSucceeds("nvidia-smi", "--query-gpu=name", "--format=csv,noheader") {
		// NVIDIA GPU detected. llama.cpp Linux releases don't ship CUDA binaries,
		// but Vulkan works on NVIDIA GPUs too.
		return Result{Backend: BackendVulkan, Detected: true}
	}

	// 2. Try Vulkan
	if commandSucceeds("vulkaninfo", "--summary") {
		return Result{Backend: BackendVulkan, Detected: true}
	}

	// 3. Check lspci for any VGA controller
	if out, err := runCommand("lspci"); err == nil {
		lower := strings.ToLower(out)
		if strings.Contains(lower, "vga") || strings.Contains(lower, "3d controller") {
			return Result{Backend: BackendVulkan, Detected: true}
		}
	}

	return Result{
		Backend:  BackendCPU,
		Detected: false,
		Warning:  "No GPU detected. Using CPU (performance will be severely limited). Consider <= 7B models.",
	}
}

func detectWindows() Result {
	// 1. Try NVIDIA CUDA
	if commandSucceeds("nvidia-smi", "--query-gpu=name", "--format=csv,noheader") {
		return Result{Backend: BackendCUDA, Detected: true}
	}

	return Result{
		Backend:  BackendCPU,
		Detected: false,
		Warning:  "No NVIDIA GPU detected. Using CPU (performance will be severely limited).",
	}
}

// commandSucceeds runs a command and returns true if it exits with code 0.
func commandSucceeds(name string, args ...string) bool {
	cmd := exec.Command(name, args...)
	cmd.Stdout = nil
	cmd.Stderr = nil
	done := make(chan error, 1)
	go func() { done <- cmd.Run() }()

	select {
	case err := <-done:
		return err == nil
	case <-time.After(5 * time.Second):
		if cmd.Process != nil {
			_ = cmd.Process.Kill()
		}
		return false
	}
}

// runCommand runs a command and returns its stdout.
func runCommand(name string, args ...string) (string, error) {
	cmd := exec.Command(name, args...)
	out, err := cmd.Output()
	return string(out), err
}
