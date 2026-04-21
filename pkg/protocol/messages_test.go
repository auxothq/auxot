package protocol

import (
	"encoding/json"
	"testing"
)

func TestParseMessage_WorkerMessages(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		want    any
		wantErr bool
	}{
		{
			name:  "valid hello",
			input: `{"type":"hello","gpu_key":"adm_test123","capabilities":{"backend":"llama.cpp","model":"qwen3","ctx_size":8192}}`,
			want: HelloMessage{
				Type:   TypeHello,
				GPUKey: "adm_test123",
				Capabilities: Capabilities{
					Backend: "llama.cpp",
					Model:   "qwen3",
					CtxSize: 8192,
				},
			},
		},
		{
			name:  "hello with optional fields",
			input: `{"type":"hello","gpu_key":"adm_test","capabilities":{"backend":"llama.cpp","model":"qwen3","ctx_size":8192,"vram_gb":24.0,"total_slots":4}}`,
			want: HelloMessage{
				Type:   TypeHello,
				GPUKey: "adm_test",
				Capabilities: Capabilities{
					Backend:    "llama.cpp",
					Model:      "qwen3",
					CtxSize:    8192,
					VRAMGB:     24.0,
					TotalSlots: 4,
				},
			},
		},
		{
			name:  "heartbeat",
			input: `{"type":"heartbeat"}`,
			want:  HeartbeatMessage{Type: TypeHeartbeat},
		},
		{
			name:  "token",
			input: `{"type":"token","job_id":"job-123","token":"Hello"}`,
			want: TokenMessage{
				Type:  TypeToken,
				JobID: "job-123",
				Token: "Hello",
			},
		},
		{
			name:  "complete without optional fields",
			input: `{"type":"complete","job_id":"job-123","full_response":"Hello world"}`,
			want: CompleteMessage{
				Type:         TypeComplete,
				JobID:        "job-123",
				FullResponse: "Hello world",
			},
		},
		{
			name:  "complete with metrics and tool calls",
			input: `{"type":"complete","job_id":"job-123","full_response":"","duration_ms":1500,"input_tokens":100,"output_tokens":50,"tool_calls":[{"id":"call_1","type":"function","function":{"name":"get_weather","arguments":"{\"city\":\"NYC\"}"}}]}`,
			want: CompleteMessage{
				Type:         TypeComplete,
				JobID:        "job-123",
				FullResponse: "",
				DurationMS:   1500,
				InputTokens:  100,
				OutputTokens: 50,
				ToolCalls: []ToolCall{
					{
						ID:   "call_1",
						Type: "function",
						Function: ToolFunction{
							Name:      "get_weather",
							Arguments: `{"city":"NYC"}`,
						},
					},
				},
			},
		},
		{
			name:  "error with job id",
			input: `{"type":"error","job_id":"job-123","error":"out of memory"}`,
			want: ErrorMessage{
				Type:  TypeError,
				JobID: "job-123",
				Error: "out of memory",
			},
		},
		{
			name:  "error without job id",
			input: `{"type":"error","error":"connection failed"}`,
			want: ErrorMessage{
				Type:  TypeError,
				Error: "connection failed",
			},
		},
		{
			name:  "config",
			input: `{"type":"config","capabilities":{"backend":"llama.cpp","model":"qwen3","ctx_size":8192}}`,
			want: ConfigMessage{
				Type: TypeConfig,
				Capabilities: Capabilities{
					Backend: "llama.cpp",
					Model:   "qwen3",
					CtxSize: 8192,
				},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ParseMessage([]byte(tt.input))
			if tt.wantErr {
				if err == nil {
					t.Fatal("expected error, got nil")
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}

			// Compare via JSON round-trip to handle json.RawMessage comparison
			gotJSON, err := json.Marshal(got)
			if err != nil {
				t.Fatalf("marshaling got: %v", err)
			}
			wantJSON, err := json.Marshal(tt.want)
			if err != nil {
				t.Fatalf("marshaling want: %v", err)
			}

			if string(gotJSON) != string(wantJSON) {
				t.Errorf("mismatch:\n  got:  %s\n  want: %s", gotJSON, wantJSON)
			}
		})
	}
}

