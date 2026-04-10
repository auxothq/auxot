package agentworker

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/auxothq/auxot/pkg/protocol"
)

const (
	// uploadMaxFileNameLen caps the sanitized filename length.
	uploadMaxFileNameLen = 200

	// uploadTTL is how long a file may sit in the tmp directory before cleanup.
	uploadTTL = 7 * 24 * time.Hour
)

// maxUploadBytes is the worker-side cap for incoming file payloads.
// The server enforces a stricter primary limit; this is a second-line defense.
// It is a var (not const) so tests can lower the limit without huge allocations.
var maxUploadBytes = 100 * 1024 * 1024 // 100 MB

// handleFileUpload writes the uploaded file to cfg.Dir/tmp/ and sends an ack.
// Always called in a dedicated goroutine — never blocks the main receive loop.
func (w *Worker) handleFileUpload(msg protocol.AgentFileUploadMessage) {
	log := w.logger.With("correlation_id", msg.CorrelationID, "file_name", msg.FileName)

	sendAck := func(resolvedPath, errMsg string) {
		if err := w.writeJSON(protocol.AgentFileUploadAckMessage{
			Type:          protocol.TypeFileUploadAck,
			CorrelationID: msg.CorrelationID,
			ResolvedPath:  resolvedPath,
			Error:         errMsg,
		}); err != nil {
			log.Warn("file.upload: failed to send ack", "err", err)
		}
	}

	// Second-line size defense — server is the primary enforcer.
	if len(msg.Data) > maxUploadBytes {
		log.Warn("file.upload: payload exceeds 100 MB limit", "size", len(msg.Data))
		sendAck("", "file exceeds 100 MB limit")
		return
	}

	tmpDir := filepath.Join(w.cfg.Dir, "tmp")

	// Cleanup stale files before writing the new one.
	// Errors during cleanup are logged but never fail the upload.
	w.cleanupOldUploads(tmpDir)

	// Ensure the tmp directory exists.
	if err := os.MkdirAll(tmpDir, 0755); err != nil {
		log.Error("file.upload: failed to create tmp directory", "err", err)
		sendAck("", fmt.Sprintf("failed to create tmp directory: %v", err))
		return
	}

	sanitized := sanitizeUploadFileName(msg.FileName)

	// Generate an 8-byte (16 hex char) random prefix to avoid collisions.
	var randBuf [8]byte
	if _, err := rand.Read(randBuf[:]); err != nil {
		log.Error("file.upload: failed to generate random prefix", "err", err)
		sendAck("", fmt.Sprintf("failed to generate file prefix: %v", err))
		return
	}
	hexPrefix := hex.EncodeToString(randBuf[:])

	finalPath := filepath.Join(tmpDir, hexPrefix+"-"+sanitized)
	tmpPath := finalPath + ".tmp"

	// Write to a .tmp path first, then rename for atomicity.
	// This prevents the agent from reading a partial file if it acts on the
	// path immediately after receiving the ack.
	if err := os.WriteFile(tmpPath, msg.Data, 0644); err != nil {
		log.Error("file.upload: write failed", "err", err)
		sendAck("", fmt.Sprintf("write failed: %v", err))
		return
	}

	if err := os.Rename(tmpPath, finalPath); err != nil {
		log.Error("file.upload: rename failed", "err", err)
		_ = os.Remove(tmpPath)
		sendAck("", fmt.Sprintf("rename failed: %v", err))
		return
	}

	log.Info("file.upload: file saved", "path", finalPath, "size", len(msg.Data))
	sendAck(finalPath, "")
}

// cleanupOldUploads removes files in dir whose mtime is older than uploadTTL.
// Subdirectories are skipped. Per-file errors are logged but do not abort the
// cleanup pass — the calling upload proceeds regardless.
func (w *Worker) cleanupOldUploads(dir string) {
	cutoff := time.Now().Add(-uploadTTL)

	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return // tmp dir not yet created — nothing to clean
		}
		w.logger.Warn("file.upload: cleanup scan failed", "dir", dir, "err", err)
		return
	}

	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			w.logger.Warn("file.upload: cleanup stat failed", "name", entry.Name(), "err", err)
			continue
		}
		if info.ModTime().Before(cutoff) {
			path := filepath.Join(dir, entry.Name())
			if err := os.Remove(path); err != nil {
				w.logger.Warn("file.upload: cleanup delete failed", "path", path, "err", err)
				continue
			}
			w.logger.Info("file.upload: deleted old file", "path", path)
		}
	}
}

// sanitizeUploadFileName strips path separators from name and caps its length.
// Returns "upload" when the result would otherwise be empty.
func sanitizeUploadFileName(name string) string {
	name = strings.ReplaceAll(name, "/", "")
	name = strings.ReplaceAll(name, "\\", "")
	if len(name) > uploadMaxFileNameLen {
		name = name[:uploadMaxFileNameLen]
	}
	if name == "" {
		return "upload"
	}
	return name
}
