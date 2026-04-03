package worker

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/auxothq/auxot/pkg/llamacpp"
	"github.com/auxothq/auxot/pkg/openai"
	"github.com/auxothq/auxot/pkg/protocol"
)

// thinkTagRe matches <think> and </think> tags (but NOT the content between them).
// Used to clean up residual think tags from suppressed-thinking responses without
// removing any actual content the model generated inside them.
var thinkTagRe = regexp.MustCompile(`</?think>`)

// Executor handles running jobs on the local llama.cpp server.
type Executor struct {
	llamaURL   string
	logger     *slog.Logger
	jobTimeout time.Duration
	hasVision  bool
}

// NewExecutor creates an Executor that sends inference requests to the
// llama.cpp server at the given URL. Set hasVision when mmproj is loaded
// so the executor can inform the model about native vision capability.
func NewExecutor(llamaURL string, jobTimeout time.Duration, hasVision bool, logger *slog.Logger) *Executor {
	return &Executor{
		llamaURL:   llamaURL,
		logger:     logger,
		jobTimeout: jobTimeout,
		hasVision:  hasVision,
	}
}

// toolCallKey is a compact key representing one tool call + its result,
// used to detect identical consecutive tool calls in the conversation history.
type toolCallKey struct {
	name      string
	arguments string
	result    string
}

// detectToolLoop scans the conversation history for the last N rounds of
// tool_call→tool_result pairs. Returns true (and a description) if the most
// recent 3 consecutive rounds are all identical: same tool name, same
// arguments, and same result. An agent retrying the same call with the same
// result is stuck and will never make progress.
func detectToolLoop(messages []protocol.ChatMessage) (bool, string) {
	const window = 3

	// Walk backwards collecting (assistant w/ tool_calls, tool result) pairs.
	type round struct{ key toolCallKey }
	var rounds []round

	i := len(messages) - 1
	for i >= 0 && len(rounds) < window {
		// Expect a tool result message (role=tool).
		if messages[i].Role != "tool" {
			break
		}
		resultContent := messages[i].ContentString()
		i--

		// The message before the tool result should be the assistant turn that
		// issued the tool call. It may itself be preceded by more tool results
		// from the same assistant turn (parallel tool calls), but for our purposes
		// we only look at single-call turns to keep the comparison simple.
		if i < 0 || messages[i].Role != "assistant" || len(messages[i].ToolCalls) != 1 {
			break
		}
		tc := messages[i].ToolCalls[0]
		rounds = append(rounds, round{key: toolCallKey{
			name:      tc.Function.Name,
			arguments: tc.Function.Arguments,
			result:    resultContent,
		}})
		i--
	}

	if len(rounds) < window {
		return false, ""
	}

	first := rounds[0].key
	for _, r := range rounds[1:] {
		if r.key != first {
			return false, ""
		}
	}

	return true, fmt.Sprintf(
		"Tool loop detected: %q called %d times with identical input and output. Stopping.",
		first.name, window,
	)
}