func TestParseMessage_ServerMessages(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		want    any
		wantErr bool
	}{
		{
			name:  "hello_ack success",
			input: `{"type":"hello_ack","success":true,"gpu_id":"gpu-001","policy":{"model_name":"qwen3-30b","quantization":"Q4_K_M","context_size":8192,"max_parallelism":4,"capabilities":["chat","code"]}}`,
			want: HelloAckMessage{
				Type:    TypeHelloAck,
				Success: true,
				GPUID:   "gpu-001",
				Policy: &Policy{
					ModelName:      "qwen3-30b",
					Quantization:   "Q4_K_M",
					ContextSize:    8192,
					MaxParallelism: 4,
					Capabilities:   []string{"chat", "code"},
				},
			},
		},
		{
			name:  "hello_ack failure with reconnect",
			input: `{"type":"hello_ack","success":false,"error":"invalid key","reconnectInSeconds":30}`,
			want: HelloAckMessage{
				Type:               TypeHelloAck,
				Success:            false,
				Error:              "invalid key",
				ReconnectInSeconds: 30,
			},
		},
		{
			name:  "heartbeat_ack",
			input: `{"type":"heartbeat_ack"}`,
			want:  HeartbeatAckMessage{Type: TypeHeartbeatAck},
		},
		{
			name:  "config_ack success",
			input: `{"type":"config_ack","success":true}`,
			want: ConfigAckMessage{
				Type:    TypeConfigAck,
				Success: true,
			},
		},
		{
			name:  "job with messages",
			input: `{"type":"job","job_id":"job-456","messages":[{"role":"system","content":"You are helpful."},{"role":"user","content":"Hi"}],"temperature":0.7,"max_tokens":1024}`,
			want: JobMessage{
				Type:  TypeJob,
				JobID: "job-456",
				Messages: []ChatMessage{
					{Role: "system", Content: ChatContentString("You are helpful.")},
					{Role: "user", Content: ChatContentString("Hi")},
				},
				Temperature: ptrFloat64(0.7),
				MaxTokens:   ptrInt(1024),
			},
		},
		{
			name:  "job with tools",
			input: `{"type":"job","job_id":"job-789","messages":[{"role":"user","content":"What is the weather?"}],"tools":[{"type":"function","function":{"name":"get_weather","description":"Get current weather","parameters":{"type":"object","properties":{"city":{"type":"string"}}}}}]}`,
			want: JobMessage{
				Type:  TypeJob,
				JobID: "job-789",
				Messages: []ChatMessage{
					{Role: "user", Content: ChatContentString("What is the weather?")},
				},
				Tools: []Tool{
					{
						Type: "function",
						Function: ToolDefinition{
							Name:        "get_weather",
							Description: "Get current weather",
							Parameters:  json.RawMessage(`{"type":"object","properties":{"city":{"type":"string"}}}`),
						},
					},
				},
			},
		},
		{
			name:  "cancel",
			input: `{"type":"cancel","job_id":"job-123"}`,
			want: CancelMessage{
				Type:  TypeCancel,
				JobID: "job-123",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ParseMessage([]byte(tt.input))
			if tt.wantErr {
				if err == nil {
					t.Fatal("expected error, got nil")
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}

			gotJSON, err := json.Marshal(got)
			if err != nil {
				t.Fatalf("marshaling got: %v", err)
			}
			wantJSON, err := json.Marshal(tt.want)
			if err != nil {
				t.Fatalf("marshaling want: %v", err)
			}

			if string(gotJSON) != string(wantJSON) {
				t.Errorf("mismatch:\n  got:  %s\n  want: %s", gotJSON, wantJSON)
			}
		})
	}
}

