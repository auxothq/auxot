package codingtools

import (
	"testing"
)

func TestSafePath(t *testing.T) {
	workDir := "/home/agent"

	tests := []struct {
		name      string
		path      string
		want      string
		wantErr   bool
	}{
		{
			name: "relative path inside workDir",
			path: "foo/bar.txt",
			want: "/home/agent/foo/bar.txt",
		},
		{
			name: "relative path at root",
			path: "file.txt",
			want: "/home/agent/file.txt",
		},
		{
			name: "absolute path inside workDir",
			path: "/home/agent/tmp/abc123-file.md",
			want: "/home/agent/tmp/abc123-file.md",
		},
		{
			name: "absolute path equal to workDir",
			path: "/home/agent",
			want: "/home/agent",
		},
		{
			name:    "relative traversal outside workDir",
			path:    "../etc/passwd",
			wantErr: true,
		},
		{
			name:    "absolute path outside workDir",
			path:    "/etc/passwd",
			wantErr: true,
		},
		{
			name:    "absolute path that looks like double-prepend",
			path:    "/home/agent/home/agent/secret",
			wantErr: false, // it's still inside workDir
			want:    "/home/agent/home/agent/secret",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := safePath(workDir, tt.path)
			if tt.wantErr {
				if err == nil {
					t.Errorf("expected error, got path %q", got)
				}
				return
			}
			if err != nil {
				t.Errorf("unexpected error: %v", err)
				return
			}
			if got != tt.want {
				t.Errorf("got %q, want %q", got, tt.want)
			}
		})
	}
}
