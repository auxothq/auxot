package tools

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"time"
)

// BraveWebAnswersDefinition is the LLM-facing schema for the web_answers tool.
var BraveWebAnswersDefinition = ToolDefinition{
	Name:        "web_answers",
	Description: "Get an AI-generated answer to a question using Brave Search. Returns a direct answer grounded in current web results. Requires BRAVE_ANSWERS_API_KEY to be configured.",
	Parameters: json.RawMessage(`{
		"type": "object",
		"properties": {
			"query": {
				"type": "string",
				"description": "The question or query to answer"
			}
		},
		"required": ["query"]
	}`),
}

// braveAnswersEndpoint is the Brave Answers API (OpenAI-compatible chat completions).
// Overridden in tests to point at a mock HTTP server.
var braveAnswersEndpoint = "https://api.search.brave.com/res/v1/chat/completions"

// webAnswersArgs is the shape of the web_answers tool's JSON arguments.
type webAnswersArgs struct {
	Query string `json:"query"`
}

// webAnswersResult is what we return to the LLM.
type webAnswersResult struct {
	Query  string `json:"query"`
	Answer string `json:"answer"`
	Error  string `json:"error,omitempty"`
}

// braveAnswersChatRequest is the POST body for the Brave Answers API.
type braveAnswersChatRequest struct {
	Messages []braveAnswersChatMessage `json:"messages"`
	Model    string                    `json:"model"`
	Stream   bool                      `json:"stream"`
}

type braveAnswersChatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// braveAnswersChatResponse is the non-streaming Brave Answers API response
// (OpenAI chat completions format).
type braveAnswersChatResponse struct {
	Choices []struct {
		Message struct {
			Content string `json:"content"`
		} `json:"message"`
	} `json:"choices"`
}

// specialTagRe strips Brave's <citation>, <enum_item>, and <usage> tags from
// the answer content so the LLM receives clean plain text.
var specialTagRe = regexp.MustCompile(`<(citation|enum_item|usage)>.*?</(citation|enum_item|usage)>`)

// WebAnswers is the Executor for the web_answers tool.
// It calls the Brave Answers API (OpenAI-compatible chat completions endpoint)
// to get a web-grounded AI answer for the query.
func WebAnswers(ctx context.Context, args json.RawMessage) (Result, error) {
	var a webAnswersArgs
	if err := json.Unmarshal(args, &a); err != nil {
		return Result{}, fmt.Errorf("parsing web_answers args: %w", err)
	}
	if a.Query == "" {
		return Result{}, fmt.Errorf("web_answers: query is required")
	}

	apiKey := Credential(ctx, "BRAVE_ANSWERS_API_KEY")
	if apiKey == "" {
		return webAnswersError(a.Query, "web_answers is not configured. Set BRAVE_ANSWERS_API_KEY to enable it.")
	}

	reqBody := braveAnswersChatRequest{
		Messages: []braveAnswersChatMessage{
			{Role: "user", Content: a.Query},
		},
		Model:  "brave",
		Stream: false,
	}
	reqBytes, err := json.Marshal(reqBody)
	if err != nil {
		return webAnswersError(a.Query, fmt.Sprintf("building request body: %s", err.Error()))
	}

	reqCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(reqCtx, http.MethodPost, braveAnswersEndpoint, bytes.NewReader(reqBytes))
	if err != nil {
		return webAnswersError(a.Query, fmt.Sprintf("building request: %s", err.Error()))
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("X-Subscription-Token", apiKey)

	// Reuse the package-level client from web_fetch.go (same package, 60s timeout).
	resp, err := sharedClient.Do(req)
	if err != nil {
		if reqCtx.Err() != nil {
			return webAnswersError(a.Query, "web_answers request timed out after 30s")
		}
		return webAnswersError(a.Query, fmt.Sprintf("request failed: %s", err.Error()))
	}
	defer resp.Body.Close()

	switch resp.StatusCode {
	case http.StatusTooManyRequests:
		return webAnswersError(a.Query, "Brave Answers API rate limit exceeded (429). Try again later.")
	case http.StatusUnauthorized:
		return webAnswersError(a.Query, "Brave Answers API key is invalid or expired (401). Check BRAVE_ANSWERS_API_KEY.")
	}
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return webAnswersError(a.Query, fmt.Sprintf("Brave Answers API returned status %d: %s", resp.StatusCode, strings.TrimSpace(string(body))))
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20)) // 1 MB cap
	if err != nil {
		return webAnswersError(a.Query, fmt.Sprintf("reading response body: %s", err.Error()))
	}

	var chatResp braveAnswersChatResponse
	if err := json.Unmarshal(body, &chatResp); err != nil {
		return webAnswersError(a.Query, fmt.Sprintf("parsing Brave Answers response: %s", err.Error()))
	}

	if len(chatResp.Choices) == 0 || chatResp.Choices[0].Message.Content == "" {
		return webAnswersError(a.Query, "Brave Answers API returned an empty response.")
	}

	// Strip Brave's special inline tags (<citation>, <enum_item>, <usage>) so the
	// LLM receives clean prose rather than raw XML-like markup.
	answer := specialTagRe.ReplaceAllString(chatResp.Choices[0].Message.Content, "")
	answer = strings.TrimSpace(answer)

	out, err := json.Marshal(webAnswersResult{
		Query:  a.Query,
		Answer: answer,
	})
	if err != nil {
		return Result{}, fmt.Errorf("marshaling web_answers result: %w", err)
	}
	return Result{Output: out}, nil
}

// webAnswersError returns a structured error result (not a Go error) so the LLM
// can see what went wrong and respond appropriately.
func webAnswersError(query, msg string) (Result, error) {
	out, err := json.Marshal(webAnswersResult{
		Query: query,
		Error: msg,
	})
	if err != nil {
		return Result{}, fmt.Errorf("marshaling web_answers error result: %w", err)
	}
	return Result{Output: out}, nil
}
