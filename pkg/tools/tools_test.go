package tools

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// --- code_executor tests ---

func TestCodeExecutor_BasicArithmetic(t *testing.T) {
	args, _ := json.Marshal(map[string]any{"code": "1 + 2"})
	result, err := CodeExecutor(context.Background(), args)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var out codeExecutorResult
	if err := json.Unmarshal(result.Output, &out); err != nil {
		t.Fatalf("parsing output: %v", err)
	}
	if out.Error != "" {
		t.Fatalf("unexpected execution error: %s", out.Error)
	}
	// 1+2 evaluates to 3 (float64 in JS)
	if out.ReturnValue != float64(3) {
		t.Errorf("expected return value 3, got %v (%T)", out.ReturnValue, out.ReturnValue)
	}
}

func TestCodeExecutor_ConsoleLog(t *testing.T) {
	args, _ := json.Marshal(map[string]any{"code": `console.log("hello", "world")`})
	result, err := CodeExecutor(context.Background(), args)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var out codeExecutorResult
	if err := json.Unmarshal(result.Output, &out); err != nil {
		t.Fatalf("parsing output: %v", err)
	}
	if out.Error != "" {
		t.Fatalf("unexpected execution error: %s", out.Error)
	}
	if out.Output != "hello world" {
		t.Errorf("expected 'hello world', got %q", out.Output)
	}
}

func TestCodeExecutor_RuntimeError(t *testing.T) {
	args, _ := json.Marshal(map[string]any{"code": "undefinedFunction()"})
	result, err := CodeExecutor(context.Background(), args)
	if err != nil {
		t.Fatalf("unexpected Go error: %v", err)
	}

	var out codeExecutorResult
	if err := json.Unmarshal(result.Output, &out); err != nil {
		t.Fatalf("parsing output: %v", err)
	}
	if out.Error == "" {
		t.Error("expected a runtime error in output, got none")
	}
}

func TestCodeExecutor_Timeout(t *testing.T) {
	args, _ := json.Marshal(map[string]any{
		"code":            "while(true) {}",
		"timeout_seconds": 1,
	})
	start := time.Now()
	result, err := CodeExecutor(context.Background(), args)
	elapsed := time.Since(start)

	if err != nil {
		t.Fatalf("unexpected Go error: %v", err)
	}
	if elapsed > 3*time.Second {
		t.Errorf("execution took too long: %s (expected ~1s)", elapsed)
	}

	var out codeExecutorResult
	if err := json.Unmarshal(result.Output, &out); err != nil {
		t.Fatalf("parsing output: %v", err)
	}
	if out.Error == "" {
		t.Error("expected timeout error in output, got none")
	}
}

func TestCodeExecutor_EmptyCode(t *testing.T) {
	args, _ := json.Marshal(map[string]any{"code": ""})
	_, err := CodeExecutor(context.Background(), args)
	if err == nil {
		t.Error("expected error for empty code, got none")
	}
}

func TestCodeExecutor_NoNetwork(t *testing.T) {
	// fetch and XMLHttpRequest should be undefined in the sandbox
	args, _ := json.Marshal(map[string]any{"code": "typeof fetch"})
	result, err := CodeExecutor(context.Background(), args)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var out codeExecutorResult
	if err := json.Unmarshal(result.Output, &out); err != nil {
		t.Fatalf("parsing output: %v", err)
	}
	if out.Error != "" {
		t.Fatalf("unexpected execution error: %s", out.Error)
	}
	if out.ReturnValue != "undefined" {
		t.Errorf("expected fetch to be undefined, got %v", out.ReturnValue)
	}
}

func TestCodeExecutor_BtoaAtob(t *testing.T) {
	args, _ := json.Marshal(map[string]any{"code": `
		var encoded = btoa("hello world");
		var decoded = atob(encoded);
		console.log(encoded);
		decoded;
	`})
	result, err := CodeExecutor(context.Background(), args)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	var out codeExecutorResult
	if err := json.Unmarshal(result.Output, &out); err != nil {
		t.Fatalf("parsing output: %v", err)
	}
	if out.Error != "" {
		t.Fatalf("unexpected execution error: %s", out.Error)
	}
	if out.ReturnValue != "hello world" {
		t.Errorf("expected 'hello world', got %v", out.ReturnValue)
	}
	if out.Output != "aGVsbG8gd29ybGQ=" {
		t.Errorf("expected base64 'aGVsbG8gd29ybGQ=', got %s", out.Output)
	}
}

