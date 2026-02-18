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
		// OpenAI — canonical has NO /v1
		{"openai: no v1 (canonical)", "/api/openai/chat/completions", "/api/openai/chat/completions"},
		{"openai: single v1 stripped", "/api/openai/v1/chat/completions", "/api/openai/chat/completions"},
		{"openai: double v1 stripped", "/api/openai/v1/v1/chat/completions", "/api/openai/chat/completions"},
		{"openai: models no v1", "/api/openai/models", "/api/openai/models"},
		{"openai: models single v1", "/api/openai/v1/models", "/api/openai/models"},
		{"openai: models double v1", "/api/openai/v1/v1/models", "/api/openai/models"},

		// Anthropic — canonical has ONE /v1
		{"anthropic: no v1 (adds v1)", "/api/anthropic/messages", "/api/anthropic/v1/messages"},
		{"anthropic: single v1 (canonical)", "/api/anthropic/v1/messages", "/api/anthropic/v1/messages"},
		{"anthropic: double v1 stripped", "/api/anthropic/v1/v1/messages", "/api/anthropic/v1/messages"},
		{"anthropic: no v1 models", "/api/anthropic/models", "/api/anthropic/v1/models"},
		{"anthropic: single v1 models", "/api/anthropic/v1/models", "/api/anthropic/v1/models"},
		{"anthropic: no v1 count_tokens", "/api/anthropic/messages/count_tokens", "/api/anthropic/v1/messages/count_tokens"},

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
