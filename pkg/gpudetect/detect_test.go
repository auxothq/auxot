package gpudetect

import (
	"runtime"
	"testing"
)

func TestDetect_ReturnsResult(t *testing.T) {
	result := Detect()

	if result.Backend == "" {
		t.Fatal("Detect() returned empty backend")
	}

	switch runtime.GOOS {
	case "darwin":
		if result.Backend != BackendMetal {
			t.Errorf("expected Metal on macOS, got %s", result.Backend)
		}
		if !result.Detected {
			t.Error("expected Detected=true on macOS")
		}
	default:
		t.Logf("GPU detection: backend=%s detected=%v warning=%q",
			result.Backend, result.Detected, result.Warning)
	}
}

// TestIsAppleSilicon verifies that the function returns a value consistent
// with the compile-time GOOS/GOARCH constants.  This test runs on every
// platform without requiring real Apple hardware.
func TestIsAppleSilicon(t *testing.T) {
	got := IsAppleSilicon()
	want := runtime.GOOS == "darwin" && runtime.GOARCH == "arm64"
	if got != want {
		t.Errorf("IsAppleSilicon() = %v, want %v (GOOS=%s GOARCH=%s)",
			got, want, runtime.GOOS, runtime.GOARCH)
	}
}

// TestDetect_AppleSiliconField verifies that Result.IsAppleSilicon is set
// consistently with the standalone IsAppleSilicon() function.
func TestDetect_AppleSiliconField(t *testing.T) {
	result := Detect()
	if result.IsAppleSilicon != IsAppleSilicon() {
		t.Errorf("Result.IsAppleSilicon=%v disagrees with IsAppleSilicon()=%v",
			result.IsAppleSilicon, IsAppleSilicon())
	}
}

// TestIsAppleSilicon_IntelAndLinuxExcluded documents that Metal-capable
// but non-arm64 platforms (Intel Mac, Linux) must not qualify for MLX.
// The MLX engine requires Apple Neural Engine hardware, not just Metal.
func TestIsAppleSilicon_IntelAndLinuxExcluded(t *testing.T) {
	// When running on a non-arm64 platform, Metal may still be present
	// (e.g. Intel Mac) but IsAppleSilicon must return false.
	if runtime.GOOS != "darwin" || runtime.GOARCH != "arm64" {
		if IsAppleSilicon() {
			t.Errorf("IsAppleSilicon()=true on %s/%s — must be false; MLX requires arm64 darwin",
				runtime.GOOS, runtime.GOARCH)
		}
	}
}

// TestDetect_MacOS_MetalAndSiliconConsistency verifies the darwin-specific
// invariant: Metal is always detected on macOS, IsAppleSilicon reflects arch.
func TestDetect_MacOS_MetalAndSiliconConsistency(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("darwin-only test")
	}
	result := Detect()
	if result.Backend != BackendMetal {
		t.Errorf("expected BackendMetal on darwin, got %s", result.Backend)
	}
	if !result.Detected {
		t.Error("expected Detected=true on darwin")
	}
	// IsAppleSilicon should match runtime arch
	wantSilicon := runtime.GOARCH == "arm64"
	if result.IsAppleSilicon != wantSilicon {
		t.Errorf("IsAppleSilicon=%v, want %v for GOARCH=%s",
			result.IsAppleSilicon, wantSilicon, runtime.GOARCH)
	}
}
