// Package modeldown downloads GGUF model files from HuggingFace.
//
// Features:
//   - Resolves model → HuggingFace URL via the embedded model registry
//   - Caches downloads to ~/.auxot/models/{repo_id}/{file_name}
//   - Supports resumable downloads (HTTP Range requests)
//   - Supports split/sharded GGUF models (e.g., 480B models with 6+ shards)
//   - Supports air-gapped mode (local GGUF file path)
package modeldown

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"github.com/auxothq/auxot/pkg/protocol"
	"github.com/auxothq/auxot/pkg/registry"
)

// splitPattern matches GGUF split filenames like "Name-00001-of-00006.gguf".
var splitPattern = regexp.MustCompile(`^(.*)-(\d{5})-of-(\d{5})(\.gguf)$`)

// EnsureResult holds the paths returned by Ensure.
type EnsureResult struct {
	ModelPath  string // Path to primary GGUF model file (first shard for split models)
	MmprojPath string // Path to mmproj (vision encoder), empty if model has no vision
}

// Ensure returns the path to the primary GGUF model file, downloading it if necessary.
//
// For split/sharded models (e.g., 480B), ALL shards are downloaded but only the
// first shard path is returned — llama.cpp auto-discovers the rest.
//
// If modelFile is non-empty, it's treated as a local path (air-gapped mode).
// For split models in air-gapped mode, all sibling shards must also be present.
//
// Otherwise, the model is resolved from the registry using the policy and
// downloaded from HuggingFace to modelsDir.
func Ensure(ctx context.Context, policy *protocol.Policy, modelsDir, modelFile string, logger *slog.Logger) (*EnsureResult, error) {
	// Air-gapped mode: use local file
	if modelFile != "" {
		modelPath, err := ensureLocalModel(modelFile, logger)
		if err != nil {
			return nil, err
		}
		return &EnsureResult{ModelPath: modelPath}, nil
	}

	// Resolve from registry
	reg, err := registry.Load()
	if err != nil {
		return nil, fmt.Errorf("loading model registry: %w", err)
	}

	entry := reg.FindByNameAndQuant(policy.ModelName, policy.Quantization)
	if entry == nil {
		return nil, fmt.Errorf("model not found in registry: %s (%s)", policy.ModelName, policy.Quantization)
	}

	// Build cache base path
	if modelsDir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return nil, fmt.Errorf("getting home directory: %w", err)
		}
		modelsDir = filepath.Join(home, ".auxot", "models")
	}

	repoDir := strings.ReplaceAll(entry.HuggingFaceID, "/", "_")
	baseDir := filepath.Join(modelsDir, repoDir)

	// Get all shard filenames (single-file models return a list of one)
	shardNames := entry.ShardFileNames()

	if len(shardNames) > 1 {
		logger.Info("split model detected",
			"model", entry.ModelName,
			"quantization", entry.Quantization,
			"shards", len(shardNames),
		)
	}

	// Download each shard
	for i, shardName := range shardNames {
		shardPath := filepath.Join(baseDir, shardName)

		// For split models, we can't verify individual shard sizes from the registry
		// (FileSizeBytes is total or unset). Just verify files exist and are non-zero.
		// For single-file models, use FileSizeBytes if available.
		var expectedSize int64
		if len(shardNames) == 1 && entry.FileSizeBytes != nil {
			expectedSize = *entry.FileSizeBytes
		}

		// Check if already downloaded
		if info, err := os.Stat(shardPath); err == nil && info.Size() > 0 {
			if expectedSize > 0 && info.Size() == expectedSize {
				logger.Info("shard already downloaded",
					"shard", fmt.Sprintf("%d/%d", i+1, len(shardNames)),
					"path", shardPath,
					"size", info.Size(),
				)
				continue
			}
			if expectedSize == 0 {
				logger.Info("shard already downloaded (size unverified)",
					"shard", fmt.Sprintf("%d/%d", i+1, len(shardNames)),
					"path", shardPath,
				)
				continue
			}
			logger.Warn("shard size mismatch, re-downloading",
				"shard", fmt.Sprintf("%d/%d", i+1, len(shardNames)),
				"expected", expectedSize,
				"actual", info.Size(),
			)
		}

		// Build download URL
		downloadURL := fmt.Sprintf("https://huggingface.co/%s/resolve/main/%s",
			entry.HuggingFaceID, shardName)

		// Include filesize in the log if known
		downloadAttrs := []any{
			"model", entry.ModelName,
			"quantization", entry.Quantization,
			"shard", fmt.Sprintf("%d/%d", i+1, len(shardNames)),
			"file", shardName,
		}
		if expectedSize > 0 {
			downloadAttrs = append(downloadAttrs, "size", formatBytes(expectedSize))
		}
		logger.Info("downloading shard", downloadAttrs...)

		if err := os.MkdirAll(filepath.Dir(shardPath), 0o755); err != nil {
			return nil, fmt.Errorf("creating model directory: %w", err)
		}

		if err := downloadWithProgress(ctx, downloadURL, shardPath, expectedSize, logger); err != nil {
			return nil, fmt.Errorf("downloading shard %d/%d: %w", i+1, len(shardNames), err)
		}
	}

	// Download mmproj (vision encoder) if the model has one
	var mmprojPath string
	if entry.MmprojFileName != "" {
		mmprojPath = filepath.Join(baseDir, entry.MmprojFileName)
		if info, err := os.Stat(mmprojPath); err != nil || info.Size() == 0 {
			downloadURL := fmt.Sprintf("https://huggingface.co/%s/resolve/main/%s",
				entry.HuggingFaceID, entry.MmprojFileName)
			logger.Info("downloading mmproj (vision encoder)", "file", entry.MmprojFileName)
			if err := downloadWithProgress(ctx, downloadURL, mmprojPath, 0, logger); err != nil {
				return nil, fmt.Errorf("downloading mmproj: %w", err)
			}
			logger.Info("mmproj ready", "path", mmprojPath)
		} else {
			logger.Info("mmproj already downloaded", "path", mmprojPath)
		}
	}

	// Return the first shard path — llama.cpp auto-discovers the rest
	primaryPath := filepath.Join(baseDir, shardNames[0])
	logger.Info("model ready",
		"path", primaryPath,
		"shards", len(shardNames),
		"mmproj", mmprojPath != "",
	)
	return &EnsureResult{ModelPath: primaryPath, MmprojPath: mmprojPath}, nil
}