func TestCodeExecutor_Buffer(t *testing.T) {
	args, _ := json.Marshal(map[string]any{"code": `
		var buf = Buffer.from("hello", "utf8");
		var b64 = buf.toString("base64");
		var hexStr = buf.toString("hex");
		console.log(b64);
		console.log(hexStr);
		buf.length;
	`})
	result, err := CodeExecutor(context.Background(), args)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	var out codeExecutorResult
	if err := json.Unmarshal(result.Output, &out); err != nil {
		t.Fatalf("parsing output: %v", err)
	}
	if out.Error != "" {
		t.Fatalf("unexpected execution error: %s", out.Error)
	}
	// "hello" in base64 is "aGVsbG8=" and hex is "68656c6c6f"
	lines := strings.Split(out.Output, "\n")
	if len(lines) < 2 || lines[0] != "aGVsbG8=" {
		t.Errorf("expected base64 'aGVsbG8=', got %q (lines: %v)", out.Output, lines)
	}
	if lines[1] != "68656c6c6f" {
		t.Errorf("expected hex '68656c6c6f', got %q", lines[1])
	}
	// goja exports JS numbers as float64
	if out.ReturnValue != float64(5) && out.ReturnValue != int64(5) {
		t.Errorf("expected length 5, got %v (%T)", out.ReturnValue, out.ReturnValue)
	}
}

func TestCodeExecutor_ConsoleWarnError(t *testing.T) {
	args, _ := json.Marshal(map[string]any{"code": `
		console.warn("watch out");
		console.error("oh no");
	`})
	result, err := CodeExecutor(context.Background(), args)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	var out codeExecutorResult
	if err := json.Unmarshal(result.Output, &out); err != nil {
		t.Fatalf("parsing output: %v", err)
	}
	if out.Error != "" {
		t.Fatalf("unexpected execution error: %s", out.Error)
	}
	if !strings.Contains(out.Output, "[warn] watch out") {
		t.Errorf("expected warn prefix, got %q", out.Output)
	}
	if !strings.Contains(out.Output, "[error] oh no") {
		t.Errorf("expected error prefix, got %q", out.Output)
	}
}

func TestCodeExecutor_CryptoRandomUUID(t *testing.T) {
	args, _ := json.Marshal(map[string]any{"code": `crypto.randomUUID()`})
	result, err := CodeExecutor(context.Background(), args)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	var out codeExecutorResult
	if err := json.Unmarshal(result.Output, &out); err != nil {
		t.Fatalf("parsing output: %v", err)
	}
	if out.Error != "" {
		t.Fatalf("unexpected execution error: %s", out.Error)
	}
	uuid, ok := out.ReturnValue.(string)
	if !ok || len(uuid) != 36 {
		t.Errorf("expected UUID string, got %v", out.ReturnValue)
	}
}

// --- web_fetch tests ---

