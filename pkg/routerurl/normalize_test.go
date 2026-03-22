package routerurl

import "testing"

func TestNormalize(t *testing.T) {
	tests := []struct {
		name    string
		raw     string
		wsPath  string
		want    string
		wantErr bool
	}{
		{
			name:   "bare hostname defaults to wss",
			raw:    "auxot.example.com",
			wsPath: "/ws",
			want:   "wss://auxot.example.com/ws",
		},
		{
			name:   "http converts to ws",
			raw:    "http://localhost:8080",
			wsPath: "/ws",
			want:   "ws://localhost:8080/ws",
		},
		{
			name:   "https converts to wss",
			raw:    "https://auxot.example.com",
			wsPath: "/ws",
			want:   "wss://auxot.example.com/ws",
		},
		{
			name:   "ws scheme preserved",
			raw:    "ws://localhost:8080/old-path",
			wsPath: "/ws",
			want:   "ws://localhost:8080/ws",
		},
		{
			name:   "wss scheme preserved",
			raw:    "wss://auxot.example.com/old-path",
			wsPath: "/ws",
			want:   "wss://auxot.example.com/ws",
		},
		{
			name:   "strips path and replaces with canonical",
			raw:    "wss://auxot.example.com/custom/path",
			wsPath: "/ws",
			want:   "wss://auxot.example.com/ws",
		},
		{
			name:   "strips trailing slash",
			raw:    "wss://auxot.example.com/ws/",
			wsPath: "/ws",
			want:   "wss://auxot.example.com/ws",
		},
		{
			name:   "custom wsPath for agent workers",
			raw:    "auxot.example.com",
			wsPath: "/ws/agent",
			want:   "wss://auxot.example.com/ws/agent",
		},
		{
			name:   "hostname with port",
			raw:    "localhost:8080",
			wsPath: "/ws",
			want:   "wss://localhost:8080/ws",
		},
		{
			name:    "empty string errors",
			raw:     "",
			wsPath:  "/ws",
			wantErr: true,
		},
		{
			name:    "whitespace only errors",
			raw:     "   ",
			wsPath:  "/ws",
			wantErr: true,
		},
		{
			name:   "strips query string",
			raw:    "wss://auxot.example.com/ws?debug=1",
			wsPath: "/ws",
			want:   "wss://auxot.example.com/ws",
		},
		{
			name:   "strips fragment",
			raw:    "wss://auxot.example.com/ws#section",
			wsPath: "/ws",
			want:   "wss://auxot.example.com/ws",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := Normalize(tt.raw, tt.wsPath)
			if tt.wantErr {
				if err == nil {
					t.Fatal("expected error, got nil")
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tt.want {
				t.Errorf("Normalize(%q, %q) = %q, want %q", tt.raw, tt.wsPath, got, tt.want)
			}
		})
	}
}

func TestHTTPBase(t *testing.T) {
	tests := []struct {
		name   string
		wsURL  string
		want   string
	}{
		{
			name:  "wss converts to https",
			wsURL: "wss://auxot.example.com/ws",
			want:  "https://auxot.example.com",
		},
		{
			name:  "ws converts to http",
			wsURL: "ws://localhost:8080/ws",
			want:  "http://localhost:8080",
		},
		{
			name:  "strips /ws path",
			wsURL: "wss://auxot.example.com/ws",
			want:  "https://auxot.example.com",
		},
		{
			name:  "strips /ws/agent path",
			wsURL: "wss://auxot.example.com/ws/agent",
			want:  "https://auxot.example.com",
		},
		{
			name:  "strips query string",
			wsURL: "wss://auxot.example.com/ws?test=1",
			want:  "https://auxot.example.com",
		},
		{
			name:  "strips fragment",
			wsURL: "wss://auxot.example.com/ws#anchor",
			want:  "https://auxot.example.com",
		},
		{
			name:  "preserves port",
			wsURL: "ws://localhost:8080/ws",
			want:  "http://localhost:8080",
		},
		{
			name:  "handles malformed URL with fallback",
			wsURL: "wss://example.com:invalid/ws",
			want:  "https://example.com:invalid/ws",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := HTTPBase(tt.wsURL)
			if got != tt.want {
				t.Errorf("HTTPBase(%q) = %q, want %q", tt.wsURL, got, tt.want)
			}
		})
	}
}
