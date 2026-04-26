package tools

// McpHttpIntrospect and McpHttpExecute implement a stateless Streamable HTTP
// MCP client for remote MCP servers (e.g. https://api.githubcopilot.com/mcp/).
//
// Each call sequence is: POST initialize → POST notifications/initialized →
// POST tools/list (or tools/call). No persistent session is maintained because
// introspect is a one-shot metadata query and tool calls are short-lived.
// Sessions could be added later for performance.
//
// SSE parsing is copied (minimally) from pkg/tools/browser/session.go to
// avoid introducing an import cycle between tools and tools/browser.

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
)

// mcpProtocolVersion is the MCP protocol version header value sent on all
// Streamable HTTP requests. We pin to the current stable spec revision;
// bump when the spec and vendors align on a newer version.
const mcpProtocolVersion = "2024-11-05"

// sharedMcpHTTPClient is reused across calls for keep-alive efficiency.
// No client-level Timeout is set so SSE streams are not cut short; each
// call uses a context deadline instead.
var sharedMcpHTTPClient = &http.Client{
	Transport: &http.Transport{
		MaxIdleConnsPerHost: 8,
		IdleConnTimeout:     90 * time.Second,
		// Omit ResponseHeaderTimeout intentionally — SSE streams deliver
		// the response header immediately but stream data over time.
	},
}

