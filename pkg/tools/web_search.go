package tools

import (
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"time"
)

// BraveWebSearchDefinition is the LLM-facing schema for the web_search tool.
var BraveWebSearchDefinition = ToolDefinition{
	Name:        "web_search",
	Description: "Search the web for current information using Brave Search. Returns top results with titles, URLs, and snippets. Requires BRAVE_API_KEY to be configured.",
	Parameters: json.RawMessage(`{
		"type": "object",
		"properties": {
			"query": {
				"type": "string",
				"description": "The search query"
			},
			"count": {
				"type": "integer",
				"description": "Number of results to return (1–10). Defaults to 5.",
				"minimum": 1,
				"maximum": 10
			},
			"freshness": {
				"type": "string",
				"description": "Time filter: \"day\" (past 24h), \"week\" (past 7 days), \"month\" (past 30 days). Omit for all time.",
				"enum": ["day", "week", "month"]
			}
		},
		"required": ["query"]
	}`),
}

// braveSearchBaseURL is the Brave Search API endpoint.
// Overridden in tests to point at a mock HTTP server.
var braveSearchBaseURL = "https://api.search.brave.com/res/v1/web/search"

// webSearchArgs is the shape of the web_search tool's JSON arguments.
type webSearchArgs struct {
	Query     string `json:"query"`
	Count     int    `json:"count"`
	Freshness string `json:"freshness"`
}

// webSearchResult is what we return to the LLM.
type webSearchResult struct {
	Query       string           `json:"query"`
	Results     []webSearchItem  `json:"results"`
	ResultCount int              `json:"result_count"`
	Error       string           `json:"error,omitempty"`
}

// webSearchItem is one search result entry.
type webSearchItem struct {
	Title   string `json:"title"`
	URL     string `json:"url"`
	Snippet string `json:"snippet"`
}

// braveAPIResponse is the relevant subset of the Brave Search API response.
type braveAPIResponse struct {
	Web struct {
		Results []struct {
			Title       string `json:"title"`
			URL         string `json:"url"`
			Description string `json:"description"`
		} `json:"results"`
	} `json:"web"`
}

// WebSearch is the Executor for the web_search tool.
// It calls the Brave Search API and returns structured results.
// If BRAVE_API_KEY is not set, it returns a descriptive error message (not a Go error).
func WebSearch(ctx context.Context, args json.RawMessage) (Result, error) {
	var a webSearchArgs
	if err := json.Unmarshal(args, &a); err != nil {
		return Result{}, fmt.Errorf("parsing web_search args: %w", err)
	}
	if a.Query == "" {
		return Result{}, fmt.Errorf("web_search: query is required")
	}

	// Check for API key in context (job credentials) or process env.
	apiKey := Credential(ctx, "BRAVE_API_KEY")
	if apiKey == "" {
		return webSearchError(a.Query, "web_search is not configured. Set BRAVE_API_KEY to enable it.")
	}

	count := a.Count
	if count <= 0 || count > 10 {
		count = 5
	}

	params := url.Values{}
	params.Set("q", a.Query)
	params.Set("count", strconv.Itoa(count))
	if a.Freshness != "" {
		params.Set("freshness", a.Freshness)
	}

	reqURL := braveSearchBaseURL + "?" + params.Encode()

	reqCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, reqURL, nil)
	if err != nil {
		return webSearchError(a.Query, fmt.Sprintf("building request: %s", err.Error()))
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Accept-Encoding", "gzip")
	req.Header.Set("X-Subscription-Token", apiKey)

	// Reuse the package-level client from web_fetch.go (same package, same 60s timeout).
	resp, err := sharedClient.Do(req)
	if err != nil {
		if reqCtx.Err() != nil {
			return webSearchError(a.Query, "web_search request timed out after 15s")
		}
		return webSearchError(a.Query, fmt.Sprintf("request failed: %s", err.Error()))
	}
	defer resp.Body.Close()

	switch resp.StatusCode {
	case http.StatusTooManyRequests:
		return webSearchError(a.Query, "Brave Search API rate limit exceeded (429). Try again later.")
	case http.StatusUnauthorized:
		return webSearchError(a.Query, "Brave Search API key is invalid or expired (401).")
	}
	if resp.StatusCode != http.StatusOK {
		return webSearchError(a.Query, fmt.Sprintf("Brave Search API returned unexpected status %d", resp.StatusCode))
	}

	// Handle gzip-encoded response body (we explicitly requested it).
	var reader io.Reader = resp.Body
	if resp.Header.Get("Content-Encoding") == "gzip" {
		gz, err := gzip.NewReader(resp.Body)
		if err != nil {
			return webSearchError(a.Query, fmt.Sprintf("decompressing gzip response: %s", err.Error()))
		}
		defer gz.Close()
		reader = gz
	}

	body, err := io.ReadAll(io.LimitReader(reader, 1<<20)) // 1 MB cap
	if err != nil {
		return webSearchError(a.Query, fmt.Sprintf("reading response body: %s", err.Error()))
	}

	var braveResp braveAPIResponse
	if err := json.Unmarshal(body, &braveResp); err != nil {
		return webSearchError(a.Query, fmt.Sprintf("parsing Brave API response: %s", err.Error()))
	}

	items := make([]webSearchItem, 0, len(braveResp.Web.Results))
	for _, r := range braveResp.Web.Results {
		items = append(items, webSearchItem{
			Title:   r.Title,
			URL:     r.URL,
			Snippet: r.Description,
		})
	}

	out, err := json.Marshal(webSearchResult{
		Query:       a.Query,
		Results:     items,
		ResultCount: len(items),
	})
	if err != nil {
		return Result{}, fmt.Errorf("marshaling web_search result: %w", err)
	}
	return Result{Output: out}, nil
}

// webSearchError returns a structured error result (not a Go error) so the LLM
// can see what went wrong and respond appropriately.
func webSearchError(query, msg string) (Result, error) {
	out, err := json.Marshal(webSearchResult{
		Query:       query,
		Results:     []webSearchItem{},
		ResultCount: 0,
		Error:       msg,
	})
	if err != nil {
		return Result{}, fmt.Errorf("marshaling web_search error result: %w", err)
	}
	return Result{Output: out}, nil
}
