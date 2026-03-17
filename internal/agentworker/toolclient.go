package agentworker

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"
)

// ToolExecRequest is the payload sent to the server's tool execution endpoint.
type ToolExecRequest struct {
	ToolName  string          `json:"tool_name"`
	Arguments json.RawMessage `json:"arguments"`
	JobID     string          `json:"job_id,omitempty"`
}

// ToolExecResponse is the response from the server's tool execution endpoint.
type ToolExecResponse struct {
	Result string `json:"result"`
	Error  string `json:"error,omitempty"`
}

// ToolClient sends tool execution requests to the Auxot server for tools that
// are not handled locally (external/MCP tools).
type ToolClient struct {
	httpClient *http.Client
	baseURL    string
	agentKey   string
}

// NewToolClient creates a ToolClient that authenticates with agentKey and
// sends requests to serverURL.
func NewToolClient(serverURL, agentKey string) *ToolClient {
	return &ToolClient{
		httpClient: &http.Client{Timeout: 120 * time.Second},
		baseURL:    serverURL,
		agentKey:   agentKey,
	}
}

// Execute sends a tool execution request to the server and returns the result.
func (c *ToolClient) Execute(ctx context.Context, req ToolExecRequest) (*ToolExecResponse, error) {
	body, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("marshal request: %w", err)
	}
	httpReq, err := http.NewRequestWithContext(ctx, "POST", c.baseURL+"/api/tools/v1/execute", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}
	httpReq.Header.Set("Authorization", "Bearer "+c.agentKey)
	httpReq.Header.Set("Content-Type", "application/json")

	resp, err := c.httpClient.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("http request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == 403 {
		return &ToolExecResponse{Error: "tool not allowed by policy"}, nil
	}
	if resp.StatusCode != 200 {
		return &ToolExecResponse{Error: fmt.Sprintf("server error: %d", resp.StatusCode)}, nil
	}

	var result ToolExecResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("decode response: %w", err)
	}
	return &result, nil
}