func TestParseMessage_Errors(t *testing.T) {
	tests := []struct {
		name  string
		input string
	}{
		{
			name:  "invalid json",
			input: `{not valid json}`,
		},
		{
			name:  "unknown type",
			input: `{"type":"explode"}`,
		},
		{
			name:  "empty type",
			input: `{"type":""}`,
		},
		{
			name:  "missing type",
			input: `{"gpu_key":"test"}`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := ParseMessage([]byte(tt.input))
			if err == nil {
				t.Fatal("expected error, got nil")
			}
		})
	}
}

func TestMarshalMessage_RoundTrip(t *testing.T) {
	// Verify that marshaling and parsing produces the same message.
	original := HelloMessage{
		Type:   TypeHello,
		GPUKey: "adm_roundtrip_test",
		Capabilities: Capabilities{
			Backend:    "llama.cpp",
			Model:      "qwen3-30b-a3b",
			CtxSize:    32768,
			VRAMGB:     24.0,
			TotalSlots: 4,
		},
	}

	data, err := MarshalMessage(original)
	if err != nil {
		t.Fatalf("marshaling: %v", err)
	}

	parsed, err := ParseMessage(data)
	if err != nil {
		t.Fatalf("parsing: %v", err)
	}

	got, ok := parsed.(HelloMessage)
	if !ok {
		t.Fatalf("expected HelloMessage, got %T", parsed)
	}

	if got.GPUKey != original.GPUKey {
		t.Errorf("GPUKey: got %q, want %q", got.GPUKey, original.GPUKey)
	}
	if got.Capabilities.CtxSize != original.Capabilities.CtxSize {
		t.Errorf("CtxSize: got %d, want %d", got.Capabilities.CtxSize, original.Capabilities.CtxSize)
	}
	if got.Capabilities.VRAMGB != original.Capabilities.VRAMGB {
		t.Errorf("VRAMGB: got %f, want %f", got.Capabilities.VRAMGB, original.Capabilities.VRAMGB)
	}
	if got.Capabilities.TotalSlots != original.Capabilities.TotalSlots {
		t.Errorf("TotalSlots: got %d, want %d", got.Capabilities.TotalSlots, original.Capabilities.TotalSlots)
	}
}

func TestMarshalMessage_JobRoundTrip(t *testing.T) {
	temp := 0.7
	maxTok := 1024
	original := JobMessage{
		Type:  TypeJob,
		JobID: "job-roundtrip",
		Messages: []ChatMessage{
			{Role: "system", Content: ChatContentString("Be helpful.")},
			{Role: "user", Content: ChatContentString("Hello")},
		},
		Temperature: &temp,
		MaxTokens:   &maxTok,
	}

	data, err := MarshalMessage(original)
	if err != nil {
		t.Fatalf("marshaling: %v", err)
	}

	parsed, err := ParseMessage(data)
	if err != nil {
		t.Fatalf("parsing: %v", err)
	}

	got, ok := parsed.(JobMessage)
	if !ok {
		t.Fatalf("expected JobMessage, got %T", parsed)
	}

	if got.JobID != original.JobID {
		t.Errorf("JobID: got %q, want %q", got.JobID, original.JobID)
	}
	if len(got.Messages) != len(original.Messages) {
		t.Fatalf("Messages count: got %d, want %d", len(got.Messages), len(original.Messages))
	}
	if got.Messages[1].ContentString() != "Hello" {
		t.Errorf("Messages[1].Content: got %q, want %q", got.Messages[1].ContentString(), "Hello")
	}
	if got.Temperature == nil || *got.Temperature != 0.7 {
		t.Errorf("Temperature: got %v, want 0.7", got.Temperature)
	}
	if got.MaxTokens == nil || *got.MaxTokens != 1024 {
		t.Errorf("MaxTokens: got %v, want 1024", got.MaxTokens)
	}
}