// Execute runs a job: converts protocol messages to llama.cpp request,
// streams tokens via sendToken, and sends completion/error via the callbacks.
// sendReasoningToken is called for thinking/reasoning tokens (may be nil if not supported).
func (e *Executor) Execute(
	ctx context.Context,
	job protocol.JobMessage,
	sendToken func(token string) error,
	sendReasoningToken func(token string) error,
	sendToolGenerating func() error,
	sendComplete func(fullResponse, reasoningContent string, cacheTokens, inputTokens, outputTokens, reasoningTokens int, durationMS int64, toolCalls []protocol.ToolCall) error,
	sendError func(errMsg, details string) error,
	sendProgress func(total, cached, processed int) error,
) {
	jobCtx, cancel := context.WithTimeout(ctx, e.jobTimeout)
	defer cancel()

	// Detect infinite tool loops before spending tokens on another inference round.
	if looped, msg := detectToolLoop(job.Messages); looped {
		e.logger.Warn("tool loop detected, aborting job", "job_id", job.JobID, "detail", msg)
		_ = sendError(msg, "")
		return
	}

	// No separate "executing job" log - the "received job" log is sufficient

	// Suppress thinking when reasoning_effort is "none".
	// This is used for title generation and agent jobs that need clean output.
	suppressThinking := strings.EqualFold(job.ReasoningEffort, "none")

	// Convert protocol → openai request (preserve tool call history)
	// Content is RawMessage — pass through as-is (string or array for vision).
	messages := make([]openai.Message, len(job.Messages))
	for i, m := range job.Messages {
		content := m.Content
		if content == nil {
			content = json.RawMessage(`""`)
		}
		content = openai.NormalizeVisionContent(append(json.RawMessage(nil), content...))
		msg := openai.Message{
			Role:       m.Role,
			Content:    content,
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

	// When mmproj is loaded, check whether any message carries image content.
	// If so, append a vision capability note to the system message so the model
	// doesn't infer "no vision" from the tool list alone.
	if e.hasVision && messagesContainImage(messages) {
		appendVisionNote(messages)
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
		Model:           "local",
		Messages:        messages,
		Tools:           tools,
		Temperature:     job.Temperature,
		MaxTokens:       job.MaxTokens,
		Stream:          true,
		ReasoningEffort: job.ReasoningEffort,
		ReturnProgress:  true,
	}

	// When thinking is suppressed, tell llama.cpp to disable thinking at the
	// template level via chat_template_kwargs. This prevents the Jinja template
	// from injecting <think> into the generation prompt, so the model never
	// enters thinking mode.
	//
	// Different model families use different variable names:
	//   - Qwen3: enable_thinking
	//   - Kimi K2.5: thinking
	// Passing both is safe — Jinja silently ignores undefined kwargs.
	if suppressThinking {
		req.ChatTemplateKwargs = map[string]any{
			"enable_thinking": false, // Qwen3, DeepSeek-R1
			"thinking":        false, // Kimi K2.5
		}
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
	var reasoningContent strings.Builder
	var reasoningTokenCount int
	// Merge tool call deltas by streaming index — SSE chunks carry incremental
	// fragments: only the first chunk for a given index has id/type/name,
	// subsequent chunks only append to arguments.
	accToolCalls := make(map[int]*openai.ToolCall)
	toolGeneratingNotified := false // Only send tool_generating once per job
	var finalTimings *llamacpp.Timings
	finishReason := ""
	// inXMLToolCall is set true when a <tool_call> XML block is detected in
	// the reasoning stream (Qwen3/llama.cpp behaviour). Once set, reasoning
	// tokens are accumulated but not forwarded to the frontend so users never
	// see raw XML in the Thought process accordion.
	inXMLToolCall := false

	for token := range tokenCh {
		// Debug log each SSE chunk from llama.cpp (level 2)
		if DebugLevel() >= 2 && (token.Content != "" || token.ReasoningContent != "" || len(token.ToolCalls) > 0 || token.FinishReason != "") {
			if len(token.ToolCalls) > 0 {
				// Show the actual Arguments fragment streaming in from this chunk
				for _, tc := range token.ToolCalls {
					DebugLlamaToWorker(fmt.Sprintf("tool_call_delta idx=%d id=%q name=%q args_fragment=%q",
						tc.Index, tc.ID, tc.Function.Name, tc.Function.Arguments))
				}
			} else {
				DebugLlamaToWorker(fmt.Sprintf("content=%q reasoning=%q finish=%q",
					token.Content, token.ReasoningContent, token.FinishReason))
			}
		}

		if token.FinishReason == "error" {
			e.logger.Error("llama.cpp stream error", "job_id", job.JobID, "error", token.Content)
			_ = sendError(fmt.Sprintf("llama.cpp: %s", token.Content), "")
			return
		}

		// Relay prompt processing progress to the server (GPU-only; cloud providers don't emit this).
		if token.PromptProgress != nil && sendProgress != nil {
			_ = sendProgress(token.PromptProgress.Total, token.PromptProgress.Cached, token.PromptProgress.Processed)
		}

		if token.Content != "" {
			fullResponse.WriteString(token.Content)
			if err := sendToken(token.Content); err != nil {
				e.logger.Warn("failed to send token", "job_id", job.JobID, "error", err)
			}
		}

		// Forward reasoning/thinking tokens separately.
		if token.ReasoningContent != "" {
			if suppressThinking {
				// When thinking is suppressed, DON'T discard reasoning tokens
				// from the full response. The model may generate structural
				// content (like closing braces) inside <think> blocks, and
				// discarding them truncates the output. Instead, fold reasoning
				// tokens back into the content so nothing is lost.
				fullResponse.WriteString(token.ReasoningContent)
			} else {
				// Once a <tool_call> XML block appears in the reasoning stream
				// (Qwen3/llama.cpp behaviour), stop forwarding tokens to the
				// frontend. They still accumulate for extractXMLToolCalls at
				// sendComplete time, but users never see raw XML in Thought process.
				if strings.Contains(token.ReasoningContent, "<tool_call>") {
					inXMLToolCall = true
				}
				reasoningContent.WriteString(token.ReasoningContent)
				reasoningTokenCount++
				if !inXMLToolCall && sendReasoningToken != nil {
					if err := sendReasoningToken(token.ReasoningContent); err != nil {
						e.logger.Warn("failed to send reasoning token", "job_id", job.JobID, "error", err)
					}
				}
			}
		}

		// Merge tool call deltas by index
		for _, tc := range token.ToolCalls {
			// Notify frontend once when the first tool call delta arrives
			if !toolGeneratingNotified && sendToolGenerating != nil {
				toolGeneratingNotified = true
				if err := sendToolGenerating(); err != nil {
					e.logger.Warn("failed to send tool_generating", "job_id", job.JobID, "error", err)
				}
			}
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

	// When thinking is suppressed, strip residual <think>/<think> tags from the
	// response but preserve everything between them. Reasoning tokens were folded
	// into fullResponse above so no content is lost; we just clean up the tags.
	finalResponse := fullResponse.String()
	finalReasoning := reasoningContent.String()
	if suppressThinking {
		finalResponse = strings.TrimSpace(thinkTagRe.ReplaceAllString(finalResponse, ""))
		finalReasoning = "" // Discard reasoning text (already folded into response)
		reasoningTokenCount = 0
	}

	// Qwen3 on llama.cpp sometimes puts tool calls in the reasoning block as XML.
	// Normalize them into structured tool_calls before sending to the server.
	if len(protoToolCalls) == 0 && strings.Contains(finalReasoning, "<tool_call>") {
		if extracted, stripped := extractXMLToolCalls(finalReasoning); len(extracted) > 0 {
			protoToolCalls = extracted
			finalReasoning = stripped
		}
	}

	// At level 1+, log the fully assembled tool call arguments before dispatch.
	// This lets you compare "raw tokens from llama" (tool_call_delta logs above)
	// vs "resolved tool call" (what actually gets executed).
	if DebugLevel() >= 1 && len(protoToolCalls) > 0 {
		for _, tc := range protoToolCalls {
			e.logger.Info("tool call resolved",
				"job_id", job.JobID,
				"tool", tc.Function.Name,
				"arguments", tc.Function.Arguments,
			)
		}
	}

	// Log job completion with progressive detail based on debug level
	logJobCompleted(e.logger, job.JobID, finishReason, cacheTokens, inputTokens, outputTokens, reasoningTokenCount, durationMS, finalResponse, finalReasoning, protoToolCalls)

	if err := sendComplete(finalResponse, finalReasoning, cacheTokens, inputTokens, outputTokens, reasoningTokenCount, durationMS, protoToolCalls); err != nil {
		e.logger.Error("failed to send completion", "job_id", job.JobID, "error", err)
	}
}

// logJobCompleted logs a job completion with progressive detail based on debug level.
// Level 0: Summary stats only (job_id, tokens, duration, finish_reason, reasoning_tokens)
// Level 1: Level 0 + full response text + reasoning content + tool calls with arguments
// Level 2: Level 0 + Level 1 (same as level 1, streaming tokens shown separately via ws_send)
func logJobCompleted(logger *slog.Logger, jobID, finishReason string, cacheTokens, inputTokens, outputTokens, reasoningTokens int, durationMS int64, fullResponse, reasoningContent string, toolCalls []protocol.ToolCall) {
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

	if reasoningTokens > 0 {
		attrs = append(attrs, "reasoning_tokens", reasoningTokens)
	}

	if len(toolCalls) > 0 {
		attrs = append(attrs, "num_tool_calls", len(toolCalls))
	}

	// Level 1+: Add full response, reasoning, and complete tool calls
	if level >= 1 {
		if fullResponse != "" {
			attrs = append(attrs, "response", fullResponse)
		}

		if reasoningContent != "" {
			attrs = append(attrs, "reasoning_content", reasoningContent)
		}

		if len(toolCalls) > 0 {
			// Show full tool calls with arguments at level 1+
			attrs = append(attrs, "tool_calls", toolCalls)
		}
	}

	logger.Info("job completed", attrs...)
}

// messagesContainImage returns true if any message has image_url content parts.
func messagesContainImage(msgs []openai.Message) bool {
	for _, m := range msgs {
		if len(m.Content) > 0 && m.Content[0] == '[' {
			if bytes.Contains(m.Content, []byte(`"image_url"`)) {
				return true
			}
		}
	}
	return false
}

const visionNote = "\n\nYou have native vision — images in user messages are visible to you. Incorporate what you see into your response."

// appendVisionNote appends a vision capability note to the first system
// message. This is a product-level injection (not an agent config change)
// so the model knows mmproj is active and doesn't deny its own perception
// based on the tool list.
func appendVisionNote(msgs []openai.Message) {
	for i := range msgs {
		if msgs[i].Role != "system" {
			continue
		}
		var s string
		if err := json.Unmarshal(msgs[i].Content, &s); err != nil {
			continue
		}
		updated, _ := json.Marshal(s + visionNote)
		msgs[i].Content = updated
		return
	}
}