// ensureLocalModel validates a local model file for air-gapped mode.
// For split models, it verifies ALL sibling shards exist in the same directory.
func ensureLocalModel(modelFile string, logger *slog.Logger) (string, error) {
	if _, err := os.Stat(modelFile); err != nil {
		return "", fmt.Errorf("model file not found: %s", modelFile)
	}

	// Check if this is a split model by examining the filename
	base := filepath.Base(modelFile)
	matches := splitPattern.FindStringSubmatch(base)
	if matches == nil {
		// Single file — done
		logger.Info("using local model file", "path", modelFile)
		return modelFile, nil
	}

	// Split model — verify all shards exist
	prefix := matches[1]
	total, _ := strconv.Atoi(matches[3])
	ext := matches[4]
	dir := filepath.Dir(modelFile)

	logger.Info("local split model detected",
		"path", modelFile,
		"shards", total,
	)

	var missing []string
	for i := 1; i <= total; i++ {
		shardName := fmt.Sprintf("%s-%05d-of-%05d%s", prefix, i, total, ext)
		shardPath := filepath.Join(dir, shardName)
		if _, err := os.Stat(shardPath); err != nil {
			missing = append(missing, shardName)
		}
	}

	if len(missing) > 0 {
		return "", fmt.Errorf("split model is incomplete — %d of %d shards missing in %s:\n  %s",
			len(missing), total, dir, strings.Join(missing, "\n  "))
	}

	logger.Info("all shards verified", "path", dir, "shards", total)
	return modelFile, nil
}