// TestJobMessage_ReferenceID verifies that a JSON payload containing
// "reference_id" deserializes into JobMessage.ReferenceID correctly, and that
// a message without the field leaves it as the zero value.
func TestJobMessage_ReferenceID(t *testing.T) {
	tests := []struct {
		name        string
		input       string
		wantRefID   string
	}{
		{
			name:      "reference_id present",
			input:     `{"type":"job","job_id":"j1","messages":[],"reference_id":"thread-abc"}`,
			wantRefID: "thread-abc",
		},
		{
			name:      "reference_id absent (omitempty)",
			input:     `{"type":"job","job_id":"j2","messages":[]}`,
			wantRefID: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ParseMessage([]byte(tt.input))
			if err != nil {
				t.Fatalf("ParseMessage error: %v", err)
			}
			msg, ok := got.(JobMessage)
			if !ok {
				t.Fatalf("expected JobMessage, got %T", got)
			}
			if msg.ReferenceID != tt.wantRefID {
				t.Errorf("ReferenceID = %q, want %q", msg.ReferenceID, tt.wantRefID)
			}
		})
	}
}

// TestToolJobMessage_ThreadID verifies that a JSON payload containing
// "thread_id" deserializes into ToolJobMessage.ThreadID correctly, and that
// a message without the field leaves it as the zero value.
func TestToolJobMessage_ThreadID(t *testing.T) {
	tests := []struct {
		name         string
		input        string
		wantThreadID string
	}{
		{
			name:         "thread_id present",
			input:        `{"type":"tool_job","job_id":"tj1","parent_job_id":"p1","tool_name":"web_fetch","tool_call_id":"c1","arguments":"{}","thread_id":"thread-xyz"}`,
			wantThreadID: "thread-xyz",
		},
		{
			name:         "thread_id absent (omitempty)",
			input:        `{"type":"tool_job","job_id":"tj2","parent_job_id":"p2","tool_name":"web_fetch","tool_call_id":"c2","arguments":"{}"}`,
			wantThreadID: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ParseMessage([]byte(tt.input))
			if err != nil {
				t.Fatalf("ParseMessage error: %v", err)
			}
			msg, ok := got.(ToolJobMessage)
			if !ok {
				t.Fatalf("expected ToolJobMessage, got %T", got)
			}
			if msg.ThreadID != tt.wantThreadID {
				t.Errorf("ThreadID = %q, want %q", msg.ThreadID, tt.wantThreadID)
			}
		})
	}
}

// helpers for pointer types in test data
func ptrFloat64(v float64) *float64 { return &v }
func ptrInt(v int) *int             { return &v }

