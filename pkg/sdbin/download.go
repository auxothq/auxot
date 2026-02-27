// Package sdbin downloads and caches the stable-diffusion.cpp sd-server binary
// from GitHub releases. It detects the correct platform/architecture variant
// and extracts the binary from the archive.
//
// Cache layout:
//
//	~/.auxot/sd-server/{os}-{arch}/sd-server-{version}/sd-server
package sdbin

import (
	"archive/zip"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
)

const upstreamRepo = "leejet/stable-diffusion.cpp"
const auxotRepo = "auxothq/auxot" // Our Metal build for macOS

// Ensure returns the path to the sd-server binary, downloading it if necessary.
// cacheDir defaults to ~/.auxot/sd-server if empty.
func Ensure(cacheDir string, logger *slog.Logger) (string, error) {
	if cacheDir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("getting home directory: %w", err)
		}
		cacheDir = filepath.Join(home, ".auxot", "sd-server")
	}

	platformDir := filepath.Join(cacheDir, fmt.Sprintf("%s-%s", runtime.GOOS, runtime.GOARCH))
	binaryName := "sd-server"
	if runtime.GOOS == "windows" {
		binaryName = "sd-server.exe"
	}

	// We use "sd-server" as version dir since we fetch latest; binary lives at platformDir/sd-server-{tag}/sd-server
	// First check if we have any cached version
	entries, err := os.ReadDir(platformDir)
	if err == nil {
		for _, e := range entries {
			if e.IsDir() && strings.HasPrefix(e.Name(), "sd-server-") {
				binaryPath := filepath.Join(platformDir, e.Name(), binaryName)
				if info, err := os.Stat(binaryPath); err == nil && info.Size() > 0 {
					logger.Info("stable-diffusion.cpp binary cached", "path", binaryPath)
					return binaryPath, nil
				}
			}
		}
	}

	// Darwin: prefer auxothq/auxot Metal build; others: leejet/stable-diffusion.cpp
	var release *githubRelease
	var fetchErr error
	if runtime.GOOS == "darwin" && runtime.GOARCH == "arm64" {
		release, fetchErr = fetchAuxotMetalRelease(logger)
		if fetchErr != nil {
			logger.Warn("auxothq/auxot Metal release not available, falling back to upstream", "error", fetchErr)
			release, fetchErr = fetchLatestRelease(upstreamRepo, logger)
		}
	} else {
		release, fetchErr = fetchLatestRelease(upstreamRepo, logger)
	}
	if fetchErr != nil {
		return "", fetchErr
	}

	assetURL, assetName, err := findAssetForPlatform(release)
	if err != nil {
		return "", err
	}

	versionDir := "sd-server-" + release.TagName
	extractDir := filepath.Join(platformDir, versionDir)
	binaryPath := filepath.Join(extractDir, binaryName)

	if info, err := os.Stat(binaryPath); err == nil && info.Size() > 0 {
		logger.Info("stable-diffusion.cpp binary cached", "path", binaryPath)
		return binaryPath, nil
	}

	logger.Info("downloading stable-diffusion.cpp binary",
		"version", release.TagName,
		"platform", runtime.GOOS+"-"+runtime.GOARCH,
		"asset", assetName,
	)

	if err := os.MkdirAll(extractDir, 0o755); err != nil {
		return "", fmt.Errorf("creating cache directory: %w", err)
	}

	archivePath := filepath.Join(platformDir, assetName)
	if err := downloadFile(assetURL, archivePath, logger); err != nil {
		return "", fmt.Errorf("downloading binary: %w", err)
	}

	logger.Info("extracting archive", "archive", assetName)
	if err := extractZip(archivePath, extractDir); err != nil {
		_ = os.Remove(archivePath)
		return "", fmt.Errorf("extracting archive: %w", err)
	}
	_ = os.Remove(archivePath)

	if _, err := os.Stat(binaryPath); err != nil {
		return "", fmt.Errorf("binary not found after extraction: %s", binaryPath)
	}

	if runtime.GOOS != "windows" {
		if err := os.Chmod(binaryPath, 0o755); err != nil {
			return "", fmt.Errorf("chmod binary: %w", err)
		}
	}

	logger.Info("stable-diffusion.cpp binary ready", "path", binaryPath)
	return binaryPath, nil
}

type githubRelease struct {
	TagName string `json:"tag_name"`
	Assets  []struct {
		Name               string `json:"name"`
		BrowserDownloadURL string `json:"browser_download_url"`
	} `json:"assets"`
}

func fetchLatestRelease(repo string, logger *slog.Logger) (*githubRelease, error) {
	url := "https://api.github.com/repos/" + repo + "/releases/latest"
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/vnd.github.v3+json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetching releases: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GitHub API returned %d", resp.StatusCode)
	}

	var release githubRelease
	if err := json.NewDecoder(resp.Body).Decode(&release); err != nil {
		return nil, fmt.Errorf("decoding release: %w", err)
	}
	return &release, nil
}

