package router

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestV1Tolerant(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		expected string
	}{
		// OpenAI: /api/openai/v1/... → /api/openai/...
		{"openai chat with v1", "/api/openai/v1/chat/completions", "/api/openai/chat/completions"},
		{"openai models with v1", "/api/openai/v1/models", "/api/openai/models"},
		{"openai chat without v1", "/api/openai/chat/completions", "/api/openai/chat/completions"},

		// Anthropic: /api/anthropic/v1/v1/... → /api/anthropic/v1/...
		{"anthropic messages with doubled v1", "/api/anthropic/v1/v1/messages", "/api/anthropic/v1/messages"},
		{"anthropic messages normal", "/api/anthropic/v1/messages", "/api/anthropic/v1/messages"},

		// Other paths: untouched
		{"health", "/health", "/health"},
		{"ws", "/ws", "/ws"},
		{"root", "/", "/"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var gotPath string
			inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				gotPath = r.URL.Path
				w.WriteHeader(http.StatusOK)
			})

			handler := v1Tolerant(inner)

			req := httptest.NewRequest(http.MethodGet, tt.input, nil)
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)

			if gotPath != tt.expected {
				t.Errorf("v1Tolerant(%q) = %q, want %q", tt.input, gotPath, tt.expected)
			}
		})
	}
}