func TestWebFetch_GetJSON(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"message":"hello"}`))
	}))
	defer server.Close()

	args, _ := json.Marshal(map[string]any{"url": server.URL})
	result, err := WebFetch(context.Background(), args)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var out webFetchResult
	if err := json.Unmarshal(result.Output, &out); err != nil {
		t.Fatalf("parsing output: %v", err)
	}
	if out.Error != "" {
		t.Fatalf("unexpected fetch error: %s", out.Error)
	}
	if out.Status != 200 {
		t.Errorf("expected status 200, got %d", out.Status)
	}
	// JSON is non-HTML — returned as-is in Content field.
	if out.Content != `{"message":"hello"}` {
		t.Errorf("unexpected content: %q", out.Content)
	}
}

func TestWebFetch_404(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "not found", http.StatusNotFound)
	}))
	defer server.Close()

	args, _ := json.Marshal(map[string]any{"url": server.URL})
	result, err := WebFetch(context.Background(), args)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var out webFetchResult
	if err := json.Unmarshal(result.Output, &out); err != nil {
		t.Fatalf("parsing output: %v", err)
	}
	// We still return a result (not a Go error) for HTTP errors — the LLM should see the 404.
	if out.Status != 404 {
		t.Errorf("expected status 404, got %d", out.Status)
	}
	if out.Error != "" {
		t.Errorf("unexpected error field: %s (HTTP 404 is not a Go error)", out.Error)
	}
}

func TestWebFetch_BinaryRejected(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/png")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte{0x89, 0x50, 0x4e, 0x47}) // PNG header
	}))
	defer server.Close()

	args, _ := json.Marshal(map[string]any{"url": server.URL})
	result, err := WebFetch(context.Background(), args)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var out webFetchResult
	if err := json.Unmarshal(result.Output, &out); err != nil {
		t.Fatalf("parsing output: %v", err)
	}
	if out.Error == "" {
		t.Error("expected error for binary content type, got none")
	}
}

func TestWebFetch_OutputTruncation(t *testing.T) {
	// Build a plain-text body larger than defaultOutputLimit.
	words := strings.Repeat("hello world\n", 2000) // ~24 000 chars
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(words))
	}))
	defer server.Close()

	args, _ := json.Marshal(map[string]any{"url": server.URL})
	result, err := WebFetch(context.Background(), args)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var out webFetchResult
	if err := json.Unmarshal(result.Output, &out); err != nil {
		t.Fatalf("parsing output: %v", err)
	}
	if out.Truncated == "" {
		t.Error("expected non-empty Truncated hint")
	}
	if len(out.Content) > defaultOutputLimit {
		t.Errorf("content exceeds defaultOutputLimit: got %d chars", len(out.Content))
	}
	if out.TotalChars == 0 {
		t.Error("expected TotalChars to be set")
	}
	if !containsStr(out.Truncated, "offset=") {
		t.Errorf("Truncated hint should contain 'offset=': %q", out.Truncated)
	}
}

func TestWebFetch_OutputPagination(t *testing.T) {
	// 100-char repeated body so we can paginate deterministically.
	body := strings.Repeat("abcdefghij", 10) // 100 chars
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		_, _ = w.Write([]byte(body))
	}))
	defer server.Close()

	unmarshalPage := func(t *testing.T, label string, res Result) webFetchResult {
		t.Helper()
		var out webFetchResult // fresh struct each time — avoids omitempty bleed-over
		if err := json.Unmarshal(res.Output, &out); err != nil {
			t.Fatalf("parsing %s: %v", label, err)
		}
		return out
	}

	// Page 1: limit=40 offset=0
	args, _ := json.Marshal(map[string]any{"url": server.URL, "limit": 40, "offset": 0})
	result, err := WebFetch(context.Background(), args)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	p1 := unmarshalPage(t, "page 1", result)
	if len(p1.Content) > 40 {
		t.Errorf("page 1 content too long: %d", len(p1.Content))
	}
	if p1.Truncated == "" {
		t.Error("expected truncation hint on page 1")
	}
	if p1.TotalChars != 100 {
		t.Errorf("expected TotalChars=100, got %d", p1.TotalChars)
	}

	// Page 2: limit=40 offset=40
	args, _ = json.Marshal(map[string]any{"url": server.URL, "limit": 40, "offset": 40})
	result, err = WebFetch(context.Background(), args)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	p2 := unmarshalPage(t, "page 2", result)
	if len(p2.Content) == 0 {
		t.Error("expected content on page 2")
	}

	// Page 3: offset=80, last 20 chars — should NOT be truncated
	args, _ = json.Marshal(map[string]any{"url": server.URL, "limit": 40, "offset": 80})
	result, err = WebFetch(context.Background(), args)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	p3 := unmarshalPage(t, "page 3", result)
	if p3.Truncated != "" {
		t.Errorf("page 3 should not be truncated: %q", p3.Truncated)
	}
	if len(p3.Content) != 20 {
		t.Errorf("page 3 expected 20 chars, got %d", len(p3.Content))
	}
}

func TestWebFetch_OffsetPastEnd(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		_, _ = w.Write([]byte("short content"))
	}))
	defer server.Close()

	args, _ := json.Marshal(map[string]any{"url": server.URL, "offset": 9999})
	result, err := WebFetch(context.Background(), args)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	var out webFetchResult
	if err := json.Unmarshal(result.Output, &out); err != nil {
		t.Fatalf("parsing output: %v", err)
	}
	if out.Content != "" {
		t.Errorf("expected empty content for out-of-range offset, got %q", out.Content)
	}
	if out.Truncated == "" {
		t.Error("expected truncation hint explaining offset is past end")
	}
}

func TestWebFetch_HeadPreflightRejectsBinary(t *testing.T) {
	// Server returns image/png for HEAD but this test verifies the binary guard works
	// (either via preflight or the GET content-type check).
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/png")
		if r.Method == http.MethodHead {
			w.WriteHeader(http.StatusOK)
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte{0x89, 0x50, 0x4e, 0x47})
	}))
	defer server.Close()

	args, _ := json.Marshal(map[string]any{"url": server.URL})
	result, err := WebFetch(context.Background(), args)
	if err != nil {
		t.Fatalf("unexpected Go error: %v", err)
	}
	var out webFetchResult
	if err := json.Unmarshal(result.Output, &out); err != nil {
		t.Fatalf("parsing output: %v", err)
	}
	if out.Error == "" {
		t.Error("expected error for binary content type (caught by preflight or GET check)")
	}
}

func TestWebFetch_HeadPreflightSkippedOn405(t *testing.T) {
	// Server rejects HEAD (405) but serves valid text/html for GET.
	// Should proceed without error.
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodHead {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		w.Header().Set("Content-Type", "text/html")
		_, _ = w.Write([]byte(`<html><body><p>Content</p></body></html>`))
	}))
	defer server.Close()

	args, _ := json.Marshal(map[string]any{"url": server.URL, "mode": "full"})
	result, err := WebFetch(context.Background(), args)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	var out webFetchResult
	if err := json.Unmarshal(result.Output, &out); err != nil {
		t.Fatalf("parsing output: %v", err)
	}
	if out.Error != "" {
		t.Errorf("expected no error when HEAD returns 405, got: %s", out.Error)
	}
	if out.Content == "" {
		t.Error("expected content after fallback from 405 HEAD")
	}
}

func TestWebFetch_SearchFilter(t *testing.T) {
	body := "line 1\nline 2 match\nline 3\nline 4\nline 5 match\nline 6"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(body))
	}))
	defer server.Close()

	args, _ := json.Marshal(map[string]any{"url": server.URL, "search": "match"})
	result, err := WebFetch(context.Background(), args)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var out webFetchResult
	if err := json.Unmarshal(result.Output, &out); err != nil {
		t.Fatalf("parsing output: %v", err)
	}
	if out.Error != "" {
		t.Fatalf("unexpected error: %s", out.Error)
	}
	// The filtered result must contain both "match" occurrences.
	if !containsStr(out.Content, "line 2 match") || !containsStr(out.Content, "line 5 match") {
		t.Errorf("search filter did not preserve matched lines: %q", out.Content)
	}
}

func TestWebFetch_InvalidURL(t *testing.T) {
	args, _ := json.Marshal(map[string]any{"url": "ftp://example.com/file"})
	_, err := WebFetch(context.Background(), args)
	if err == nil {
		t.Error("expected error for non-http URL, got none")
	}
}

func TestWebFetch_InvalidMode(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		_, _ = w.Write([]byte(`<html><body>hello</body></html>`))
	}))
	defer server.Close()

	args, _ := json.Marshal(map[string]any{"url": server.URL, "mode": "invalid"})
	_, err := WebFetch(context.Background(), args)
	if err == nil {
		t.Error("expected error for invalid mode, got none")
	}
}

func TestWebFetch_ArticleMode_HTML(t *testing.T) {
	// Serve a minimal article-like HTML page.
	articleHTML := `<!DOCTYPE html>
