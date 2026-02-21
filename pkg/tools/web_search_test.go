package tools

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestWebSearch_NoAPIKey verifies that a missing BRAVE_API_KEY returns a descriptive
// error message in the result body (not a Go-level error), so the LLM can surface it.
func TestWebSearch_NoAPIKey(t *testing.T) {
	// Use a context with no credentials and ensure env var is unset.
	t.Setenv("BRAVE_API_KEY", "")
	ctx := context.Background() // no WithCredentials

	args, _ := json.Marshal(map[string]any{"query": "golang testing"})
	result, err := WebSearch(ctx, args)
	if err != nil {
		t.Fatalf("expected no Go error, got: %v", err)
	}

	var out webSearchResult
	if err := json.Unmarshal(result.Output, &out); err != nil {
		t.Fatalf("parsing result output: %v", err)
	}
	if out.Error == "" {
		t.Error("expected a non-empty error field in result, got none")
	}
	if out.Results == nil {
		t.Error("expected empty (non-nil) results slice")
	}
	if out.ResultCount != 0 {
		t.Errorf("expected result_count=0, got %d", out.ResultCount)
	}
}

// TestWebSearch_ValidResponse verifies correct parsing of a Brave API response.
// A mock HTTP server stands in for the real Brave endpoint.
func TestWebSearch_ValidResponse(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if token := r.Header.Get("X-Subscription-Token"); token != "test-key" {
			t.Errorf("expected X-Subscription-Token=test-key, got %q", token)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{
			"web": {
				"results": [
					{"title": "Result One", "url": "https://example.com/1", "description": "Snippet one"},
					{"title": "Result Two", "url": "https://example.com/2", "description": "Snippet two"}
				]
			}
		}`))
	}))
	defer server.Close()

	// Redirect the tool to the test server.
	old := braveSearchBaseURL
	braveSearchBaseURL = server.URL
	defer func() { braveSearchBaseURL = old }()

	ctx := WithCredentials(context.Background(), map[string]string{"BRAVE_API_KEY": "test-key"})
	args, _ := json.Marshal(map[string]any{"query": "golang"})

	result, err := WebSearch(ctx, args)
	if err != nil {
		t.Fatalf("unexpected Go error: %v", err)
	}

	var out webSearchResult
	if err := json.Unmarshal(result.Output, &out); err != nil {
		t.Fatalf("parsing result output: %v", err)
	}
	if out.Error != "" {
		t.Errorf("unexpected error field: %s", out.Error)
	}
	if len(out.Results) != 2 {
		t.Fatalf("expected 2 results, got %d", len(out.Results))
	}
	if out.Results[0].Title != "Result One" {
		t.Errorf("unexpected title[0]: %q", out.Results[0].Title)
	}
	if out.Results[1].URL != "https://example.com/2" {
		t.Errorf("unexpected url[1]: %q", out.Results[1].URL)
	}
	if out.Results[0].Snippet != "Snippet one" {
		t.Errorf("unexpected snippet[0]: %q", out.Results[0].Snippet)
	}
	if out.ResultCount != 2 {
		t.Errorf("expected result_count=2, got %d", out.ResultCount)
	}
}

// TestWebSearch_CountAndFreshness verifies that count and freshness parameters
// are forwarded as query params to the Brave Search API.
func TestWebSearch_CountAndFreshness(t *testing.T) {
	var gotCount, gotFreshness, gotQuery string

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.Query().Get("q")
		gotCount = r.URL.Query().Get("count")
		gotFreshness = r.URL.Query().Get("freshness")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"web":{"results":[]}}`))
	}))
	defer server.Close()

	old := braveSearchBaseURL
	braveSearchBaseURL = server.URL
	defer func() { braveSearchBaseURL = old }()

	ctx := WithCredentials(context.Background(), map[string]string{"BRAVE_API_KEY": "test-key"})
	args, _ := json.Marshal(map[string]any{
		"query":     "open source gpu",
		"count":     7,
		"freshness": "week",
	})

	result, err := WebSearch(ctx, args)
	if err != nil {
		t.Fatalf("unexpected Go error: %v", err)
	}

	var out webSearchResult
	if err := json.Unmarshal(result.Output, &out); err != nil {
		t.Fatalf("parsing result output: %v", err)
	}
	if out.Error != "" {
		t.Errorf("unexpected error field: %s", out.Error)
	}
	if gotQuery != "open source gpu" {
		t.Errorf("expected q=open source gpu, got %q", gotQuery)
	}
	if gotCount != "7" {
		t.Errorf("expected count=7, got %q", gotCount)
	}
	if gotFreshness != "week" {
		t.Errorf("expected freshness=week, got %q", gotFreshness)
	}
}

// TestWebSearch_RateLimit verifies that a 429 response returns a graceful error message.
func TestWebSearch_RateLimit(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer server.Close()

	old := braveSearchBaseURL
	braveSearchBaseURL = server.URL
	defer func() { braveSearchBaseURL = old }()

	ctx := WithCredentials(context.Background(), map[string]string{"BRAVE_API_KEY": "test-key"})
	args, _ := json.Marshal(map[string]any{"query": "test"})

	result, err := WebSearch(ctx, args)
	if err != nil {
		t.Fatalf("unexpected Go error: %v", err)
	}

	var out webSearchResult
	if err := json.Unmarshal(result.Output, &out); err != nil {
		t.Fatalf("parsing result output: %v", err)
	}
	if out.Error == "" {
		t.Error("expected error message for 429, got none")
	}
}
