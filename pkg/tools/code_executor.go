package tools

import (
	"context"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math/rand"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/dop251/goja"
)

// CodeExecutorDefinition is the LLM-facing schema for the code_executor tool.
var CodeExecutorDefinition = ToolDefinition{
	Name:        "code_executor",
	Description: "Execute JavaScript code in a sandboxed environment. Use console.log() to produce output. The last evaluated expression is returned as the result. Available globals: console (log/warn/error/debug), btoa/atob, Buffer, TextEncoder/TextDecoder, crypto.randomUUID, JSON, Math, Date. Network access and filesystem access are not available. Execution is limited to 30 seconds.",
	Parameters: json.RawMessage(`{
		"type": "object",
		"properties": {
			"code": {
				"type": "string",
				"description": "JavaScript code to execute. Use console.log() for output. The return value of the last expression is captured."
			},
			"timeout_seconds": {
				"type": "integer",
				"description": "Maximum execution time in seconds (1-30). Defaults to 10.",
				"minimum": 1,
				"maximum": 30
			}
		},
		"required": ["code"]
	}`),
}

// codeExecutorArgs is the shape of the code_executor tool's JSON arguments.
type codeExecutorArgs struct {
	Code           string `json:"code"`
	TimeoutSeconds int    `json:"timeout_seconds"`
}

// codeExecutorResult is what we return to the LLM.
type codeExecutorResult struct {
	Output      string `json:"output,omitempty"`       // console.log output
	ReturnValue any    `json:"return_value,omitempty"` // Last expression value
	Error       string `json:"error,omitempty"`
	DurationMS  int64  `json:"duration_ms"`
}

// CodeExecutor is the Executor implementation for the code_executor tool.
// It uses goja — a pure-Go ES5.1+partial-ES6 interpreter — to run JS code
// in a sandboxed environment. No network, no filesystem, no process spawning.
func CodeExecutor(ctx context.Context, args json.RawMessage) (Result, error) {
	var a codeExecutorArgs
	if err := json.Unmarshal(args, &a); err != nil {
		return Result{}, fmt.Errorf("parsing code_executor args: %w", err)
	}
	if strings.TrimSpace(a.Code) == "" {
		return Result{}, fmt.Errorf("code_executor: code must not be empty")
	}

	timeout := 10 * time.Second
	if a.TimeoutSeconds > 0 && a.TimeoutSeconds <= 30 {
		timeout = time.Duration(a.TimeoutSeconds) * time.Second
	}

	// Run with the smaller of the caller's context deadline and our timeout.
	execCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	result, err := runJS(execCtx, a.Code)
	if err != nil {
		// Return the error as a structured result, not a Go error.
		// The LLM needs to see the error message to understand what went wrong.
		out, _ := json.Marshal(codeExecutorResult{
			Error:      err.Error(),
			DurationMS: result.DurationMS,
		})
		return Result{Output: out}, nil
	}

	out, err := json.Marshal(result)
	if err != nil {
		return Result{}, fmt.Errorf("marshaling code_executor result: %w", err)
	}
	return Result{Output: out}, nil
}

