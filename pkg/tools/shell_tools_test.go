package tools

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// TestDiscoverShellTools_ValidTool checks that a correctly-paired .sh + .tool.json
// is discovered and returned as a ToolDefinition with the right metadata.
func TestDiscoverShellTools_ValidTool(t *testing.T) {
	tmpDir := t.TempDir()
	toolsDir := filepath.Join(tmpDir, "tools")
	if err := os.Mkdir(toolsDir, 0755); err != nil {
		t.Fatal(err)
	}

	// Write an executable shell script.
	scriptPath := filepath.Join(toolsDir, "echo_input.sh")
	scriptContent := "#!/bin/sh\ncat" // echo stdin to stdout
	if err := os.WriteFile(scriptPath, []byte(scriptContent), 0755); err != nil {
		t.Fatal(err)
	}

	// Write the companion metadata file.
	meta := `{
		"name":        "echo_input",
		"description": "Echoes the JSON input back as output",
		"parameters": {
			"type": "object",
			"properties": {
				"message": { "type": "string", "description": "Message to echo" }
			},
			"required": ["message"]
		}
	}`
	if err := os.WriteFile(filepath.Join(toolsDir, "echo_input.tool.json"), []byte(meta), 0644); err != nil {
		t.Fatal(err)
	}

	// Change cwd so ./tools/ resolves to our temp dir.
	orig, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(tmpDir); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(orig) })

	defs := DiscoverShellTools()
	if len(defs) != 1 {
		t.Fatalf("expected 1 discovered tool, got %d", len(defs))
	}
	if defs[0].Name != "echo_input" {
		t.Errorf("unexpected name: %q", defs[0].Name)
	}
	if defs[0].Description == "" {
		t.Error("expected non-empty description")
	}
	if len(defs[0].Parameters) == 0 {
		t.Error("expected non-empty parameters")
	}

	// Also verify the executor actually runs and produces valid output.
	reg := NewRegistry()
	LoadShellToolsIntoRegistry(reg)

	args, _ := json.Marshal(map[string]string{"message": "hello"})
	result, err := reg.Execute(context.Background(), "echo_input", args)
	if err != nil {
		t.Fatalf("executing echo_input tool: %v", err)
	}
	if !json.Valid(result.Output) {
		t.Errorf("expected valid JSON output, got: %s", result.Output)
	}
}

// TestDiscoverShellTools_MissingMeta checks that an executable *.sh file without a
// companion *.tool.json is silently skipped (not registered as a tool).
func TestDiscoverShellTools_MissingMeta(t *testing.T) {
	tmpDir := t.TempDir()
	toolsDir := filepath.Join(tmpDir, "tools")
	if err := os.Mkdir(toolsDir, 0755); err != nil {
		t.Fatal(err)
	}

	// Write an executable shell script with NO companion .tool.json.
	scriptPath := filepath.Join(toolsDir, "no_meta.sh")
	if err := os.WriteFile(scriptPath, []byte("#!/bin/sh\necho '{}'"), 0755); err != nil {
		t.Fatal(err)
	}

	orig, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(tmpDir); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(orig) })

	defs := DiscoverShellTools()
	if len(defs) != 0 {
		t.Errorf("expected 0 tools (script without .tool.json should be skipped), got %d: %v", len(defs), defs)
	}
}

// TestDiscoverShellTools_NonExecutable checks that a non-executable *.sh file is skipped.
func TestDiscoverShellTools_NonExecutable(t *testing.T) {
	tmpDir := t.TempDir()
	toolsDir := filepath.Join(tmpDir, "tools")
	if err := os.Mkdir(toolsDir, 0755); err != nil {
		t.Fatal(err)
	}

	// Write a script without execute permission (mode 0644).
	scriptPath := filepath.Join(toolsDir, "not_exec.sh")
	if err := os.WriteFile(scriptPath, []byte("#!/bin/sh\necho '{}'"), 0644); err != nil {
		t.Fatal(err)
	}

	meta := `{"name":"not_exec","description":"Non-executable script","parameters":{"type":"object","properties":{}}}`
	if err := os.WriteFile(filepath.Join(toolsDir, "not_exec.tool.json"), []byte(meta), 0644); err != nil {
		t.Fatal(err)
	}

	orig, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(tmpDir); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(orig) })

	defs := DiscoverShellTools()
	if len(defs) != 0 {
		t.Errorf("expected 0 tools (non-executable script should be skipped), got %d", len(defs))
	}
}