// mcpHTTPPost marshals req and POSTs it to baseURL, injecting customHeaders
// and MCP protocol headers. It handles both plain JSON and SSE responses,
// returning the JSON-RPC response whose "id" field equals wantID.
//
// Pass wantID = -1 for notifications (no expected response body).
// Returns (nil, nil) for 202 Accepted (notification acknowledgement).
func mcpHTTPPost(ctx context.Context, baseURL, sessionID string, customHeaders map[string]string, wantID int, payload any) (json.RawMessage, string, error) {
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, "", fmt.Errorf("marshal MCP HTTP request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, baseURL, bytes.NewReader(body))
	if err != nil {
		return nil, "", fmt.Errorf("build MCP HTTP request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set("MCP-Protocol-Version", mcpProtocolVersion)
	if sessionID != "" {
		req.Header.Set("Mcp-Session-Id", sessionID)
	}
	for k, v := range customHeaders {
		if k != "" {
			req.Header.Set(k, v)
		}
	}

	resp, err := sharedMcpHTTPClient.Do(req)
	if err != nil {
		return nil, "", fmt.Errorf("POST MCP %s: %w", baseURL, err)
	}
	defer resp.Body.Close()

	// Carry the session ID forward from the response if provided.
	newSessionID := resp.Header.Get("Mcp-Session-Id")
	if newSessionID == "" {
		newSessionID = sessionID
	}

	if resp.StatusCode == http.StatusAccepted {
		// 202: notification acknowledged, no body.
		return nil, newSessionID, nil
	}

	if resp.StatusCode >= 400 {
		b, _ := io.ReadAll(resp.Body)
		return nil, newSessionID, fmt.Errorf("MCP HTTP server returned %d: %s", resp.StatusCode, strings.TrimSpace(string(b)))
	}

	ct := resp.Header.Get("Content-Type")
	if strings.Contains(ct, "text/event-stream") {
		// SSE response — scan until we find the matching id.
		raw, err := mcpReadSSEResponse(resp.Body, wantID)
		return raw, newSessionID, err
	}

	// Plain JSON response.
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, newSessionID, fmt.Errorf("reading MCP HTTP JSON response: %w", err)
	}
	return json.RawMessage(b), newSessionID, nil
}

// mcpReadSSEResponse scans an SSE body and returns the first JSON-RPC message
// whose numeric "id" equals wantID. When wantID == -1, the first non-empty
// data event is returned. Modelled after browser/session.go:readSSEResponse.
func mcpReadSSEResponse(r io.Reader, wantID int) (json.RawMessage, error) {
	br := bufio.NewReaderSize(r, 1<<20) // 1 MiB for large payloads
	var currentData strings.Builder
	for {
		line, err := br.ReadString('\n')
		line = strings.TrimRight(line, "\r\n")

		switch {
		case strings.HasPrefix(line, "data: "):
			currentData.WriteString(strings.TrimPrefix(line, "data: "))
		case line == "":
			if currentData.Len() > 0 {
				data := currentData.String()
				currentData.Reset()

				if wantID == -1 {
					return json.RawMessage(data), nil
				}
				var msg struct {
					ID *json.Number `json:"id"`
				}
				if jsonErr := json.Unmarshal([]byte(data), &msg); jsonErr == nil && msg.ID != nil {
					gotID, parseErr := msg.ID.Int64()
					if parseErr == nil && int(gotID) == wantID {
						return json.RawMessage(data), nil
					}
				}
				// Not our ID; keep reading — server may send notifications first.
			}
		}

		if err != nil {
			if err == io.EOF {
				return nil, fmt.Errorf("SSE stream closed before matching response (wantID=%d)", wantID)
			}
			return nil, fmt.Errorf("reading SSE stream: %w", err)
		}
	}
}

// mcpHTTPHandshake performs the initialize + notifications/initialized handshake
// against baseURL. Returns the session ID (may be empty if the server doesn't
// set Mcp-Session-Id).
func mcpHTTPHandshake(ctx context.Context, baseURL string, headers map[string]string) (string, error) {
	id1 := 1
	raw, sessionID, err := mcpHTTPPost(ctx, baseURL, "", headers, id1, map[string]any{
		"jsonrpc": "2.0",
		"id":      id1,
		"method":  "initialize",
		"params": map[string]any{
			"protocolVersion": mcpProtocolVersion,
			"capabilities":    map[string]any{},
			"clientInfo":      map[string]any{"name": "auxot-tools", "version": "1.0"},
		},
	})
	if err != nil {
		return "", fmt.Errorf("MCP HTTP initialize: %w", err)
	}
	if raw != nil {
		var initResp struct {
			Error *struct {
				Code    int    `json:"code"`
				Message string `json:"message"`
			} `json:"error"`
		}
		if jsonErr := json.Unmarshal(raw, &initResp); jsonErr == nil && initResp.Error != nil {
			return "", fmt.Errorf("MCP HTTP initialize error %d: %s", initResp.Error.Code, initResp.Error.Message)
		}
	}

	// notifications/initialized is fire-and-forget (no id).
	_, sessionID, err = mcpHTTPPost(ctx, baseURL, sessionID, headers, -1, map[string]any{
		"jsonrpc": "2.0",
		"method":  "notifications/initialized",
	})
	if err != nil {
		// Non-fatal — some servers return 202, some return errors; proceed.
		slog.Debug("MCP HTTP notifications/initialized", "error", err)
	}

	return sessionID, nil
}

// McpHttpIntrospect connects to a remote Streamable HTTP MCP server, calls
// tools/list, and returns the tool definitions. It is the HTTP analogue of
// McpIntrospect for stdio servers.
//
// headers contains pre-resolved HTTP header name→value pairs (e.g.
// {"Authorization": "Bearer ghp_xxx"}). The caller is responsible for
// resolving credentials before calling this function.
func McpHttpIntrospect(ctx context.Context, baseURL string, headers map[string]string) ([]McpToolDef, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	sessionID, err := mcpHTTPHandshake(ctx, baseURL, headers)
	if err != nil {
		return nil, err
	}

	id2 := 2
	raw, _, err := mcpHTTPPost(ctx, baseURL, sessionID, headers, id2, map[string]any{
		"jsonrpc": "2.0",
		"id":      id2,
		"method":  "tools/list",
	})
	if err != nil {
		return nil, fmt.Errorf("MCP HTTP tools/list: %w", err)
	}

	// tools/list result: {"result": {"tools": [...]}}
	var envelope struct {
		Result *struct {
			Tools []struct {
				Name        string          `json:"name"`
				Description string          `json:"description"`
				InputSchema json.RawMessage `json:"inputSchema"`
			} `json:"tools"`
		} `json:"result"`
		Error *struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(raw, &envelope); err != nil {
		return nil, fmt.Errorf("parsing MCP HTTP tools/list response: %w", err)
	}
	if envelope.Error != nil {
		return nil, fmt.Errorf("MCP HTTP tools/list error %d: %s", envelope.Error.Code, envelope.Error.Message)
	}
	if envelope.Result == nil {
		return nil, fmt.Errorf("MCP HTTP tools/list: missing result field")
	}

	defs := make([]McpToolDef, 0, len(envelope.Result.Tools))
	for _, t := range envelope.Result.Tools {
		var schema struct {
			Properties map[string]json.RawMessage `json:"properties"`
			Required   []string                   `json:"required"`
		}
		_ = json.Unmarshal(t.InputSchema, &schema)
		seen := make(map[string]bool)
		params := append([]string{}, schema.Required...)
		for _, p := range params {
			seen[p] = true
		}
		for k := range schema.Properties {
			if !seen[k] {
				params = append(params, k)
			}
		}
		defs = append(defs, McpToolDef{
			Name:        t.Name,
			Description: t.Description,
			ParamNames:  params,
			InputSchema: t.InputSchema,
		})
	}
	return defs, nil
}

// McpHttpExecute executes a single MCP tool call against a remote Streamable
// HTTP MCP server. It is the HTTP analogue of McpExecute for stdio servers.
//
// headers contains pre-resolved HTTP header name→value pairs. The server
// must pre-resolve credentials before sending them to the worker.
func McpHttpExecute(ctx context.Context, baseURL string, headers map[string]string, toolName string, args json.RawMessage) (Result, error) {
	args = normalizeMCPArguments(args)

	sessionID, err := mcpHTTPHandshake(ctx, baseURL, headers)
	if err != nil {
		return Result{}, err
	}

	id2 := 2
	raw, _, err := mcpHTTPPost(ctx, baseURL, sessionID, headers, id2, map[string]any{
		"jsonrpc": "2.0",
		"id":      id2,
		"method":  "tools/call",
		"params": map[string]any{
			"name":      toolName,
			"arguments": args,
		},
	})
	if err != nil {
		return Result{}, fmt.Errorf("MCP HTTP tools/call: %w", err)
	}

	// tools/call response: {"result": {...}} or {"error": {...}}
	var envelope struct {
		Result json.RawMessage `json:"result"`
		Error  *struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(raw, &envelope); err != nil {
		return Result{}, fmt.Errorf("parsing MCP HTTP tools/call response: %w", err)
	}
	if envelope.Error != nil {
		return Result{}, mcpJSONRPCToolCallError(toolName, envelope.Error.Code, envelope.Error.Message)
	}
	if envelope.Result == nil {
		return Result{Output: json.RawMessage(`null`)}, nil
	}
	return interpretMcpToolsCallResult(toolName, envelope.Result)
}

func mcpHTTPHostnameSlug(rawURL string) string {
	if strings.TrimSpace(rawURL) == "" {
		return ""
	}
	host := rawURL
	if idx := strings.Index(host, "://"); idx >= 0 {
		host = host[idx+3:]
	}
	if idx := strings.Index(host, "/"); idx >= 0 {
		host = host[:idx]
	}
	if idx := strings.LastIndex(host, ":"); idx >= 0 {
		host = host[:idx]
	}
	host = strings.TrimPrefix(host, "api.")
	parts := strings.Split(host, ".")
	if len(parts) >= 2 {
		host = parts[len(parts)-2]
	}
	return strings.ReplaceAll(host, "-", "_")
}

func mcpHTTPIDLooksLikeUUID(id string) bool {
	id = strings.TrimSpace(id)
	if id == "" {
		return false
	}
	_, err := uuid.Parse(id)
	return err == nil
}

// McpHttpSlug derives a stable LLM-visible aggregate name for an HTTP MCP server.
//
// Rules:
//   - If id is set and is NOT a UUID, treat it as an operator-chosen slug (e.g. "github-copilot").
//   - Otherwise, if the URL yields a hostname slug, use that — so UUID persistence ids do not
//     become the tool prefix (which breaks discover/search for "github").
//   - Fall back to sanitized id, then "remote_mcp".
//
// Examples:
//
//	id="my-server", url=… → "my_server"
//	id=UUID, url="https://api.githubcopilot.com/mcp/" → "githubcopilot"
//	id="", url="https://api.githubcopilot.com/mcp/" → "githubcopilot"
func McpHttpSlug(id, rawURL string) string {
	id = strings.TrimSpace(id)
	urlSlug := mcpHTTPHostnameSlug(rawURL)

	if id != "" && !mcpHTTPIDLooksLikeUUID(id) {
		return strings.ReplaceAll(id, "-", "_")
	}
	if urlSlug != "" {
		return urlSlug
	}
	if id != "" {
		return strings.ReplaceAll(id, "-", "_")
	}
	return "remote_mcp"
}
