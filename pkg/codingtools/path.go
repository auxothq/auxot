package codingtools

import (
	"fmt"
	"path/filepath"
	"strings"
)

// safePath resolves requestedPath relative to workDir and ensures the result
// is contained within workDir (no path traversal).
func safePath(workDir, requestedPath string) (string, error) {
	abs := filepath.Join(workDir, requestedPath)
	abs = filepath.Clean(abs)
	rel, err := filepath.Rel(workDir, abs)
	if err != nil || strings.HasPrefix(rel, "..") {
		return "", fmt.Errorf("path %q is outside agent directory", requestedPath)
	}
	return abs, nil
}