<html>
<head><title>Test Article</title></head>
<body>
  <nav>Skip me</nav>
  <article>
    <h1>My Article</h1>
    <p>This is the first paragraph of a well-written article about interesting things.</p>
    <p>This is the second paragraph. It has more content so readability will pick it up.</p>
    <p>The third paragraph continues the article with even more fascinating information for the reader.</p>
  </article>
  <footer>Ignore footer</footer>
</body>
</html>`
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte(articleHTML))
	}))
	defer server.Close()

	args, _ := json.Marshal(map[string]any{"url": server.URL, "mode": "article"})
	result, err := WebFetch(context.Background(), args)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var out webFetchResult
	if err := json.Unmarshal(result.Output, &out); err != nil {
		t.Fatalf("parsing output: %v", err)
	}
	if out.Error != "" {
		t.Fatalf("unexpected error: %s", out.Error)
	}
	if out.Mode != "article" {
		t.Errorf("expected mode='article', got %q", out.Mode)
	}
	// Content should be Markdown (not raw HTML).
	if containsStr(out.Content, "<p>") || containsStr(out.Content, "<article>") {
		t.Errorf("content should be Markdown, not HTML: %q", out.Content)
	}
	// WordCount should be populated.
	if out.WordCount == 0 {
		t.Error("expected non-zero word count")
	}
}

func TestWebFetch_FullMode_HTML(t *testing.T) {
	pageHTML := `<html><body><h1>Hello</h1><p>World content here.</p><a href="/other">Link</a></body></html>`
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		_, _ = w.Write([]byte(pageHTML))
	}))
	defer server.Close()

	args, _ := json.Marshal(map[string]any{"url": server.URL, "mode": "full"})
	result, err := WebFetch(context.Background(), args)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var out webFetchResult
	if err := json.Unmarshal(result.Output, &out); err != nil {
		t.Fatalf("parsing output: %v", err)
	}
	if out.Error != "" {
		t.Fatalf("unexpected error: %s", out.Error)
	}
	if out.Mode != "full" {
		t.Errorf("expected mode='full', got %q", out.Mode)
	}
	// Should be markdown, not raw HTML tags.
	if containsStr(out.Content, "<p>") || containsStr(out.Content, "<h1>") {
		t.Errorf("expected Markdown output, got HTML: %q", out.Content)
	}
	if out.WordCount == 0 {
		t.Error("expected non-zero word count")
	}
}

func TestWebFetch_LinksMode(t *testing.T) {
	pageHTML := `<html><body>
  <a href="/about">About Us</a>
  <a href="https://external.com/page">External</a>
  <a href="#section">Skip</a>
  <a href="javascript:void(0)">Script</a>