// downloadWithProgress downloads a URL to a file with structured log output.
// Progress is reported at 20% intervals to avoid log noise on large files.
// The context is used for cancellation — Ctrl+C will abort the download immediately.
func downloadWithProgress(ctx context.Context, url, dest string, expectedSize int64, logger *slog.Logger) error {
	// Check for partial download to support resume
	var startByte int64
	tempPath := dest + ".download"

	if info, err := os.Stat(tempPath); err == nil {
		startByte = info.Size()
	}

	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return err
	}
	if startByte > 0 {
		req.Header.Set("Range", fmt.Sprintf("bytes=%d-", startByte))
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusRequestedRangeNotSatisfiable {
		// File already complete
		return os.Rename(tempPath, dest)
	}

	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusPartialContent {
		return fmt.Errorf("HTTP %d: %s", resp.StatusCode, resp.Status)
	}

	// Determine total size: prefer expectedSize, fall back to Content-Length.
	// For split models the registry doesn't have per-shard sizes, so
	// Content-Length from the server is our primary source.
	totalSize := expectedSize
	if cl := resp.Header.Get("Content-Length"); cl != "" {
		if parsed, parseErr := strconv.ParseInt(cl, 10, 64); parseErr == nil {
			remaining := parsed
			serverTotal := remaining + startByte
			if totalSize == 0 {
				totalSize = serverTotal
			}
		}
	}

	// Now that we know the total, log resume or size info
	if startByte > 0 {
		resumeAttrs := []any{"resuming_from", formatBytes(startByte)}
		if totalSize > 0 {
			resumeAttrs = append(resumeAttrs,
				"total", formatBytes(totalSize),
				"percent", fmt.Sprintf("%d%%", int(float64(startByte)/float64(totalSize)*100)),
			)
		}
		logger.Info("resuming download", resumeAttrs...)
	} else if totalSize > 0 && expectedSize == 0 {
		// We didn't know size upfront but server told us — log it
		logger.Info("shard size", "total", formatBytes(totalSize))
	}

	// Open file for writing (append if resuming)
	flags := os.O_CREATE | os.O_WRONLY
	if startByte > 0 && resp.StatusCode == http.StatusPartialContent {
		flags |= os.O_APPEND
	} else {
		flags |= os.O_TRUNC
		startByte = 0
	}

	f, err := os.OpenFile(tempPath, flags, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()

	// Copy with progress reporting.
	// If we know total size: report at 20% milestones.
	// If we don't: report every 1 GB so downloads aren't silent.
	downloaded := startByte
	buf := make([]byte, 64*1024)    // 64KB buffer
	lastMilestone := -1             // last reported 20% milestone (0-4)
	const unknownInterval int64 = 1 << 30 // 1 GB
	lastUnknownReport := startByte

	for {
		n, readErr := resp.Body.Read(buf)
		if n > 0 {
			if _, writeErr := f.Write(buf[:n]); writeErr != nil {
				return writeErr
			}
			downloaded += int64(n)

			if totalSize > 0 {
				// Report at 20% milestones (20%, 40%, 60%, 80%)
				milestone := int(float64(downloaded) / float64(totalSize) * 5) // 0-5
				if milestone > lastMilestone && milestone < 5 {
					percent := milestone * 20
					logger.Info("download progress",
						"downloaded", formatBytes(downloaded),
						"total", formatBytes(totalSize),
						"percent", fmt.Sprintf("%d%%", percent),
					)
					lastMilestone = milestone
				}
			} else {
				// Unknown total: report every 1 GB
				if downloaded-lastUnknownReport >= unknownInterval {
					logger.Info("download progress",
						"downloaded", formatBytes(downloaded),
					)
					lastUnknownReport = downloaded
				}
			}
		}
		if readErr == io.EOF {
			break
		}
		if readErr != nil {
			return readErr
		}
	}

	// Rename temp to final
	if err := os.Rename(tempPath, dest); err != nil {
		return err
	}

	logger.Info("download complete", "size", formatBytes(downloaded))
	return nil
}

// Flux2AuxiliaryPaths holds paths to VAE and LLM for FLUX.2-klein models.
type Flux2AuxiliaryPaths struct {
	VAEPath string
	LLMPath string
}

// EnsureFlux2KleinAuxiliary downloads VAE and LLM for FLUX.2-klein models.
// FLUX.2-klein requires these in addition to the diffusion model.
// is4B: true for 4B variant (Qwen3-4B), false for 9B (Qwen3-8B).
func EnsureFlux2KleinAuxiliary(ctx context.Context, modelsDir string, is4B bool, logger *slog.Logger) (*Flux2AuxiliaryPaths, error) {
	if modelsDir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return nil, fmt.Errorf("getting home directory: %w", err)
		}
		modelsDir = filepath.Join(home, ".auxot", "models")
	}

	// VAE: shared by all FLUX.2 models. Use Comfy-Org mirror (ungated) — black-forest-labs/FLUX.2-dev is gated.
	vaeDir := filepath.Join(modelsDir, "Comfy-Org_flux2-dev")
	vaePath := filepath.Join(vaeDir, "flux2-vae.safetensors")
	if info, err := os.Stat(vaePath); err != nil || info.Size() == 0 {
		os.MkdirAll(vaeDir, 0o755)
		url := "https://huggingface.co/Comfy-Org/flux2-dev/resolve/main/split_files/vae/flux2-vae.safetensors"
		logger.Info("downloading FLUX.2 VAE", "file", "flux2-vae.safetensors")
		if err := downloadWithProgress(ctx, url, vaePath, 0, logger); err != nil {
			return nil, fmt.Errorf("downloading VAE: %w", err)
		}
		logger.Info("VAE ready", "path", vaePath)
	} else {
		logger.Info("VAE already downloaded", "path", vaePath)
	}

	// LLM: Qwen3-4B for 4B variant, Qwen3-8B for 9B (filenames are case-sensitive on HuggingFace)
	var llmRepo, llmFile string
	if is4B {
		llmRepo = "unsloth/Qwen3-4B-GGUF"
		llmFile = "Qwen3-4B-Q4_K_S.gguf"
	} else {
		llmRepo = "unsloth/Qwen3-8B-GGUF"
		llmFile = "Qwen3-8B-Q4_K_S.gguf"
	}
	llmDir := filepath.Join(modelsDir, strings.ReplaceAll(llmRepo, "/", "_"))
	llmPath := filepath.Join(llmDir, llmFile)
	if info, err := os.Stat(llmPath); err != nil || info.Size() == 0 {
		os.MkdirAll(llmDir, 0o755)
		url := fmt.Sprintf("https://huggingface.co/%s/resolve/main/%s", llmRepo, llmFile)
		logger.Info("downloading FLUX.2-klein LLM (text encoder)", "file", llmFile)
		if err := downloadWithProgress(ctx, url, llmPath, 0, logger); err != nil {
			return nil, fmt.Errorf("downloading LLM: %w", err)
		}
		logger.Info("LLM ready", "path", llmPath)
	} else {
		logger.Info("LLM already downloaded", "path", llmPath)
	}

	return &Flux2AuxiliaryPaths{VAEPath: vaePath, LLMPath: llmPath}, nil
}

