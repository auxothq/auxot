// Package llamabin downloads and caches the llama.cpp server binary from
// GitHub releases. It detects the correct platform/architecture variant and
// extracts the binary from the archive.
//
// Cache layout:
//
//	~/.auxot/llama-server/{os}-{arch}/llama-{version}/llama-server
package llamabin

import (
	"archive/tar"
	"compress/gzip"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
)

// Version of llama.cpp to download. Pinned for compatibility.
const Version = "b8072"

const githubRepo = "ggml-org/llama.cpp"

// Ensure returns the path to the llama-server binary, downloading it if
// necessary. cacheDir defaults to ~/.auxot/llama-server if empty.
func Ensure(cacheDir string, logger *slog.Logger) (string, error) {
	if cacheDir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("getting home directory: %w", err)
		}
		cacheDir = filepath.Join(home, ".auxot", "llama-server")
	}

	platformDir := filepath.Join(cacheDir, fmt.Sprintf("%s-%s", runtime.GOOS, runtime.GOARCH))
	binaryName := "llama-server"
	if runtime.GOOS == "windows" {
		binaryName = "llama-server.exe"
	}
	binaryPath := filepath.Join(platformDir, fmt.Sprintf("llama-%s", Version), binaryName)

	// Check if binary already exists
	if info, err := os.Stat(binaryPath); err == nil && info.Size() > 0 {
		logger.Info("llama.cpp binary cached", "path", binaryPath)
		return binaryPath, nil
	}

	// Determine the archive to download
	archiveName, err := archiveNameForPlatform()
	if err != nil {
		return "", err
	}

	url := fmt.Sprintf("https://github.com/%s/releases/download/%s/%s", githubRepo, Version, archiveName)
	logger.Info("downloading llama.cpp binary",
		"version", Version,
		"platform", runtime.GOOS+"-"+runtime.GOARCH,
		"url", url,
	)

	if err := os.MkdirAll(platformDir, 0o755); err != nil {
		return "", fmt.Errorf("creating cache directory: %w", err)
	}

	archivePath := filepath.Join(platformDir, archiveName)

	// Download
	if err := downloadFile(url, archivePath, logger); err != nil {
		return "", fmt.Errorf("downloading binary: %w", err)
	}

	// Extract
	logger.Info("extracting archive", "archive", archiveName)
	if err := extractArchive(archivePath, platformDir); err != nil {
		_ = os.Remove(archivePath)
		return "", fmt.Errorf("extracting archive: %w", err)
	}
	_ = os.Remove(archivePath) // Clean up archive

	// Verify binary exists after extraction
	if _, err := os.Stat(binaryPath); err != nil {
		return "", fmt.Errorf("binary not found after extraction: %s", binaryPath)
	}

	// Make executable
	if runtime.GOOS != "windows" {
		if err := os.Chmod(binaryPath, 0o755); err != nil {
			return "", fmt.Errorf("chmod binary: %w", err)
		}
	}

	logger.Info("llama.cpp binary ready", "path", binaryPath)
	return binaryPath, nil
}

// archiveNameForPlatform returns the GitHub release asset name for the
// current OS and architecture.
func archiveNameForPlatform() (string, error) {
	os := runtime.GOOS
	arch := runtime.GOARCH

	switch os {
	case "darwin":
		if arch == "arm64" {
			return fmt.Sprintf("llama-%s-bin-macos-arm64.tar.gz", Version), nil
		}
		return fmt.Sprintf("llama-%s-bin-macos-x64.tar.gz", Version), nil

	case "linux":
		if arch == "arm64" {
			return "", fmt.Errorf("Linux ARM64 binaries not available — build from source")
		}
		// Use Vulkan for GPU acceleration on Linux
		return fmt.Sprintf("llama-%s-bin-ubuntu-vulkan-x64.tar.gz", Version), nil

	case "windows":
		if arch == "arm64" {
			return fmt.Sprintf("llama-%s-bin-win-cpu-arm64.zip", Version), nil
		}
		return fmt.Sprintf("llama-%s-bin-win-cpu-x64.zip", Version), nil

	default:
		return "", fmt.Errorf("unsupported platform: %s-%s", os, arch)
	}
}

// downloadFile downloads a URL to a local file.
func downloadFile(url, dest string, logger *slog.Logger) error {
	resp, err := http.Get(url)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("HTTP %d: %s", resp.StatusCode, resp.Status)
	}

	f, err := os.Create(dest)
	if err != nil {
		return err
	}
	defer f.Close()

	written, err := io.Copy(f, resp.Body)
	if err != nil {
		return err
	}

	logger.Info("downloaded",
		"file", filepath.Base(dest),
		"bytes", written,
	)
	return nil
}

// extractArchive extracts a .tar.gz or .zip archive into destDir.
func extractArchive(archivePath, destDir string) error {
	if strings.HasSuffix(archivePath, ".tar.gz") {
		return extractTarGz(archivePath, destDir)
	}
	if strings.HasSuffix(archivePath, ".zip") {
		return extractZip(archivePath, destDir)
	}
	return fmt.Errorf("unsupported archive format: %s", archivePath)
}

func extractTarGz(archivePath, destDir string) error {
	f, err := os.Open(archivePath)
	if err != nil {
		return err
	}
	defer f.Close()

	gz, err := gzip.NewReader(f)
	if err != nil {
		return err
	}
	defer gz.Close()

	tr := tar.NewReader(gz)
	for {
		header, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}

		target := filepath.Join(destDir, header.Name)

		// Security: prevent path traversal
		if !strings.HasPrefix(filepath.Clean(target), filepath.Clean(destDir)) {
			return fmt.Errorf("archive contains path traversal: %s", header.Name)
		}

		switch header.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, 0o755); err != nil {
				return err
			}
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return err
			}
			outFile, err := os.Create(target)
			if err != nil {
				return err
			}
			if _, err := io.Copy(outFile, tr); err != nil {
				outFile.Close()
				return err
			}
			outFile.Close()

			// Preserve executable permissions
			if header.FileInfo().Mode()&0o111 != 0 {
				if err := os.Chmod(target, 0o755); err != nil {
					return err
				}
			}
		case tar.TypeSymlink:
			// Symlinks are critical — llama.cpp dylibs use them
			// (e.g. libmtmd.0.dylib -> libmtmd.0.0.8072.dylib)
			linkTarget := header.Linkname
			// Security: symlink target must resolve within destDir
			resolved := filepath.Join(filepath.Dir(target), linkTarget)
			if !strings.HasPrefix(filepath.Clean(resolved), filepath.Clean(destDir)) {
				return fmt.Errorf("archive symlink escapes directory: %s -> %s", header.Name, linkTarget)
			}
			// Remove existing file/symlink if present
			_ = os.Remove(target)
			if err := os.Symlink(linkTarget, target); err != nil {
				return fmt.Errorf("creating symlink %s -> %s: %w", header.Name, linkTarget, err)
			}
		}
	}
	return nil
}

func extractZip(archivePath, destDir string) error {
	// Use the system unzip command — simpler and handles all edge cases
	cmd := exec.Command("unzip", "-q", archivePath, "-d", destDir)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}