</body></html>`
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		_, _ = w.Write([]byte(pageHTML))
	}))
	defer server.Close()

	args, _ := json.Marshal(map[string]any{"url": server.URL, "mode": "links"})
	result, err := WebFetch(context.Background(), args)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var out webFetchResult
	if err := json.Unmarshal(result.Output, &out); err != nil {
		t.Fatalf("parsing output: %v", err)
	}
	if out.Error != "" {
		t.Fatalf("unexpected error: %s", out.Error)
	}
	if out.Mode != "links" {
		t.Errorf("expected mode='links', got %q", out.Mode)
	}
	// Should have 2 real links (fragment # and javascript: are excluded).
	if len(out.Links) != 2 {
		t.Errorf("expected 2 links, got %d: %+v", len(out.Links), out.Links)
	}
	// External link text should be preserved.
	foundExternal := false
	for _, l := range out.Links {
		if containsStr(l.URL, "external.com") && l.Text == "External" {
			foundExternal = true
		}
	}
	if !foundExternal {
		t.Errorf("expected to find external.com link with text 'External': %+v", out.Links)
	}
}

func TestWebFetch_PostBodyAsObject(t *testing.T) {
	var gotBody string
	var gotContentType string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotContentType = r.Header.Get("Content-Type")
		raw, _ := io.ReadAll(r.Body)
		gotBody = string(raw)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer server.Close()

	// LLM passes body as a JSON object (not a stringified JSON).
	args, _ := json.Marshal(map[string]any{
		"url":    server.URL,
		"method": "POST",
		"body":   map[string]any{"hello": "world"},
	})
	result, err := WebFetch(context.Background(), args)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	var out webFetchResult
	if err := json.Unmarshal(result.Output, &out); err != nil {
		t.Fatalf("parsing output: %v", err)
	}
	if out.Error != "" {
		t.Fatalf("tool error: %s", out.Error)
	}
	if gotBody != `{"hello":"world"}` {
		t.Errorf("expected body {\"hello\":\"world\"}, got %q", gotBody)
	}
	if !strings.HasPrefix(gotContentType, "application/json") {
		t.Errorf("expected Content-Type application/json, got %q", gotContentType)
	}
}

func TestWebFetch_PostBodyAsString(t *testing.T) {
	var gotBody string
	var gotContentType string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotContentType = r.Header.Get("Content-Type")
		raw, _ := io.ReadAll(r.Body)
		gotBody = string(raw)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer server.Close()

	// LLM passes body as a plain string.
	args, _ := json.Marshal(map[string]any{
		"url":    server.URL,
		"method": "POST",
		"body":   "name=alice&age=30",
		"headers": map[string]string{"Content-Type": "application/x-www-form-urlencoded"},
	})
	result, err := WebFetch(context.Background(), args)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	var out webFetchResult
	if err := json.Unmarshal(result.Output, &out); err != nil {
		t.Fatalf("parsing output: %v", err)
	}
	if out.Error != "" {
		t.Fatalf("tool error: %s", out.Error)
	}
	if gotBody != "name=alice&age=30" {
		t.Errorf("expected form body, got %q", gotBody)
	}
	if gotContentType != "application/x-www-form-urlencoded" {
		t.Errorf("expected form content-type, got %q", gotContentType)
	}
}

// --- Registry tests ---

func TestRegistry_DefaultTools(t *testing.T) {
	r := DefaultRegistry()
	names := r.Names()
	if len(names) < 2 {
		t.Errorf("expected at least 2 built-in tools, got %d: %v", len(names), names)
	}
}

func TestRegistry_UnknownTool(t *testing.T) {
	r := DefaultRegistry()
	_, err := r.Execute(context.Background(), "nonexistent_tool", json.RawMessage(`{}`))
	if err == nil {
		t.Error("expected error for unknown tool, got none")
	}
}

// containsStr is a helper since strings.Contains is enough.
func containsStr(haystack, needle string) bool {
	return len(haystack) >= len(needle) && (haystack == needle ||
		len(needle) == 0 ||
		containsStrLoop(haystack, needle))
}

func containsStrLoop(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