// TestParseMessage_ToolsWorkerMessages tests round-trip for tools-worker message types.
func TestParseMessage_ToolsWorkerMessages(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  any
	}{
		{
			name:  "tools worker hello",
			input: `{"type":"hello","gpu_key":"wrk_test123","worker_type":"tools","tools_capabilities":{"tools":["web_search","shell_exec"]}}`,
			want: HelloMessage{
				Type:       TypeHello,
				GPUKey:     "wrk_test123",
				WorkerType: "tools",
				ToolsCapabilities: &ToolsCapabilities{
					Tools: []string{"web_search", "shell_exec"},
				},
			},
		},
		{
			name:  "tool_job message",
			input: `{"type":"tool_job","job_id":"tool-123","parent_job_id":"parent-123","tool_name":"web_search","tool_call_id":"call_1","arguments":{"query":"auxot"}}`,
			want: ToolJobMessage{
				Type:        "tool_job",
				JobID:       "tool-123",
				ParentJobID: "parent-123",
				ToolName:    "web_search",
				ToolCallID:  "call_1",
				Arguments:   json.RawMessage(`{"query":"auxot"}`),
			},
		},
		{
			name:  "tool_result success",
			input: `{"type":"tool_result","job_id":"tool-123","parent_job_id":"parent-123","tool_call_id":"call_1","tool_name":"web_search","result":{"status":"ok"}}`,
			want: ToolResultMessage{
				Type:        "tool_result",
				JobID:       "tool-123",
				ParentJobID: "parent-123",
				ToolCallID:  "call_1",
				ToolName:    "web_search",
				Result:      json.RawMessage(`{"status":"ok"}`),
			},
		},
		{
			name:  "tool_result error",
			input: `{"type":"tool_result","job_id":"tool-124","parent_job_id":"parent-124","tool_call_id":"call_2","tool_name":"web_fetch","error":"tool failed"}`,
			want: ToolResultMessage{
				Type:        "tool_result",
				JobID:       "tool-124",
				ParentJobID: "parent-124",
				ToolCallID:  "call_2",
				ToolName:    "web_fetch",
				Error:       "tool failed",
			},
		},
		{
			name:  "policy_reloaded with mcp schemas",
			input: `{"type":"policy_reloaded","advertised_tools":["web_fetch","github"],"mcp_schemas":[{"tool_name":"github","package":"@modelcontextprotocol/server-github","version":"1.0.0","description":"GitHub ops","commands":["create_issue"]}]}`,
			want: PolicyReloadedMessage{
				Type:            "policy_reloaded",
				AdvertisedTools: []string{"web_fetch", "github"},
				McpSchemas: []McpAggregateSchema{
					{
						ToolName:    "github",
						Package:     "@modelcontextprotocol/server-github",
						Version:     "1.0.0",
						Description: "GitHub ops",
						Commands:    []string{"create_issue"},
					},
				},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ParseMessage([]byte(tt.input))
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}

			gotJSON, _ := json.Marshal(got)
			wantJSON, _ := json.Marshal(tt.want)

			if string(gotJSON) != string(wantJSON) {
				t.Errorf("mismatch:\n  got:  %s\n  want: %s", gotJSON, wantJSON)
			}
		})
	}
}

// TestParseMessage_AgentWorkerMessages tests round-trip for agent-worker message types.
func TestParseMessage_AgentWorkerMessages(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  any
	}{
		{
			name:  "agent_job",
			input: `{"type":"agent_job","job_id":"job-999","messages":[{"role":"user","content":"Hello"}],"system_prompt":"You are helpful"}`,
			want: AgentJobMessage{
				Type:         TypeAgentJob,
				JobID:        "job-999",
				Messages:     []AgentChatMsg{{Role: "user", Content: "Hello"}},
				SystemPrompt: "You are helpful",
			},
		},
		{
			name:  "agent_token",
			input: `{"type":"agent_token","job_id":"job-999","token":"Hi"}`,
			want: AgentTokenMessage{
				Type:  TypeAgentToken,
				JobID: "job-999",
				Token: "Hi",
			},
		},
		{
			name:  "agent_complete",
			input: `{"type":"agent_complete","job_id":"job-999","stop_reason":"end_turn"}`,
			want: AgentCompleteMessage{
				Type:       TypeAgentComplete,
				JobID:      "job-999",
				StopReason: "end_turn",
			},
		},
		{
			name:  "agent_error",
			input: `{"type":"agent_error","job_id":"job-999","error":"connection lost"}`,
			want: AgentErrorMessage{
				Type:  TypeAgentError,
				JobID: "job-999",
				Error: "connection lost",
			},
		},
		{
			name:  "agent_tool_call",
			input: `{"type":"agent_tool_call","job_id":"job-999","id":"call_1","name":"web_search","arguments":"{\"q\":\"test\"}"}`,
			want: AgentToolCallMessage{
				Type:      TypeAgentToolCall,
				JobID:     "job-999",
				ID:        "call_1",
				Name:      "web_search",
				Arguments: `{"q":"test"}`,
			},
		},
		{
			name:  "agent_tool_result success",
			input: `{"type":"agent_tool_result","job_id":"job-999","tool_call_id":"call_1","content":"result data","is_error":false}`,
			want: AgentToolResultMessage{
				Type:       TypeAgentToolResult,
				JobID:      "job-999",
				ToolCallID: "call_1",
				Content:    "result data",
				IsError:    false,
			},
		},
		{
			name:  "agent_tool_result error",
			input: `{"type":"agent_tool_result","job_id":"job-999","tool_call_id":"call_2","content":"error message","is_error":true}`,
			want: AgentToolResultMessage{
				Type:       TypeAgentToolResult,
				JobID:      "job-999",
				ToolCallID: "call_2",
				Content:    "error message",
				IsError:    true,
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ParseMessage([]byte(tt.input))
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}

			gotJSON, _ := json.Marshal(got)
			wantJSON, _ := json.Marshal(tt.want)

			if string(gotJSON) != string(wantJSON) {
				t.Errorf("mismatch:\n  got:  %s\n  want: %s", gotJSON, wantJSON)
			}
		})
	}
}

