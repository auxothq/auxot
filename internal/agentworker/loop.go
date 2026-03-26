package agentworker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"

	openai "github.com/sashabaranov/go-openai"

	"github.com/auxothq/auxot/pkg/codingtools"
	"github.com/auxothq/auxot/pkg/protocol"
	"github.com/auxothq/auxot/pkg/routerurl"
)

const defaultMaxTurns = 25

// ToolEvent carries a tool call or result notification from the agentic loop
// to the worker, which forwards it over the WebSocket to the server.
type ToolEvent struct {
	IsResult   bool   // false = tool call, true = tool result
	ID         string // tool call ID
	Name       string // tool function name (call only)
	Arguments  string // JSON arguments (call only)
	Content    string // result content (result only)
	IsError    bool   // true if the result is an error (result only)
}

// AgenticLoop executes an agentic inference loop using Auxot's OpenAI-compatible
// API. It calls the API, executes any tool calls locally (Read/Write/Edit/Bash),
// appends results, and loops until the model stops calling tools or maxTurns is hit.
type AgenticLoop struct {
	client        *openai.Client
	workDir       string
	maxTurns      int
	gitagent      *GitAgent
	logger        *slog.Logger
	externalTools []protocol.ExternalToolDef
	toolClient    *ToolClient
	currentJobID  string
	toolEventCh   chan<- ToolEvent // nil-safe; events are dropped if not set
}

// NewAgenticLoop creates a loop that authenticates to the Auxot server with
// agentKey and calls the OpenAI-compatible API at serverURL + "/api/openai/v1".
func NewAgenticLoop(serverURL, agentKey, workDir string, ga *GitAgent, externalTools []protocol.ExternalToolDef, toolClient *ToolClient, logger *slog.Logger) *AgenticLoop {
	httpURL := routerurl.HTTPBase(serverURL)

	cfg := openai.DefaultConfig(agentKey)
	cfg.BaseURL = httpURL + "/api/openai/v1"
	client := openai.NewClientWithConfig(cfg)

	maxTurns := defaultMaxTurns
	if ga != nil && ga.Config.Runtime != nil && ga.Config.Runtime.MaxTurns > 0 {
		maxTurns = ga.Config.Runtime.MaxTurns
	}

	return &AgenticLoop{
		client:        client,
		workDir:       workDir,
		maxTurns:      maxTurns,
		gitagent:      ga,
		logger:        logger,
		externalTools: externalTools,
		toolClient:    toolClient,
	}
}

// allToolDefs merges local coding tools with any external tools from the server.
func (l *AgenticLoop) allToolDefs() []openai.Tool {
	defs := codingtools.OpenAIToolDefs()
	for _, ext := range l.externalTools {
		defs = append(defs, openai.Tool{
			Type: openai.ToolTypeFunction,
			Function: &openai.FunctionDefinition{
				Name:        ext.Name,
				Description: ext.Description,
				Parameters:  ext.Parameters,
			},
		})
	}
	return defs
}

// hasExternalTool reports whether name is among the registered external tools.
func (l *AgenticLoop) hasExternalTool(name string) bool {
	for _, t := range l.externalTools {
		if t.Name == name {
			return true
		}
	}
	return false
}

// Run executes the agentic loop for a single job.
// Tokens streamed from the model are forwarded to tokenCh in real time.
// Tool call/result events are forwarded to toolEventCh (may be nil).
// The caller closes tokenCh after Run returns.
func (l *AgenticLoop) Run(ctx context.Context, job protocol.AgentJobMessage, tokenCh chan<- string, toolEventCh chan<- ToolEvent) error {
	l.currentJobID = job.JobID
	l.toolEventCh = toolEventCh
	messages := buildMessages(job)

	for turn := 0; turn < l.maxTurns; turn++ {
		l.logger.Debug("agentic loop turn", "turn", turn+1, "messages", len(messages))

		stream, err := l.client.CreateChatCompletionStream(ctx, openai.ChatCompletionRequest{
			Model:    "auto",
			Messages: messages,
			Tools:    l.allToolDefs(),
		})
		if err != nil {
			return fmt.Errorf("create stream (turn %d): %w", turn+1, err)
		}

		resp, err := l.collectStream(ctx, stream, tokenCh)
		stream.Close()
		if err != nil {
			return fmt.Errorf("collect stream (turn %d): %w", turn+1, err)
		}

		l.logger.Debug("stream collected", "turn", turn+1, "tool_calls", len(resp.ToolCalls), "content_len", len(resp.Content))

		// No tool calls → model is done.
		if len(resp.ToolCalls) == 0 {
			return nil
		}

		// Append assistant message with accumulated content and tool calls.
		messages = append(messages, openai.ChatCompletionMessage{
			Role:      openai.ChatMessageRoleAssistant,
			Content:   resp.Content,
			ToolCalls: resp.ToolCalls,
		})

		// Execute each tool call and append results.
		for _, tc := range resp.ToolCalls {
			result := l.executeTool(ctx, tc)
			messages = append(messages, openai.ChatCompletionMessage{
				Role:       openai.ChatMessageRoleTool,
				ToolCallID: tc.ID,
				Content:    result,
			})
		}
	}

	return fmt.Errorf("max turns (%d) exceeded without end_turn", l.maxTurns)
}

