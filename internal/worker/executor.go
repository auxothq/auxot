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

// Executor runs chat-completions against an OpenAI-compatible HTTP server
// (llama.cpp or any compatible backend).
type Executor struct {
	llamaURL   string
	logger     *slog.Logger
	jobTimeout time.Duration
	hasVision  bool
	// completionModel is the OpenAI JSON "model" field for /v1/chat/completions.
	// llama.cpp ignores it for a single loaded model — use "" to send "local".
	completionModel string
}

// NewExecutor creates an executor for the given OpenAI base URL. Set hasVision
// when mmproj is loaded so the executor can inform the model about vision.
// completionModel is the JSON "model" field; use "" for llama.cpp (becomes
// "local").
func NewExecutor(llamaURL string, jobTimeout time.Duration, hasVision bool, completionModel string, logger *slog.Logger) *Executor {
	return &Executor{
		llamaURL:        llamaURL,
		logger:          logger,
		jobTimeout:      jobTimeout,
		hasVision:       hasVision,
		completionModel: completionModel,
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

// extractContextSize reads context_size from job.Data (injected by the server
// at dispatch time so the worker knows the model's n_ctx).
func extractContextSize(data map[string]any) int {
	if data == nil {
		return 0
	}
	v, ok := data["context_size"]
	if !ok {
		return 0
	}
	switch n := v.(type) {
	case float64:
		return int(n)
	case int:
		return n
	case int64:
		return int(n)
	}
	return 0
}

// msgContentBytes returns a byte-length approximation for a single message.
func msgContentBytes(m openai.Message) int {
	n := len(m.Content)
	for _, tc := range m.ToolCalls {
		n += len(tc.Function.Name) + len(tc.Function.Arguments)
	}
	return n
}

// lastResortContextTruncation drops the oldest non-system messages until the
// estimated byte payload fits within contextSize. It uses 3 bytes/token as a
// conservative estimate (better than /4 for URL-encoded / JSON-dense content)
// and reserves 20% of the window for tools, system prompt, and output.
// Called on every job so the worker never sends an exceed_context_size error.
func lastResortContextTruncation(messages []openai.Message, contextSize int, logger *slog.Logger, jobID string) []openai.Message {
	if contextSize <= 0 {
		return messages
	}
	// 80% of tokens × 3 bytes/token gives a conservative byte ceiling.
	maxBytes := int(float64(contextSize) * 0.80 * 3)

	total := 0
	for _, m := range messages {
		total += msgContentBytes(m)
	}
	if total <= maxBytes {
		return messages
	}

	logger.Warn("worker: context too large — applying last-resort message truncation",
		"job_id", jobID, "total_bytes", total, "max_bytes", maxBytes, "context_size", contextSize)

	// Locate the first non-system message so we never drop the system prompt.
	first := 0
	for first < len(messages) && messages[first].Role == "system" {
		first++
	}

	// Drop from the oldest end, one message at a time.
	for total > maxBytes && first < len(messages)-1 {
		dropped := messages[first]
		total -= msgContentBytes(dropped)
		messages = append(messages[:first], messages[first+1:]...)
		logger.Warn("worker: dropped message to reduce context",
			"job_id", jobID, "role", dropped.Role, "bytes", msgContentBytes(dropped))
	}

	return messages
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
			args := tc.Function.Arguments
			if args == "" {
				args = "{}"
			}
			msg.ToolCalls = append(msg.ToolCalls, openai.ToolCall{
				ID:   tc.ID,
				Type: tc.Type,
				Function: openai.ToolCallFunction{
					Name:      tc.Function.Name,
					Arguments: args,
				},
			})
		}
		messages[i] = msg
	}

	// Last-resort context truncation: the server compacts aggressively, but
	// URL-encoded content and JSON blobs tokenise 2–4× denser than plain
	// English, so the heuristic estimator can still under-count. If we detect
	// the payload is too large for the model's context window, drop the oldest
	// non-system messages one round at a time until we fit. A degraded response
	// is always better than a hard exceed_context_size error that kills the job.
	messages = lastResortContextTruncation(messages, extractContextSize(job.Data), e.logger, job.JobID)

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

	modelName := strings.TrimSpace(e.completionModel)
	if modelName == "" {
		modelName = "local"
	}
	req := &openai.ChatCompletionRequest{
		Model:       modelName,
		Messages:    messages,
		Tools:       tools,
		Temperature: job.Temperature,
		MaxTokens:   job.MaxTokens,
		Stream:      true,
	}
	req.ReasoningEffort = job.ReasoningEffort
	req.ReturnProgress = true
	if suppressThinking {
		req.ChatTemplateKwargs = map[string]any{
			"enable_thinking": false, // Qwen3, DeepSeek-R1
			"thinking":        false, // Kimi K2.5
		}
	}

	// Debug log the request to llama.cpp (level 2)
	DebugWorkerToLlama(req)

	tStream := time.Now()
	// Long prompts: no SSE chunk (hence no tokens) until prefill — can look "stuck".
	stallWarn := time.AfterFunc(45*time.Second, func() {
		e.logger.Info("inference: still no stream chunk; possible long prompt prefill (large context can take minutes); first token not received yet",
			"job_id", job.JobID, "wait_s", int(time.Since(tStream).Seconds()))
	})
	defer func() { stallWarn.Stop() }()

	client := llamacpp.NewClient(e.llamaURL)

	tokenCh, err := client.StreamCompletion(jobCtx, req)
	if err != nil {
		e.logger.Error("chat stream failed", "job_id", job.JobID, "error", err)
		_ = sendError(fmt.Sprintf("inference error: %v", err), "")
		return
	}

	sawStreamChunk := false
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
	// inXMLToolCallContent mirrors the same suppression for the content stream.
	// Some models emit XML tool calls in content (after </think>) rather than in
	// reasoning. We accumulate the tokens but don't stream them to the user.
	inXMLToolCallContent := false
	// inThinkContent tracks <think>...</think> state in the content stream.
	// llama.cpp sometimes emits the <think> tag itself as a content token
	// (e.g. when the model generates it before the server intercepts it), and
	// may put subsequent reasoning tokens in reasoning_content while leaving
	// the opening/closing tags in content. We detect this and:
	//   • strip <think>/<think> tags from the user-visible stream
	//   • reroute any reasoning text that appears between them in content to
	//     sendReasoningToken instead of sendToken
	inThinkContent := false

	for token := range tokenCh {
		// Any streaming signal from the server (incl. empty + finish / progress) ends "silent stall" anxiety.
		if !sawStreamChunk {
			if token.PromptProgress != nil || token.Content != "" || token.ReasoningContent != "" || len(token.ToolCalls) > 0 || token.FinishReason != "" {
				stallWarn.Stop()
				if token.Content != "" || token.ReasoningContent != "" {
					e.logger.Info("inference: first text chunk from server", "job_id", job.JobID, "ttft_ms", time.Since(tStream).Milliseconds())
				} else {
					e.logger.Info("inference: first server chunk (progress or control)", "job_id", job.JobID, "ms", time.Since(tStream).Milliseconds())
				}
				sawStreamChunk = true
			}
		}

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
			e.logger.Error("stream error from inference server", "job_id", job.JobID, "error", token.Content)
			_ = sendError(fmt.Sprintf("inference: %s", token.Content), "")
			return
		}

		// Relay prompt processing progress to the server (GPU-only; cloud providers don't emit this).
		if token.PromptProgress != nil && sendProgress != nil {
			_ = sendProgress(token.PromptProgress.Total, token.PromptProgress.Cached, token.PromptProgress.Processed)
		}

		if token.Content != "" {
			// Split the content chunk around any <think>...</think> boundaries.
			// llama.cpp normally puts reasoning in reasoning_content, but can
			// also emit the <think>/<think> tags themselves as content tokens.
			// When that happens we reroute the enclosed text to reasoning so it
			// never surfaces as visible response text.
			for _, part := range splitThinkContent(token.Content, &inThinkContent) {
				if part.isThinking {
					if !suppressThinking {
						reasoningContent.WriteString(part.text)
						reasoningTokenCount++
						if !inXMLToolCall && sendReasoningToken != nil {
							if err := sendReasoningToken(part.text); err != nil {
								e.logger.Warn("failed to send reasoning token", "job_id", job.JobID, "error", err)
							}
						}
					}
					continue
				}
				// Regular response content — apply XML tool call suppression.
				// Tool calls may appear in content (after </think>); we still
				// accumulate in fullResponse so parsers can extract them at job end.
				// We check two formats:
				//   <tool_call>...</tool_call>  (Qwen3 XML)
				//   [Calling tool: NAME({...})] (fallback inline format)
				// The pattern may split across SSE chunk boundaries, so we probe
				// the tail of the accumulated response for a partial opening.
				if !inXMLToolCallContent {
					tail := fullResponse.String()
					if len(tail) > 20 {
						tail = tail[len(tail)-20:]
					}
					combined := tail + part.text
					if strings.Contains(combined, "<tool_call>") || strings.Contains(combined, "[Calling tool:") {
						inXMLToolCallContent = true
					}
				}
				fullResponse.WriteString(part.text)
				if !inXMLToolCallContent {
					if err := sendToken(part.text); err != nil {
						e.logger.Warn("failed to send token", "job_id", job.JobID, "error", err)
					}
				}
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

	// If the job context was cancelled (WebSocket disconnect or server cancel),
	// there is no live connection to write to — skip sendComplete/sendError.
	if jobCtx.Err() != nil {
		return
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

	// Qwen3 emits XML tool calls either inside the reasoning block or
	// in the response content after </think>. Check both.
	if len(protoToolCalls) == 0 {
		if strings.Contains(finalReasoning, "<tool_call>") {
			if extracted, stripped := extractXMLToolCalls(finalReasoning); len(extracted) > 0 {
				protoToolCalls = extracted
				finalReasoning = stripped
			}
		}
		if len(protoToolCalls) == 0 && strings.Contains(finalResponse, "<tool_call>") {
			if extracted, stripped := extractXMLToolCalls(finalResponse); len(extracted) > 0 {
				protoToolCalls = extracted
				finalResponse = stripped
			}
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
	logJobCompleted(e.logger, job.JobID, finishReason, cacheTokens, inputTokens, outputTokens, reasoningTokenCount, durationMS, finalResponse, finalReasoning, protoToolCalls, finalTimings)

	if err := sendComplete(finalResponse, finalReasoning, cacheTokens, inputTokens, outputTokens, reasoningTokenCount, durationMS, protoToolCalls); err != nil {
		e.logger.Error("failed to send completion", "job_id", job.JobID, "error", err)
	}
}

// logJobCompleted logs a job completion with progressive detail based on debug level.
// Level 0: Summary stats only (job_id, tokens, duration, finish_reason, reasoning_tokens)
// Level 1: Level 0 + full response text + reasoning content + tool calls with arguments
// Level 2: Level 0 + Level 1 (same as level 1, streaming tokens shown separately via ws_send)
func logJobCompleted(logger *slog.Logger, jobID, finishReason string, cacheTokens, inputTokens, outputTokens, reasoningTokens int, durationMS int64, fullResponse, reasoningContent string, toolCalls []protocol.ToolCall, finalTimings *llamacpp.Timings) {
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

	if finalTimings != nil {
		if finalTimings.TokensPerSecond > 0 {
			attrs = append(attrs, "tokens_per_s", finalTimings.TokensPerSecond)
		} else if finalTimings.PredictedMS > 0 && finalTimings.PredictedTokens > 0 {
			attrs = append(attrs, "tokens_per_s", float64(finalTimings.PredictedTokens)/(finalTimings.PredictedMS/1000.0))
		}
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

// thinkContentPart is one segment produced by splitThinkContent.
type thinkContentPart struct {
	text       string
	isThinking bool // true → route to reasoning; false → route to response
}

// splitThinkContent splits a raw content chunk around <think>...</think>
// boundaries, updating *inThink across SSE chunk calls so partial tags work
// correctly. The <think> and </think> tags themselves are consumed (not
// included in any returned part).
//
// This handles the edge case where llama.cpp emits the <think>/<think> tags
// as content tokens rather than cleanly separating them into reasoning_content.
func splitThinkContent(chunk string, inThink *bool) []thinkContentPart {
	var parts []thinkContentPart
	remainder := chunk
	for len(remainder) > 0 {
		if *inThink {
			if idx := strings.Index(remainder, "</think>"); idx >= 0 {
				if idx > 0 {
					parts = append(parts, thinkContentPart{text: remainder[:idx], isThinking: true})
				}
				*inThink = false
				remainder = remainder[idx+len("</think>"):]
			} else {
				parts = append(parts, thinkContentPart{text: remainder, isThinking: true})
				return parts
			}
		} else {
			if idx := strings.Index(remainder, "<think>"); idx >= 0 {
				if idx > 0 {
					parts = append(parts, thinkContentPart{text: remainder[:idx], isThinking: false})
				}
				*inThink = true
				remainder = remainder[idx+len("<think>"):]
			} else {
				parts = append(parts, thinkContentPart{text: remainder, isThinking: false})
				return parts
			}
		}
	}
	return parts
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
