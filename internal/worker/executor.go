package worker

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"time"

	"github.com/auxothq/auxot/pkg/llamacpp"
	"github.com/auxothq/auxot/pkg/openai"
	"github.com/auxothq/auxot/pkg/protocol"
)

// Executor handles running jobs on the local llama.cpp server.
type Executor struct {
	llamaURL   string
	logger     *slog.Logger
	jobTimeout time.Duration
}

// NewExecutor creates an Executor that sends inference requests to the
// llama.cpp server at the given URL.
func NewExecutor(llamaURL string, jobTimeout time.Duration, logger *slog.Logger) *Executor {
	return &Executor{
		llamaURL:   llamaURL,
		logger:     logger,
		jobTimeout: jobTimeout,
	}
}

// Execute runs a job: converts protocol messages to llama.cpp request,
// streams tokens via sendToken, and sends completion/error via the callbacks.
func (e *Executor) Execute(
	ctx context.Context,
	job protocol.JobMessage,
	sendToken func(token string) error,
	sendComplete func(fullResponse string, cacheTokens, inputTokens, outputTokens int, durationMS int64, toolCalls []protocol.ToolCall) error,
	sendError func(errMsg, details string) error,
) {
	jobCtx, cancel := context.WithTimeout(ctx, e.jobTimeout)
	defer cancel()

	// No separate "executing job" log - the "received job" log is sufficient

	// Convert protocol → openai request (preserve tool call history)
	messages := make([]openai.Message, len(job.Messages))
	for i, m := range job.Messages {
		msg := openai.Message{
			Role:       m.Role,
			Content:    m.Content,
			ToolCallID: m.ToolCallID,
		}
		for _, tc := range m.ToolCalls {
			msg.ToolCalls = append(msg.ToolCalls, openai.ToolCall{
				ID:   tc.ID,
				Type: tc.Type,
				Function: openai.ToolCallFunction{
					Name:      tc.Function.Name,
					Arguments: tc.Function.Arguments,
				},
			})
		}
		messages[i] = msg
	}

	// Convert tools — pass through the full definition (name, description, parameters schema).
	// llama.cpp needs the JSON Schema in parameters.properties for its Jinja template.
	var tools []openai.Tool
	for _, t := range job.Tools {
		tools = append(tools, openai.Tool{
			Type: t.Type,
			Function: openai.ToolFunction{
				Name:        t.Function.Name,
				Description: t.Function.Description,
				Parameters:  t.Function.Parameters,
			},
		})
	}

	req := &openai.ChatCompletionRequest{
		Model:       "local",
		Messages:    messages,
		Tools:       tools,
		Temperature: job.Temperature,
		MaxTokens:   job.MaxTokens,
		Stream:      true,
	}

	// Debug log the request to llama.cpp (level 2)
	DebugWorkerToLlama(req)

	client := llamacpp.NewClient(e.llamaURL)
	tokenCh, err := client.StreamCompletion(jobCtx, req)
	if err != nil {
		e.logger.Error("llama.cpp stream failed", "job_id", job.JobID, "error", err)
		_ = sendError(fmt.Sprintf("llama.cpp error: %v", err), "")
		return
	}

	var fullResponse strings.Builder
	// Merge tool call deltas by streaming index — SSE chunks carry incremental
	// fragments: only the first chunk for a given index has id/type/name,
	// subsequent chunks only append to arguments.
	accToolCalls := make(map[int]*openai.ToolCall)
	var finalTimings *llamacpp.Timings
	finishReason := ""

	for token := range tokenCh {
		// Debug log each SSE chunk from llama.cpp (level 2)
		if DebugLevel() >= 2 && (token.Content != "" || len(token.ToolCalls) > 0 || token.FinishReason != "") {
			DebugLlamaToWorker(fmt.Sprintf("content=%q finish=%q tool_calls=%d",
				token.Content, token.FinishReason, len(token.ToolCalls)))
		}

		if token.FinishReason == "error" {
			e.logger.Error("llama.cpp stream error", "job_id", job.JobID, "error", token.Content)
			_ = sendError(fmt.Sprintf("llama.cpp: %s", token.Content), "")
			return
		}

		if token.Content != "" {
			fullResponse.WriteString(token.Content)
			if err := sendToken(token.Content); err != nil {
				e.logger.Warn("failed to send token", "job_id", job.JobID, "error", err)
			}
		}

		// Merge tool call deltas by index
		for _, tc := range token.ToolCalls {
			existing, ok := accToolCalls[tc.Index]
			if !ok {
				copy := tc
				accToolCalls[tc.Index] = &copy
			} else {
				if tc.ID != "" {
					existing.ID = tc.ID
				}
				if tc.Type != "" {
					existing.Type = tc.Type
				}
				if tc.Function.Name != "" {
					existing.Function.Name = tc.Function.Name
				}
				existing.Function.Arguments += tc.Function.Arguments
			}
		}

		if token.FinishReason != "" && token.FinishReason != "error" {
			finishReason = token.FinishReason
		}

		if token.Timings != nil {
			finalTimings = token.Timings
		}
	}

	// Convert merged tool calls to protocol format (sorted by index)
	var protoToolCalls []protocol.ToolCall
	if len(accToolCalls) > 0 {
		indices := make([]int, 0, len(accToolCalls))
		for idx := range accToolCalls {
			indices = append(indices, idx)
		}
		sort.Ints(indices)
		for _, idx := range indices {
			tc := accToolCalls[idx]
			protoToolCalls = append(protoToolCalls, protocol.ToolCall{
				ID:   tc.ID,
				Type: tc.Type,
				Function: protocol.ToolFunction{
					Name:      tc.Function.Name,
					Arguments: tc.Function.Arguments,
				},
			})
		}
	}

	cacheTokens, inputTokens, outputTokens, durationMS := 0, 0, 0, int64(0)
	if finalTimings != nil {
		cacheTokens = finalTimings.CacheTokens
		inputTokens = finalTimings.PromptTokens
		outputTokens = finalTimings.PredictedTokens
		durationMS = int64(finalTimings.PromptMS + finalTimings.PredictedMS)
	}

	// Log job completion with progressive detail based on debug level
	logJobCompleted(e.logger, job.JobID, finishReason, cacheTokens, inputTokens, outputTokens, durationMS, fullResponse.String(), protoToolCalls)

	if err := sendComplete(fullResponse.String(), cacheTokens, inputTokens, outputTokens, durationMS, protoToolCalls); err != nil {
		e.logger.Error("failed to send completion", "job_id", job.JobID, "error", err)
	}
}

