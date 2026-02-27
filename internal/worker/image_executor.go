// Package worker provides ImageExecutor for running image generation jobs
// against stable-diffusion.cpp sd-server.
package worker

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/auxothq/auxot/pkg/protocol"
)

// ImageExecutor runs image generation jobs against sd-server's /v1/images/generations.
type ImageExecutor struct {
	sdURL       string
	logger      *slog.Logger
	jobTimeout  time.Duration
}

// NewImageExecutor creates an ImageExecutor for the given sd-server URL.
func NewImageExecutor(sdURL string, jobTimeout time.Duration, logger *slog.Logger) *ImageExecutor {
	return &ImageExecutor{
		sdURL:      sdURL,
		logger:     logger,
		jobTimeout: jobTimeout,
	}
}

// Execute runs an image generation job: extracts prompt from messages,
// calls sd-server, and returns the generated image as a data URL in fullResponse.
func (e *ImageExecutor) Execute(
	ctx context.Context,
	job protocol.JobMessage,
	sendToken func(token string) error,
	sendReasoningToken func(token string) error,
	sendToolGenerating func() error,
	sendComplete func(fullResponse, reasoningContent string, cacheTokens, inputTokens, outputTokens, reasoningTokens int, durationMS int64, toolCalls []protocol.ToolCall) error,
	sendError func(errMsg, details string) error,
) {
	jobCtx, cancel := context.WithTimeout(ctx, e.jobTimeout)
	defer cancel()

	prompt := extractPromptFromMessages(job.Messages)
	if prompt == "" {
		_ = sendError("No prompt found", "Image generation requires a text prompt in the user message")
		return
	}

	// Default size 512x512 and steps 4 for FLUX (fast, good quality)
	size := "512x512"
	steps := 4
	if job.Data != nil {
		if args, ok := job.Data["arguments"].(map[string]any); ok {
			if s, ok := args["size"].(string); ok && s != "" {
				size = s
			}
			if n, ok := args["steps"].(float64); ok && n > 0 {
				steps = int(n)
			}
		}
	}

	reqBody := map[string]any{
		"prompt": prompt,
		"n":      1,
		"size":   size,
		"steps":  steps,
	}
	body, _ := json.Marshal(reqBody)

	req, err := http.NewRequestWithContext(jobCtx, "POST", e.sdURL+"/v1/images/generations", bytes.NewReader(body))
	if err != nil {
		_ = sendError("Failed to create request", err.Error())
		return
	}
	req.Header.Set("Content-Type", "application/json")

	start := time.Now()
	resp, err := http.DefaultClient.Do(req)
	durationMS := time.Since(start).Milliseconds()
	if err != nil {
		_ = sendError("Image generation request failed", err.Error())
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		var errBody struct {
			Error   string `json:"error"`
			Message string `json:"message"`
		}
		_ = json.NewDecoder(resp.Body).Decode(&errBody)
		msg := errBody.Message
		if msg == "" {
			msg = errBody.Error
		}
		if msg == "" {
			msg = resp.Status
		}
		_ = sendError("Image generation failed", msg)
		return
	}

	var result struct {
		Data []struct {
			B64JSON string `json:"b64_json"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		_ = sendError("Failed to decode response", err.Error())
		return
	}

	if len(result.Data) == 0 || result.Data[0].B64JSON == "" {
		_ = sendError("No image in response", "sd-server returned empty data")
		return
	}

	// Return as data URL so the frontend can display it
	fullResponse := "data:image/png;base64," + result.Data[0].B64JSON

	_ = sendComplete(fullResponse, "", 0, 0, 0, 0, durationMS, nil)
}

func extractPromptFromMessages(messages []protocol.ChatMessage) string {
	for i := len(messages) - 1; i >= 0; i-- {
		m := messages[i]
		if m.Role != "user" {
			continue
		}
		s := m.ContentString()
		s = strings.TrimSpace(s)
		if s != "" {
			return s
		}
	}
	return ""
}