// streamResult holds the accumulated output of one streaming API call.
type streamResult struct {
	Content   string
	ToolCalls []openai.ToolCall
}

// pendingToolCall accumulates streamed tool call chunks by index.
type pendingToolCall struct {
	id       string
	typ      string
	name     string
	argsAccum strings.Builder
}

// collectStream reads a streaming response, forwards text tokens to tokenCh,
// and accumulates the full content and tool calls for the loop.
func (l *AgenticLoop) collectStream(ctx context.Context, stream *openai.ChatCompletionStream, tokenCh chan<- string) (*streamResult, error) {
	var contentAccum strings.Builder
	pending := make(map[int]*pendingToolCall) // index → pending tool call

	for {
		chunk, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, err
		}

		for _, choice := range chunk.Choices {
			// Forward text content tokens in real time.
			if choice.Delta.Content != "" {
				contentAccum.WriteString(choice.Delta.Content)
				select {
				case tokenCh <- choice.Delta.Content:
				case <-ctx.Done():
					return nil, ctx.Err()
				}
			}

			// Accumulate tool call fragments.
			for _, tc := range choice.Delta.ToolCalls {
				idx := tc.Index
				if idx == nil {
					continue
				}
				p, ok := pending[*idx]
				if !ok {
					p = &pendingToolCall{}
					pending[*idx] = p
				}
				if tc.ID != "" {
					p.id = tc.ID
				}
				if tc.Type != "" {
					p.typ = string(tc.Type)
				}
				if tc.Function.Name != "" {
					p.name = tc.Function.Name
				}
				p.argsAccum.WriteString(tc.Function.Arguments)
			}
		}
	}

	// Assemble final tool calls in index order.
	toolCalls := make([]openai.ToolCall, 0, len(pending))
	for i := 0; i < len(pending); i++ {
		p, ok := pending[i]
		if !ok {
			break
		}
		tcType := openai.ToolTypeFunction
		if p.typ != "" {
			tcType = openai.ToolType(p.typ)
		}
		toolCalls = append(toolCalls, openai.ToolCall{
			ID:   p.id,
			Type: tcType,
			Function: openai.FunctionCall{
				Name:      p.name,
				Arguments: p.argsAccum.String(),
			},
		})
	}

	return &streamResult{
		Content:   contentAccum.String(),
		ToolCalls: toolCalls,
	}, nil
}

// executeTool dispatches a single tool call and returns the result string.
// Errors are returned as a string so the model can see and handle them.
// Emits ToolEvent notifications so the UI can track tool execution in real time.
func (l *AgenticLoop) executeTool(ctx context.Context, tc openai.ToolCall) string {
	l.logger.Debug("executing tool", "tool", tc.Function.Name, "args", tc.Function.Arguments)

	l.emitToolEvent(ToolEvent{
		ID:        tc.ID,
		Name:      tc.Function.Name,
		Arguments: tc.Function.Arguments,
	})

	var result string
	var isError bool

	// Try local tools first.
	if tool := codingtools.FindTool(tc.Function.Name); tool != nil {
		out, err := tool.Execute(ctx, l.workDir, nil, json.RawMessage(tc.Function.Arguments))
		if err != nil {
			l.logger.Warn("tool execution failed", "tool", tc.Function.Name, "err", err)
			result = fmt.Sprintf("Error: %s", err)
			isError = true
		} else {
			result = out
		}
	} else if l.toolClient != nil && l.hasExternalTool(tc.Function.Name) {
		resp, err := l.toolClient.Execute(ctx, ToolExecRequest{
			ToolName:  tc.Function.Name,
			Arguments: json.RawMessage(tc.Function.Arguments),
			JobID:     l.currentJobID,
		})
		if err != nil {
			l.logger.Warn("external tool execution failed", "tool", tc.Function.Name, "err", err)
			result = fmt.Sprintf("Error executing %s: %s", tc.Function.Name, err)
			isError = true
		} else if resp.Error != "" {
			result = fmt.Sprintf("Tool error: %s", resp.Error)
			isError = true
		} else {
			result = resp.Result
		}
	} else {
		result = fmt.Sprintf("Unknown tool: %s", tc.Function.Name)
		isError = true
	}

	l.emitToolEvent(ToolEvent{
		IsResult: true,
		ID:       tc.ID,
		Content:  result,
		IsError:  isError,
	})

	return result
}

func (l *AgenticLoop) emitToolEvent(ev ToolEvent) {
	if l.toolEventCh == nil {
		return
	}
	l.toolEventCh <- ev
}

// buildMessages converts a job's system prompt and conversation history into
// the OpenAI messages format.
func buildMessages(job protocol.AgentJobMessage) []openai.ChatCompletionMessage {
	var messages []openai.ChatCompletionMessage

	if job.SystemPrompt != "" {
		messages = append(messages, openai.ChatCompletionMessage{
			Role:    openai.ChatMessageRoleSystem,
			Content: job.SystemPrompt,
		})
	}

	for _, m := range job.Messages {
		role := m.Role
		// Normalize role strings to OpenAI format.
		switch role {
		case "human":
			role = openai.ChatMessageRoleUser
		case "ai", "assistant":
			role = openai.ChatMessageRoleAssistant
		}
		messages = append(messages, openai.ChatCompletionMessage{
			Role:    role,
			Content: m.Content,
		})
	}

	return messages
}
