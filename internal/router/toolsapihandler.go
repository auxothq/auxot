package router

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"strings"

	"github.com/auxothq/auxot/pkg/auth"
)

// ToolsAPIHandler handles direct tool execution requests under /api/tools/v1/.
//
// These endpoints allow callers to invoke tools without going through the LLM
// pipeline — useful for testing, scripting, and non-LLM tool workflows.
//
// Routes:
//
//	GET  /api/tools/v1/tools           List tools available from connected workers
//	POST /api/tools/v1/execute         Execute a named tool directly
type ToolsAPIHandler struct {
	verifier *auth.Verifier
	tools    *ToolsWSHandler
	config   *Config
	logger   *slog.Logger
}

// NewToolsAPIHandler creates a ToolsAPIHandler.
func NewToolsAPIHandler(
	verifier *auth.Verifier,
	tools *ToolsWSHandler,
	config *Config,
	logger *slog.Logger,
) *ToolsAPIHandler {
	return &ToolsAPIHandler{
		verifier: verifier,
		tools:    tools,
		config:   config,
		logger:   logger,
	}
}

// ServeHTTP routes incoming requests to the appropriate handler.
func (h *ToolsAPIHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Path
	method := r.Method

	switch {
	case method == http.MethodGet && (path == "/api/tools/v1/tools" || path == "/api/tools/v1/tools/"):
		h.handleListTools(w, r)

	case method == http.MethodPost && (path == "/api/tools/v1/execute" || path == "/api/tools/v1/execute/"):
		h.handleExecuteTool(w, r)

	default:
		writeErrorJSON(w, http.StatusNotFound, "not_found", "endpoint not found")
	}
}

// requireAuth checks the Bearer token. Used for API-key protected tool endpoints.
func (h *ToolsAPIHandler) requireAuth(w http.ResponseWriter, r *http.Request) bool {
	apiKey := extractBearerToken(r)
	if apiKey == "" {
		writeErrorJSON(w, http.StatusUnauthorized, "authentication_error", "missing Bearer token")
		return false
	}
	valid, err := h.verifier.VerifyAPIKey(apiKey)
	if err != nil || !valid {
		writeErrorJSON(w, http.StatusUnauthorized, "authentication_error", "invalid API key")
		return false
	}
	return true
}

// --- GET /api/tools/v1/tools ---

// handleListTools returns a list of tool names and their definitions.
func (h *ToolsAPIHandler) handleListTools(w http.ResponseWriter, r *http.Request) {
	if !h.requireAuth(w, r) {
		return
	}

	tools := h.tools.ConnectedTools()
	defs := h.tools.AllowedToolDefs()

	type toolItem struct {
		Name        string          `json:"name"`
		Description string          `json:"description,omitempty"`
		Parameters  json.RawMessage `json:"parameters,omitempty"`
		Available   bool            `json:"available"` // true = at least one worker is connected
	}

	defsByName := make(map[string]toolItem, len(defs))
	for _, d := range defs {
		defsByName[d.Function.Name] = toolItem{
			Name:        d.Function.Name,
			Description: d.Function.Description,
			Parameters:  d.Function.Parameters,
			Available:   true,
		}
	}

	// Merge in any tools advertised by workers but not in allowed list
	// (they're available but not auto-injected into LLM calls).
	for _, name := range tools {
		if _, ok := defsByName[name]; !ok {
			defsByName[name] = toolItem{Name: name, Available: true}
		}
	}

	result := make([]toolItem, 0, len(defsByName))
	for _, item := range defsByName {
		result = append(result, item)
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{
		"tools":   result,
		"workers": len(tools),
	})
}

// --- POST /api/tools/v1/execute ---

// executeRequest is the body for direct tool execution.
type executeRequest struct {
	Tool      string          `json:"tool"`      // required: tool name
	Arguments json.RawMessage `json:"arguments"` // required: tool arguments as JSON object
}

// handleExecuteTool executes a named tool synchronously and returns the result.
func (h *ToolsAPIHandler) handleExecuteTool(w http.ResponseWriter, r *http.Request) {
	if !h.requireAuth(w, r) {
		return
	}

	var req executeRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErrorJSON(w, http.StatusBadRequest, "invalid_request_error", "invalid JSON: "+err.Error())
		return
	}

	if strings.TrimSpace(req.Tool) == "" {
		writeErrorJSON(w, http.StatusBadRequest, "invalid_request_error", "tool name is required")
		return
	}
	if req.Arguments == nil {
		req.Arguments = json.RawMessage("{}")
	}

	if !h.tools.HasToolsWorker() {
		writeErrorJSON(w, http.StatusServiceUnavailable, "service_unavailable",
			"no tools worker connected — start auxot-tools and connect it to this router")
		return
	}

	h.logger.Info("direct tool execution",
		"tool", req.Tool,
		"remote", r.RemoteAddr,
	)

	ctx := r.Context()
	result, err := h.tools.DispatchDirectToolCall(ctx, req.Tool, req.Arguments)
	if err != nil {
		h.logger.Error("tool execution failed", "tool", req.Tool, "error", err)
		writeErrorJSON(w, http.StatusInternalServerError, "tool_error", err.Error())
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{
		"tool":   req.Tool,
		"result": result,
	})
}