// Flux1SchnellAuxiliaryPaths holds paths to VAE, clip_l, and t5xxl for FLUX.1-schnell.
type Flux1SchnellAuxiliaryPaths struct {
	VAEPath   string
	ClipLPath string
	T5xxlPath string
}

// EnsureFlux1SchnellAuxiliary downloads VAE, clip_l, and t5xxl for FLUX.1-schnell.
// FLUX.1-schnell uses the same text encoders as FLUX.1-dev (clip_l + t5xxl).
func EnsureFlux1SchnellAuxiliary(ctx context.Context, modelsDir string, logger *slog.Logger) (*Flux1SchnellAuxiliaryPaths, error) {
	if modelsDir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return nil, fmt.Errorf("getting home directory: %w", err)
		}
		modelsDir = filepath.Join(home, ".auxot", "models")
	}

	// VAE: shared with FLUX.1-dev (black-forest-labs/FLUX.1-dev)
	vaeDir := filepath.Join(modelsDir, "black-forest-labs_FLUX.1-dev")
	vaePath := filepath.Join(vaeDir, "ae.safetensors")
	if info, err := os.Stat(vaePath); err != nil || info.Size() == 0 {
		os.MkdirAll(vaeDir, 0o755)
		url := "https://huggingface.co/black-forest-labs/FLUX.1-dev/resolve/main/ae.safetensors"
		logger.Info("downloading FLUX.1 VAE", "file", "ae.safetensors")
		if err := downloadWithProgress(ctx, url, vaePath, 0, logger); err != nil {
			return nil, fmt.Errorf("downloading VAE: %w", err)
		}
		logger.Info("VAE ready", "path", vaePath)
	} else {
		logger.Info("VAE already downloaded", "path", vaePath)
	}

	// clip_l and t5xxl: from comfyanonymous/flux_text_encoders
	encDir := filepath.Join(modelsDir, "comfyanonymous_flux_text_encoders")
	clipLPath := filepath.Join(encDir, "clip_l.safetensors")
	t5xxlPath := filepath.Join(encDir, "t5xxl_fp16.safetensors")

	for _, spec := range []struct {
		path string
		url  string
		name string
	}{
		{clipLPath, "https://huggingface.co/comfyanonymous/flux_text_encoders/resolve/main/clip_l.safetensors", "clip_l"},
		{t5xxlPath, "https://huggingface.co/comfyanonymous/flux_text_encoders/resolve/main/t5xxl_fp16.safetensors", "t5xxl"},
	} {
		if info, err := os.Stat(spec.path); err != nil || info.Size() == 0 {
			os.MkdirAll(encDir, 0o755)
			logger.Info("downloading FLUX.1 text encoder", "file", spec.name)
			if err := downloadWithProgress(ctx, spec.url, spec.path, 0, logger); err != nil {
				return nil, fmt.Errorf("downloading %s: %w", spec.name, err)
			}
			logger.Info("text encoder ready", "path", spec.path)
		} else {
			logger.Info("text encoder already downloaded", "path", spec.path)
		}
	}

	return &Flux1SchnellAuxiliaryPaths{VAEPath: vaePath, ClipLPath: clipLPath, T5xxlPath: t5xxlPath}, nil
}

func formatBytes(b int64) string {
	const kb = 1024
	const mb = kb * 1024
	const gb = mb * 1024

	switch {
	case b >= gb:
		return fmt.Sprintf("%.1f GB", float64(b)/float64(gb))
	case b >= mb:
		return fmt.Sprintf("%.1f MB", float64(b)/float64(mb))
	case b >= kb:
		return fmt.Sprintf("%.1f KB", float64(b)/float64(kb))
	default:
		return fmt.Sprintf("%d B", b)
	}
}
