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
func Ensure(ctx context.Context, policy *protocol.Policy, modelsDir, modelFile string, logger *slog.Logger) (string, error) {
	// Air-gapped mode: use local file
	if modelFile != "" {
		return ensureLocalModel(modelFile, logger)
	}

	// Resolve from registry
	reg, err := registry.Load()
	if err != nil {
		return "", fmt.Errorf("loading model registry: %w", err)
	}

	entry := reg.FindByNameAndQuant(policy.ModelName, policy.Quantization)
	if entry == nil {
		return "", fmt.Errorf("model not found in registry: %s (%s)", policy.ModelName, policy.Quantization)
	}

	// Build cache base path
	if modelsDir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("getting home directory: %w", err)
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
			return "", fmt.Errorf("creating model directory: %w", err)
		}

		if err := downloadWithProgress(ctx, downloadURL, shardPath, expectedSize, logger); err != nil {
			return "", fmt.Errorf("downloading shard %d/%d: %w", i+1, len(shardNames), err)
		}
	}

	// Return the first shard path — llama.cpp auto-discovers the rest
	primaryPath := filepath.Join(baseDir, shardNames[0])
	logger.Info("model ready",
		"path", primaryPath,
		"shards", len(shardNames),
	)
	return primaryPath, nil
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
