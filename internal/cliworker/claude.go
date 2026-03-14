// Package cliworker implements the CLI worker execution path for the auxot-worker binary.
// Instead of spawning llama.cpp, a CLI worker spawns a local AI CLI tool (claude, cursor, codex)
// and streams tokens back to the server via the same WebSocket protocol.
package cliworker

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"

	"github.com/auxothq/auxot/pkg/protocol"
)

// RunJob executes a single job using the Claude Code CLI and streams results
// via the provided callbacks. Mirrors the llama.cpp executor interface so that
// main.go can use the same job loop for both backends.
func RunJob(
	ctx context.Context,
	job protocol.JobMessage,
	claudePath string, // path to the claude binary, defaults to "claude"
	model string, // --model flag value, e.g. "claude-sonnet-4-5"; empty = CLI default
	onToken func(string) error,
	onReasoningToken func(string) error,
	onComplete func(fullResponse, reasoningContent string, cacheTokens, inputTokens, outputTokens, reasoningTokens int, durationMS int64, toolCalls []protocol.ToolCall) error,
	onError func(errMsg, details string) error,
) {
	if claudePath == "" {
		claudePath = "claude"
	}

	systemPrompt, prompt := buildPrompt(job.Messages)

	args := []string{
		"--print",
		"--output-format", "stream-json",
		"--verbose",
		"--permission-mode", "bypassPermissions",
	}
	if model != "" {
		args = append(args, "--model", model)
	}
	if systemPrompt != "" {
		args = append(args, "--system-prompt", systemPrompt)
	}

	args = append(args, "-p", prompt)

	cmd := exec.CommandContext(ctx, claudePath, args...)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		_ = onError("failed to create stdout pipe", err.Error())
		return
	}
	if err := cmd.Start(); err != nil {
		_ = onError("failed to start claude", err.Error())
		return
	}

	var fullText strings.Builder
	var fullReasoning strings.Builder
	var inputTokens, outputTokens int

	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 1<<20), 1<<20)

	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			continue
		}
		var event claudeEvent
		if err := json.Unmarshal([]byte(line), &event); err != nil {
			continue
		}
		switch event.Type {
		case "assistant":
			if event.Message == nil {
				continue
			}
			for _, block := range event.Message.Content {
				switch block.Type {
				case "text":
					if block.Text != "" {
						fullText.WriteString(block.Text)
						_ = onToken(block.Text)
					}
				case "thinking":
					if block.Thinking != "" {
						fullReasoning.WriteString(block.Thinking)
						_ = onReasoningToken(block.Thinking)
					}
				}
			}
			if event.Message.Usage != nil {
				inputTokens = event.Message.Usage.InputTokens
				outputTokens = event.Message.Usage.OutputTokens
			}
		case "result":
			if event.Usage != nil {
				if event.Usage.InputTokens > inputTokens {
					inputTokens = event.Usage.InputTokens
				}
				if event.Usage.OutputTokens > outputTokens {
					outputTokens = event.Usage.OutputTokens
				}
			}
		}
	}

	if err := cmd.Wait(); err != nil && fullText.Len() == 0 {
		_ = onError(fmt.Sprintf("claude exited: %v", err), "")
		return
	}

	_ = onComplete(fullText.String(), fullReasoning.String(), 0, inputTokens, outputTokens, 0, 0, nil)
}

// buildPrompt extracts the system message and formats the conversation history
// into a single prompt string for claude's one-shot --print mode.
func buildPrompt(messages []protocol.ChatMessage) (systemPrompt, prompt string) {
	var turns []string

	for _, msg := range messages {
		text := msg.ContentString()
		switch msg.Role {
		case "system":
			systemPrompt = text
		case "user":
			turns = append(turns, "[user]\n"+text)
		case "assistant":
			turns = append(turns, "[assistant]\n"+text)
		}
	}

	if len(turns) == 0 {
		return
	}

	last := turns[len(turns)-1]
	history := turns[:len(turns)-1]

	if len(history) > 0 {
		prompt = "<conversation_history>\n" + strings.Join(history, "\n\n") + "\n</conversation_history>\n\n" + strings.TrimPrefix(last, "[user]\n")
	} else {
		prompt = strings.TrimPrefix(last, "[user]\n")
	}
	return
}

// ── Claude NDJSON types ────────────────────────────────────────────────────────

type claudeEvent struct {
	Type    string         `json:"type"`
	Message *claudeMessage `json:"message,omitempty"`
	Usage   *claudeUsage   `json:"usage,omitempty"`
}

type claudeMessage struct {
	Content []claudeBlock `json:"content"`
	Usage   *claudeUsage  `json:"usage,omitempty"`
}

type claudeBlock struct {
	Type     string `json:"type"`
	Text     string `json:"text,omitempty"`
	Thinking string `json:"thinking,omitempty"`
}

type claudeUsage struct {
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
}