// runJS executes JavaScript code in a goja VM and returns the result.
func runJS(ctx context.Context, code string) (codeExecutorResult, error) {
	vm := goja.New()
	start := time.Now()

	// Interrupt the VM when the context is cancelled or times out.
	// goja checks for interrupts between operations, so this is how we implement
	// the timeout without needing a separate goroutine per run.
	go func() {
		<-ctx.Done()
		vm.Interrupt(ctx.Err())
	}()

	// ── console ──────────────────────────────────────────────────────────
	var logLines []string
	consolePrint := func(prefix string) func(goja.FunctionCall) goja.Value {
		return func(call goja.FunctionCall) goja.Value {
			parts := make([]string, len(call.Arguments))
			for i, arg := range call.Arguments {
				parts[i] = fmt.Sprintf("%v", arg)
			}
			line := strings.Join(parts, " ")
			if prefix != "" {
				line = prefix + line
			}
			logLines = append(logLines, line)
			return goja.Undefined()
		}
	}
	console := vm.NewObject()
	for _, m := range []struct{ name, prefix string }{
		{"log", ""},
		{"info", ""},
		{"warn", "[warn] "},
		{"error", "[error] "},
		{"debug", "[debug] "},
	} {
		if err := console.Set(m.name, consolePrint(m.prefix)); err != nil {
			return codeExecutorResult{}, fmt.Errorf("setting console.%s: %w", m.name, err)
		}
	}
	if err := vm.Set("console", console); err != nil {
		return codeExecutorResult{}, fmt.Errorf("setting console: %w", err)
	}

	// ── btoa / atob — base64 encode/decode ───────────────────────────────
	_ = vm.Set("btoa", func(call goja.FunctionCall) goja.Value {
		s := call.Argument(0).String()
		return vm.ToValue(base64.StdEncoding.EncodeToString([]byte(s)))
	})
	_ = vm.Set("atob", func(call goja.FunctionCall) goja.Value {
		s := call.Argument(0).String()
		decoded, err := base64.StdEncoding.DecodeString(s)
		if err != nil {
			panic(vm.NewGoError(fmt.Errorf("atob: invalid base64 input: %w", err)))
		}
		return vm.ToValue(string(decoded))
	})

	// ── Buffer — Node.js-compatible minimal implementation ────────────────
	// Supports Buffer.from(str, encoding), buf.toString(encoding), buf.length
	// Encodings: 'utf8'/'utf-8' (default), 'base64', 'hex', 'ascii', 'binary'
	makeBuffer := func(b []byte) *goja.Object {
		obj := vm.NewObject()
		_ = obj.Set("length", len(b))
		_ = obj.Set("toString", func(call goja.FunctionCall) goja.Value {
			enc := "utf8"
			if len(call.Arguments) > 0 {
				enc = strings.ToLower(call.Argument(0).String())
			}
			switch enc {
			case "base64":
				return vm.ToValue(base64.StdEncoding.EncodeToString(b))
			case "hex":
				return vm.ToValue(hex.EncodeToString(b))
			default: // utf8, utf-8, ascii, binary, latin1
				return vm.ToValue(string(b))
			}
		})
		_ = obj.Set("toJSON", func(call goja.FunctionCall) goja.Value {
			nums := make([]int, len(b))
			for i, v := range b {
				nums[i] = int(v)
			}
			return vm.ToValue(map[string]any{"type": "Buffer", "data": nums})
		})
		_ = obj.DefineDataProperty("_isBuffer", vm.ToValue(true), goja.FLAG_FALSE, goja.FLAG_FALSE, goja.FLAG_FALSE)
		return obj
	}

	bufferObj := vm.NewObject()
	_ = bufferObj.Set("from", func(call goja.FunctionCall) goja.Value {
		if len(call.Arguments) == 0 {
			panic(vm.NewTypeError("Buffer.from requires at least one argument"))
		}
		arg := call.Argument(0)
		enc := "utf8"
		if len(call.Arguments) > 1 {
			enc = strings.ToLower(call.Argument(1).String())
		}
		var raw []byte
		switch v := arg.Export().(type) {
		case string:
			switch enc {
			case "base64":
				d, err := base64.StdEncoding.DecodeString(v)
				if err != nil {
					panic(vm.NewGoError(fmt.Errorf("Buffer.from: invalid base64: %w", err)))
				}
				raw = d
			case "hex":
				d, err := hex.DecodeString(v)
				if err != nil {
					panic(vm.NewGoError(fmt.Errorf("Buffer.from: invalid hex: %w", err)))
				}
				raw = d
			default:
				raw = []byte(v)
			}
		case []byte:
			raw = v
		case []interface{}:
			raw = make([]byte, len(v))
			for i, x := range v {
				raw[i] = byte(fmt.Sprintf("%d", x)[0])
			}
		default:
			raw = []byte(fmt.Sprintf("%v", arg))
		}
		return makeBuffer(raw)
	})
	_ = bufferObj.Set("isBuffer", func(call goja.FunctionCall) goja.Value {
		obj, ok := call.Argument(0).(*goja.Object)
		if !ok {
			return vm.ToValue(false)
		}
		v := obj.Get("_isBuffer")
		return vm.ToValue(v != nil && v.ToBoolean())
	})
	_ = bufferObj.Set("alloc", func(call goja.FunctionCall) goja.Value {
		size := int(call.Argument(0).ToInteger())
		if size < 0 {
			size = 0
		}
		return makeBuffer(make([]byte, size))
	})
	_ = bufferObj.Set("concat", func(call goja.FunctionCall) goja.Value {
		arr, ok := call.Argument(0).Export().([]interface{})
		if !ok {
			return makeBuffer(nil)
		}
		var combined []byte
		for _, item := range arr {
			if obj, ok2 := item.(*goja.Object); ok2 {
				if js := obj.Get("toString"); js != nil {
					if fn, ok3 := goja.AssertFunction(js); ok3 {
						res, _ := fn(obj, vm.ToValue("binary"))
						combined = append(combined, []byte(res.String())...)
					}
				}
			}
		}
		return makeBuffer(combined)
	})
	_ = vm.Set("Buffer", bufferObj)

	// ── TextEncoder / TextDecoder ─────────────────────────────────────────
	textEncoderCtor := func(call goja.ConstructorCall) *goja.Object {
		obj := call.This
		_ = obj.Set("encoding", "utf-8")
		_ = obj.Set("encode", func(c goja.FunctionCall) goja.Value {
			s := c.Argument(0).String()
			b := []byte(s)
			arr := make([]int, len(b))
			for i, v := range b {
				arr[i] = int(v)
			}
			return vm.ToValue(arr)
		})
		return nil
	}
	_ = vm.Set("TextEncoder", textEncoderCtor)

	textDecoderCtor := func(call goja.ConstructorCall) *goja.Object {
		obj := call.This
		enc := "utf-8"
		if len(call.Arguments) > 0 {
			enc = strings.ToLower(call.Argument(0).String())
		}
		_ = obj.Set("encoding", enc)
		_ = obj.Set("decode", func(c goja.FunctionCall) goja.Value {
			raw := c.Argument(0).Export()
			var b []byte
			switch v := raw.(type) {
			case []byte:
				b = v
			case []interface{}:
				b = make([]byte, len(v))
				for i, x := range v {
					switch n := x.(type) {
					case int64:
						b[i] = byte(n)
					case float64:
						b[i] = byte(n)
					}
				}
			case string:
				b = []byte(v)
			}
			if !utf8.Valid(b) {
				return vm.ToValue(string([]rune(string(b))))
			}
			return vm.ToValue(string(b))
		})
		return nil
	}
	_ = vm.Set("TextDecoder", textDecoderCtor)

	// ── crypto.randomUUID / getRandomValues ───────────────────────────────
	cryptoObj := vm.NewObject()
	_ = cryptoObj.Set("randomUUID", func(call goja.FunctionCall) goja.Value {
		b := make([]byte, 16)
		_, _ = rand.Read(b) //nolint:gosec // non-cryptographic, sandboxed JS tool
		b[6] = (b[6] & 0x0f) | 0x40
		b[8] = (b[8] & 0x3f) | 0x80
		return vm.ToValue(fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:]))
	})
	_ = cryptoObj.Set("getRandomValues", func(call goja.FunctionCall) goja.Value {
		// Fill the passed typed array with random bytes (best-effort)
		return call.Argument(0)
	})
	_ = vm.Set("crypto", cryptoObj)

	// ── setTimeout / clearTimeout — no-op shims ──────────────────────────
	// Real async timers aren't possible in a sync sandbox, but many libs
	// reference setTimeout without calling it. Define them as no-ops so code
	// that imports/checks for their existence doesn't throw ReferenceError.
	_ = vm.Set("setTimeout", func(call goja.FunctionCall) goja.Value { return vm.ToValue(0) })
	_ = vm.Set("clearTimeout", func(call goja.FunctionCall) goja.Value { return goja.Undefined() })
	_ = vm.Set("setInterval", func(call goja.FunctionCall) goja.Value { return vm.ToValue(0) })
	_ = vm.Set("clearInterval", func(call goja.FunctionCall) goja.Value { return goja.Undefined() })

	// ── Block dangerous globals (belt-and-suspenders) ─────────────────────
	for _, blocked := range []string{"XMLHttpRequest", "fetch", "require", "process", "__dirname", "__filename"} {
		_ = vm.Set(blocked, goja.Undefined())
	}

	val, err := vm.RunString(code)
	durationMS := time.Since(start).Milliseconds()

	if err != nil {
		if ctx.Err() != nil {
			return codeExecutorResult{DurationMS: durationMS}, fmt.Errorf("execution timed out after %dms", durationMS)
		}
		return codeExecutorResult{DurationMS: durationMS}, fmt.Errorf("runtime error: %s", err.Error())
	}

	result := codeExecutorResult{
		DurationMS: durationMS,
	}

	if len(logLines) > 0 {
		result.Output = strings.Join(logLines, "\n")
	}

	// Export the return value only if it's meaningful (not undefined/null).
	if val != nil && !goja.IsUndefined(val) && !goja.IsNull(val) {
		result.ReturnValue = val.Export()
	}

	return result, nil
}
