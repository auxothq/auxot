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
