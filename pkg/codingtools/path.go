package codingtools

import (
	"fmt"
	"path/filepath"
	"strings"
)

// safePath resolves requestedPath relative to workDir and ensures the result
// is contained within workDir (no path traversal).
//
// Absolute paths are accepted as-is (no double-prepending of workDir) but are
// still subject to the containment check — an absolute path outside workDir is
// rejected exactly like a relative traversal attempt.
func safePath(workDir, requestedPath string) (string, error) {
	var abs string
	if filepath.IsAbs(requestedPath) {
		abs = filepath.Clean(requestedPath)
	} else {
		abs = filepath.Clean(filepath.Join(workDir, requestedPath))
	}
	rel, err := filepath.Rel(workDir, abs)
	if err != nil || strings.HasPrefix(rel, "..") {
		return "", fmt.Errorf("path %q is outside agent directory", requestedPath)
	}
	return abs, nil
}