// logJobCompleted logs a job completion with progressive detail based on debug level.
// Level 0: Summary stats only (job_id, tokens, duration, finish_reason)
// Level 1: Level 0 + full response text + tool calls with arguments
// Level 2: Level 0 + Level 1 (same as level 1, streaming tokens shown separately via ws_send)
func logJobCompleted(logger *slog.Logger, jobID, finishReason string, cacheTokens, inputTokens, outputTokens int, durationMS int64, fullResponse string, toolCalls []protocol.ToolCall) {
	level := DebugLevel()

	// Level 0: Always log summary stats
	attrs := []any{
		"job_id", jobID,
		"finish_reason", finishReason,
		"cache_tokens", cacheTokens,
		"input_tokens", inputTokens,
		"output_tokens", outputTokens,
		"duration_ms", durationMS,
	}

	if len(toolCalls) > 0 {
		attrs = append(attrs, "num_tool_calls", len(toolCalls))
	}

	// Level 1+: Add full response and complete tool calls
	if level >= 1 {
		if fullResponse != "" {
			attrs = append(attrs, "response", fullResponse)
		}

		if len(toolCalls) > 0 {
			// Show full tool calls with arguments at level 1+
			attrs = append(attrs, "tool_calls", toolCalls)
		}
	}

	logger.Info("job completed", attrs...)
}
