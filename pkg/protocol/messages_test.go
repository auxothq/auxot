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
					{Role: "system", Content: "You are helpful."},
					{Role: "user", Content: "Hi"},
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
					{Role: "user", Content: "What is the weather?"},
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
			{Role: "system", Content: "Be helpful."},
			{Role: "user", Content: "Hello"},
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
	if got.Messages[1].Content != "Hello" {
		t.Errorf("Messages[1].Content: got %q, want %q", got.Messages[1].Content, "Hello")
	}
	if got.Temperature == nil || *got.Temperature != 0.7 {
		t.Errorf("Temperature: got %v, want 0.7", got.Temperature)
	}
	if got.MaxTokens == nil || *got.MaxTokens != 1024 {
		t.Errorf("MaxTokens: got %v, want 1024", got.MaxTokens)
	}
}

// helpers for pointer types in test data
func ptrFloat64(v float64) *float64 { return &v }
func ptrInt(v int) *int             { return &v }