// fetchAuxotMetalRelease fetches the sd-server-metal release from auxothq/auxot.
// Tag format: sd-server-metal-v1.0.0
func fetchAuxotMetalRelease(logger *slog.Logger) (*githubRelease, error) {
	url := "https://api.github.com/repos/" + auxotRepo + "/releases"
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/vnd.github.v3+json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetching auxothq releases: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GitHub API returned %d", resp.StatusCode)
	}

	var releases []githubRelease
	if err := json.NewDecoder(resp.Body).Decode(&releases); err != nil {
		return nil, fmt.Errorf("decoding releases: %w", err)
	}
	for _, r := range releases {
		if strings.HasPrefix(r.TagName, "sd-server-metal-") {
			return &r, nil
		}
	}
	return nil, fmt.Errorf("no sd-server-metal-* release found in %s", auxotRepo)
}

// findAssetForPlatform returns the download URL and asset name for the current platform.
func findAssetForPlatform(release *githubRelease) (url, name string, err error) {
	osName := runtime.GOOS
	arch := runtime.GOARCH

	var pattern *regexp.Regexp
	switch osName {
	case "darwin":
		// auxothq/auxot Metal build: sd-server-darwin-arm64-metal.zip
		if arch == "arm64" {
			for _, a := range release.Assets {
				if a.Name == "sd-server-darwin-arm64-metal.zip" {
					return a.BrowserDownloadURL, a.Name, nil
				}
			}
		}
		// Fallback: upstream leejet Darwin assets
		if arch == "arm64" {
			pattern = regexp.MustCompile(`^sd-master-[a-f0-9]+-bin-Darwin-macOS-[^-]+-arm64\.zip$`)
		} else {
			pattern = regexp.MustCompile(`^sd-master-[a-f0-9]+-bin-Darwin-macOS-[^-]+-x64\.zip$`)
		}
	case "linux":
		if arch == "arm64" {
			return "", "", fmt.Errorf("Linux ARM64 binaries not available — build from source")
		}
		// Prefer Vulkan for GPU
		for _, a := range release.Assets {
			if strings.Contains(a.Name, "Linux") && strings.Contains(a.Name, "x86_64") && strings.Contains(a.Name, "vulkan") {
				return a.BrowserDownloadURL, a.Name, nil
			}
		}
		// Fallback: ROCm or CPU
		for _, a := range release.Assets {
			if strings.Contains(a.Name, "Linux") && strings.Contains(a.Name, "x86_64") {
				return a.BrowserDownloadURL, a.Name, nil
			}
		}
		return "", "", fmt.Errorf("no Linux x86_64 asset in release %s", release.TagName)
	case "windows":
		// Prefer AVX2, fallback to AVX
		for _, suffix := range []string{"avx2", "avx", "avx512"} {
			pat := regexp.MustCompile(`^sd-master-[a-f0-9]+-bin-win-` + suffix + `-x64\.zip$`)
			for _, a := range release.Assets {
				if pat.MatchString(a.Name) {
					return a.BrowserDownloadURL, a.Name, nil
				}
			}
		}
		pattern = regexp.MustCompile(`^sd-master-[a-f0-9]+-bin-win-avx-x64\.zip$`)
	default:
		return "", "", fmt.Errorf("unsupported platform: %s-%s", osName, arch)
	}

	for _, a := range release.Assets {
		if pattern.MatchString(a.Name) {
			return a.BrowserDownloadURL, a.Name, nil
		}
	}
	return "", "", fmt.Errorf("no matching asset for %s-%s in release %s", osName, arch, release.TagName)
}


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
	logger.Info("downloaded", "file", filepath.Base(dest), "bytes", written)
	return nil
}

func extractZip(archivePath, destDir string) error {
	r, err := zip.OpenReader(archivePath)
	if err != nil {
		return err
	}
	defer r.Close()

	for _, f := range r.File {
		target := filepath.Join(destDir, f.Name)
		if !strings.HasPrefix(filepath.Clean(target), filepath.Clean(destDir)) {
			return fmt.Errorf("archive contains path traversal: %s", f.Name)
		}

		if f.FileInfo().IsDir() {
			if err := os.MkdirAll(target, 0o755); err != nil {
				return err
			}
			continue
		}

		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}

		out, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, f.Mode())
		if err != nil {
			return err
		}

		rc, err := f.Open()
		if err != nil {
			out.Close()
			return err
		}

		_, err = io.Copy(out, rc)
		rc.Close()
		out.Close()
		if err != nil {
			return err
		}

		if f.Mode()&0o111 != 0 && runtime.GOOS != "windows" {
			_ = os.Chmod(target, 0o755)
		}
	}
	return nil
}
