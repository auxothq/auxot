package tools

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// shellToolMeta is the parsed content of a {name}.tool.json metadata file.
type shellToolMeta struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Parameters  json.RawMessage `json:"parameters"`
}

// shellToolBundle pairs a ToolDefinition with its Executor for internal use.
type shellToolBundle struct {
	def  ToolDefinition
	exec Executor
}

// DiscoverShellTools scans ./tools/ (relative to process cwd) for shell tool definitions.
//
// For each executable *.sh file with a companion *.tool.json metadata file, a ToolDefinition
// is created. Scripts without a *.tool.json are silently skipped.
//
// Returns an empty (non-nil) slice if ./tools/ does not exist or is empty.
func DiscoverShellTools() []ToolDefinition {
	bundles := discoverShellToolsInDir(filepath.Join(".", "tools"))
	defs := make([]ToolDefinition, 0, len(bundles))
	for _, b := range bundles {
		defs = append(defs, b.def)
	}
	return defs
}

// LoadShellToolsIntoRegistry discovers shell tools from ./tools/ and registers
// their executors in reg. Call this after DefaultRegistry() at startup.
func LoadShellToolsIntoRegistry(reg *Registry) {
	for _, b := range discoverShellToolsInDir(filepath.Join(".", "tools")) {
		reg.Register(b.def.Name, b.exec)
		slog.Info("shell_tools: registered tool", "name", b.def.Name)
	}
}

// discoverShellToolsInDir is the internal implementation, accepting an explicit
// directory path so tests can point it at a temporary directory.
func discoverShellToolsInDir(dir string) []shellToolBundle {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return []shellToolBundle{}
		}
		slog.Warn("shell_tools: cannot read tools directory", "dir", dir, "error", err)
		return []shellToolBundle{}
	}

	var bundles []shellToolBundle

	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		name := entry.Name()
		if !strings.HasSuffix(name, ".sh") {
			continue
		}

		scriptPath := filepath.Join(dir, name)

		// Only include executable scripts.
		info, err := entry.Info()
		if err != nil {
			continue
		}
		if !isExecutable(info) {
			slog.Debug("shell_tools: skipping non-executable script", "script", scriptPath)
			continue
		}

		// Require a companion .tool.json metadata file; skip silently if absent.
		baseName := strings.TrimSuffix(name, ".sh")
		metaPath := filepath.Join(dir, baseName+".tool.json")

		metaData, err := os.ReadFile(metaPath)
		if err != nil {
			// Missing metadata is the expected case for raw scripts — not an error.
			continue
		}

		var meta shellToolMeta
		if err := json.Unmarshal(metaData, &meta); err != nil {
			slog.Warn("shell_tools: invalid .tool.json, skipping", "file", metaPath, "error", err)
			continue
		}
		if meta.Name == "" || meta.Description == "" {
			slog.Warn("shell_tools: .tool.json missing name or description, skipping", "file", metaPath)
			continue
		}

		def := ToolDefinition{
			Name:        meta.Name,
			Description: meta.Description,
			Parameters:  meta.Parameters,
		}

		bundles = append(bundles, shellToolBundle{
			def:  def,
			exec: makeShellExecutor(scriptPath),
		})

		slog.Info("shell_tools: discovered tool", "name", meta.Name, "script", scriptPath)
	}

	return bundles
}

// makeShellExecutor returns an Executor that runs scriptPath as a child process.
//
// Protocol:
//   - The tool's JSON arguments are written to the script's stdin.
//   - The script's stdout must be valid JSON (returned as the tool result).
//     If stdout is not valid JSON, it is returned as a JSON string.
//   - The script's stderr is logged at debug level.
//   - Credentials from the job (via WithCredentials) and AUXOT_TOOL_* env vars
//     are merged into the child process environment via BuildToolEnv.
//   - Execution is limited to 60 seconds.
func makeShellExecutor(scriptPath string) Executor {
	return func(ctx context.Context, args json.RawMessage) (Result, error) {
		execCtx, cancel := context.WithTimeout(ctx, 60*time.Second)
		defer cancel()

		cmd := exec.CommandContext(execCtx, scriptPath)

		// Build child env: process env + AUXOT_TOOL_* stripped + job credentials.
		cmd.Env = BuildToolEnv(CredentialsFromCtx(ctx))

		// Pass tool arguments as JSON on stdin.
		if len(args) > 0 {
			cmd.Stdin = bytes.NewReader(args)
		}

		var stdout, stderr bytes.Buffer
		cmd.Stdout = &stdout
		cmd.Stderr = &stderr

		if err := cmd.Run(); err != nil {
			if stderr.Len() > 0 {
				slog.Debug("shell_tools: script stderr", "script", scriptPath, "stderr", stderr.String())
			}
			if execCtx.Err() != nil {
				return shellToolError(fmt.Sprintf("shell tool %q timed out after 60s", scriptPath))
			}
			return shellToolError(fmt.Sprintf("shell tool %q exited with error: %s", scriptPath, err.Error()))
		}

		if stderr.Len() > 0 {
			slog.Debug("shell_tools: script stderr", "script", scriptPath, "stderr", stderr.String())
		}

		outBytes := stdout.Bytes()

		// Return stdout directly if it's already valid JSON.
		if json.Valid(outBytes) {
			return Result{Output: json.RawMessage(outBytes)}, nil
		}

		// Otherwise wrap the raw output as a JSON string so the LLM can still read it.
		wrapped, err := json.Marshal(string(outBytes))
		if err != nil {
			return Result{}, fmt.Errorf("marshaling shell tool output: %w", err)
		}
		return Result{Output: wrapped}, nil
	}
}

// shellToolError returns a structured error result (not a Go error).
func shellToolError(msg string) (Result, error) {
	out, err := json.Marshal(map[string]string{"error": msg})
	if err != nil {
		return Result{}, fmt.Errorf("marshaling shell tool error: %w", err)
	}
	return Result{Output: out}, nil
}

// isExecutable reports whether a file has at least one executable bit set.
// This is a Unix-only check; on Windows all files are treated as executable.
func isExecutable(info fs.FileInfo) bool {
	return info.Mode()&0111 != 0
}