// TestAgentHelloAck tests agent hello_ack round-trip (not part of ParseMessage).
func TestAgentHelloAck(t *testing.T) {
	input := `{"type":"agent_hello_ack","status":"ok","agent_id":"agt-001","external_tools":[{"name":"github__list_repos","description":"List repos","parameters":{"type":"object"},"source":"mcp:github"}]}`
	
	var ack AgentHelloAckMessage
	if err := json.Unmarshal([]byte(input), &ack); err != nil {
		t.Fatalf("unmarshal failed: %v", err)
	}

	if ack.Status != "ok" {
		t.Errorf("status = %q, want ok", ack.Status)
	}
	if ack.AgentID != "agt-001" {
		t.Errorf("agent_id = %q, want agt-001", ack.AgentID)
	}
	if len(ack.ExternalTools) != 1 {
		t.Fatalf("external_tools length = %d, want 1", len(ack.ExternalTools))
	}
	if ack.ExternalTools[0].Name != "github__list_repos" {
		t.Errorf("tool name = %q, want github__list_repos", ack.ExternalTools[0].Name)
	}
}

// TestAgentReloadPolicy tests agent reload_policy round-trip.
func TestAgentReloadPolicy(t *testing.T) {
	input := `{"type":"reload_policy","external_tools":[{"name":"jira__create_ticket","description":"Create ticket","parameters":{"type":"object"},"source":"mcp:jira"}]}`
	
	var reload AgentReloadPolicyMessage
	if err := json.Unmarshal([]byte(input), &reload); err != nil {
		t.Fatalf("unmarshal failed: %v", err)
	}

	if reload.Type != "reload_policy" {
		t.Errorf("type = %q, want reload_policy", reload.Type)
	}
	if len(reload.ExternalTools) != 1 {
		t.Fatalf("external_tools length = %d, want 1", len(reload.ExternalTools))
	}
	if reload.ExternalTools[0].Name != "jira__create_ticket" {
		t.Errorf("tool name = %q, want jira__create_ticket", reload.ExternalTools[0].Name)
	}
}

// TestParseToolsMessage tests that ParseToolsMessage delegates correctly.
func TestParseToolsMessage(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		wantErr bool
	}{
		{
			name:    "valid tool_job",
			input:   `{"type":"tool_job","job_id":"job-1","parent_job_id":"p-1","tool_name":"web_search","tool_call_id":"call_1","arguments":"{}"}`,
			wantErr: false,
		},
		{
			name:    "valid tool_result",
			input:   `{"type":"tool_result","job_id":"job-1","parent_job_id":"p-1","tool_call_id":"call_1","tool_name":"web_search","result":"ok"}`,
			wantErr: false,
		},
		{
			name:    "valid policy_reloaded",
			input:   `{"type":"policy_reloaded","advertised_tools":[],"mcp_schemas":[]}`,
			wantErr: false,
		},
		{
			name:    "unknown message type",
			input:   `{"type":"unknown"}`,
			wantErr: true,
		},
		{
			name:    "invalid json",
			input:   `{not json}`,
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := ParseToolsMessage([]byte(tt.input))
			if (err != nil) != tt.wantErr {
				t.Errorf("ParseToolsMessage() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

