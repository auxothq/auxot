package tools

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	nurl "net/url"
	"regexp"
	"strings"
	"time"

	htmltomarkdown "github.com/JohannesKaufmann/html-to-markdown/v2"
	readability "github.com/go-shiori/go-readability"
	"golang.org/x/net/html"
)

const (
	// rawFetchCap is the maximum raw bytes we download before processing.
	// This is intentionally generous — readability/markdown conversion is applied AFTER
	// fetching, so we need the full document. 5 MB covers any real-world article.
	rawFetchCap = 5 * 1024 * 1024 // 5 MB

	// defaultOutputLimit and maxOutputLimit bound the processed output returned to the LLM.
	// ~18 000 chars ≈ 4 500 tokens ≈ a full article read. The LLM can paginate with offset.
	defaultOutputLimit = 18_000
	maxOutputLimit     = 100_000

	// defaultLinksLimit is the max links returned per call in 'links' mode.
	defaultLinksLimit = 200
	maxLinksLimit     = 2_000
)

// WebFetchDefinition is the LLM-facing schema for the web_fetch tool.
var WebFetchDefinition = ToolDefinition{
	Name: "web_fetch",
	Description: "Fetch the content of a URL and return it in a format optimized for LLM consumption. " +
		"A preflight HEAD request checks the content type before downloading. " +
		"HTML pages are converted to Markdown. Three modes: " +
		"'article' (default) uses readability to extract the main body then converts to Markdown; " +
		"'full' converts the entire HTML page to Markdown with no content filtering; " +
		"'links' returns a structured list of all links. " +
		"Non-HTML responses (JSON, plain text) are returned as-is. " +
		"Output is paginated: use offset + limit to read large pages in chunks. " +
		"word_count and total_chars are always present so you can decide whether to paginate.",
	Parameters: json.RawMessage(`{
		"type": "object",
		"properties": {
			"url": {
				"type": "string",
				"description": "The URL to fetch (must be http:// or https://)"
			},
			"mode": {
				"type": "string",
				"enum": ["article", "links", "full"],
				"description": "Content extraction mode for HTML pages. 'article' (default): readability + Markdown, best for articles and docs. 'links': structured link list, best for navigation and site discovery. 'full': full HTML to Markdown, no content filtering, use when 'article' returns too little."
			},
			"offset": {
				"type": "integer",
				"description": "Character offset into the processed output to start from. Use with 'truncated' hint to paginate large pages. Default: 0.",
				"minimum": 0
			},
			"limit": {
				"type": "integer",
				"description": "Maximum characters of processed output to return. Default: 18000 (~4500 tokens). Max: 100000. In 'links' mode this is the max number of links.",
				"minimum": 1,
				"maximum": 100000
			},
			"method": {
				"type": "string",
				"description": "HTTP method. Defaults to GET.",
				"enum": ["GET", "POST", "PUT", "DELETE", "HEAD"]
			},
			"headers": {
				"type": "object",
				"description": "Additional HTTP request headers. Example: {\"Authorization\": \"Bearer token\", \"Accept\": \"application/json\"}.",
				"additionalProperties": { "type": "string" }
			},
			"body": {
				"type": "string",
				"description": "Request body for POST/PUT. Always pass as a string. For JSON APIs: \"{\\\"key\\\":\\\"value\\\"}\". For form data: \"field1=value1&field2=value2\"."
			},
			"search": {
				"type": "string",
				"description": "Regex: return only lines matching this pattern (with 3 lines of context). Applied to 'article' and 'full' modes after Markdown conversion."
			},
			"timeout_seconds": {
				"type": "integer",
				"description": "Request timeout in seconds (1–60). Default: 30.",
				"minimum": 1,
				"maximum": 60
			}
		},
		"required": ["url"]
	}`),
}

// webFetchArgs is the shape of the web_fetch tool's JSON arguments.
type webFetchArgs struct {
	URL            string            `json:"url"`
	Mode           string            `json:"mode"`
	Offset         int               `json:"offset"`
	Limit          int               `json:"limit"`
	Method         string            `json:"method"`
	Headers        map[string]string `json:"headers"`
	// Body accepts either a JSON string ("raw text") or a JSON object/array.
	// When a JSON object or array is provided, it is re-serialised and
	// Content-Type: application/json is added automatically (unless overridden).
	Body           json.RawMessage   `json:"body"`
	Search         string            `json:"search"`
	TimeoutSeconds int               `json:"timeout_seconds"`
}

// webFetchLink is a single link extracted in links mode.
type webFetchLink struct {
	URL  string `json:"url"`
	Text string `json:"text"`
}

// webFetchResult is what we return to the LLM.
type webFetchResult struct {
	URL         string            `json:"url"`
	Mode        string            `json:"mode,omitempty"`
	Status      int               `json:"status"`
	ContentType string            `json:"content_type,omitempty"`
	Headers     map[string]string `json:"headers,omitempty"`
	Title       string            `json:"title,omitempty"`
	Content     string            `json:"content,omitempty"`
	Links       []webFetchLink    `json:"links,omitempty"`
	WordCount   int               `json:"word_count,omitempty"`
	TotalChars  int               `json:"total_chars,omitempty"`
	// Truncated is a human-readable hint when output was cut; empty means not truncated.
	Truncated string `json:"truncated,omitempty"`
	Note      string `json:"note,omitempty"`
	Error     string `json:"error,omitempty"`
	DurationMS int64 `json:"duration_ms"`
}

// sharedClient is a package-level HTTP client reused across fetch calls.
var sharedClient = &http.Client{
	Timeout: 60 * time.Second,
}

// allowedContentTypes is the set of MIME type prefixes we return.
// Binary types are rejected to keep responses LLM-readable.
var allowedContentTypes = []string{
	"text/",
	"application/json",
	"application/xml",
	"application/xhtml",
	"application/javascript",
	"application/ld+json",
	"application/rss+xml",
	"application/atom+xml",
}

// WebFetch is the Executor implementation for the web_fetch tool.
func WebFetch(ctx context.Context, args json.RawMessage) (Result, error) {
	var a webFetchArgs
	if err := json.Unmarshal(args, &a); err != nil {
		return Result{}, fmt.Errorf("parsing web_fetch args: %w", err)
	}

	if a.URL == "" {
		return Result{}, fmt.Errorf("web_fetch: url is required")
	}
	if !strings.HasPrefix(a.URL, "http://") && !strings.HasPrefix(a.URL, "https://") {
		return Result{}, fmt.Errorf("web_fetch: url must start with http:// or https://")
	}

	mode := strings.ToLower(a.Mode)
	if mode == "" {
		mode = "article"
	}
	if mode != "article" && mode != "links" && mode != "full" {
		return Result{}, fmt.Errorf("web_fetch: mode must be 'article', 'links', or 'full'")
	}

	method := strings.ToUpper(a.Method)
	if method == "" {
		method = "GET"
	}

	outputLimit := a.Limit
	if outputLimit <= 0 || outputLimit > maxOutputLimit {
		outputLimit = defaultOutputLimit
	}

	outputOffset := a.Offset
	if outputOffset < 0 {
		outputOffset = 0
	}

	timeoutSecs := a.TimeoutSeconds
	if timeoutSecs <= 0 || timeoutSecs > 60 {
		timeoutSecs = 30
	}

	reqCtx, cancel := context.WithTimeout(ctx, time.Duration(timeoutSecs)*time.Second)
	defer cancel()

	// ── Preflight HEAD ─────────────────────────────────────────────────────────
	// For GET requests, do a cheap HEAD first to detect binary content without
	// downloading the body. Skip preflight for POST/PUT/DELETE (not idempotent
	// and many servers don't support HEAD on those paths).
	if method == "GET" {
		if refusedErr := headPreflight(reqCtx, a.URL, a.Headers); refusedErr != "" {
			return fetchResult(webFetchResult{
				URL:   a.URL,
				Mode:  mode,
				Error: refusedErr,
			})
		}
	}

	// ── Main request ───────────────────────────────────────────────────────────
	// Resolve the request body: accept a JSON object/array OR a plain string.
	// When the LLM passes a JSON object the body is re-serialised as compact JSON
	// and Content-Type: application/json is injected (unless the caller sets it).
	bodyReader, autoContentType := resolveBody(a.Body)

	req, err := http.NewRequestWithContext(reqCtx, method, a.URL, bodyReader)
	if err != nil {
		return fetchResult(webFetchResult{
			URL:   a.URL,
			Mode:  mode,
			Error: fmt.Sprintf("building request: %s", err.Error()),
		})
	}

	req.Header.Set("User-Agent", "auxot-tools/1.0 (web_fetch)")
	// Apply auto Content-Type before caller headers so explicit headers win.
	if autoContentType != "" {
		req.Header.Set("Content-Type", autoContentType)
	}
	for k, v := range a.Headers {
		req.Header.Set(k, v)
	}

	start := time.Now()
	resp, err := sharedClient.Do(req)
	durationMS := time.Since(start).Milliseconds()

	if err != nil {
		errMsg := err.Error()
		if reqCtx.Err() != nil {
			errMsg = fmt.Sprintf("request timed out after %ds", timeoutSecs)
		}
		return fetchResult(webFetchResult{
			URL:        a.URL,
			Mode:       mode,
			Error:      errMsg,
			DurationMS: durationMS,
		})
	}
	defer resp.Body.Close()

	contentType := resp.Header.Get("Content-Type")
	if !isAllowedContentType(contentType) {
		return fetchResult(webFetchResult{
			URL:         a.URL,
			Mode:        mode,
			Status:      resp.StatusCode,
			ContentType: contentType,
			Error:       fmt.Sprintf("content type %q is not text-based; binary responses are not returned", contentType),
			DurationMS:  durationMS,
		})
	}

	// Read up to rawFetchCap bytes. We use a large cap so readability/markdown
	// conversion operates on the full document; output truncation happens later.
	rawBytes, err := io.ReadAll(io.LimitReader(resp.Body, rawFetchCap))
	durationMS = time.Since(start).Milliseconds()
	if err != nil {
		return fetchResult(webFetchResult{
			URL:        a.URL,
			Mode:       mode,
			Status:     resp.StatusCode,
			Error:      fmt.Sprintf("reading response body: %s", err.Error()),
			DurationMS: durationMS,
		})
	}

	rawBody := string(rawBytes)

	// Collect a subset of response headers useful to the LLM.
	respHeaders := map[string]string{}
	for _, h := range []string{"Content-Type", "Content-Length", "Last-Modified", "ETag", "Location"} {
		if v := resp.Header.Get(h); v != "" {
			respHeaders[h] = v
		}
	}

	res := webFetchResult{
		URL:         a.URL,
		Mode:        mode,
		Status:      resp.StatusCode,
		ContentType: contentType,
		Headers:     respHeaders,
		DurationMS:  durationMS,
	}

	// ── Content processing ────────────────────────────────────────────────────

	if !isHTMLContentType(contentType) {
		// Non-HTML (JSON, plain text, etc.): return as-is, apply search + pagination.
		content := rawBody
		var err error
		if a.Search != "" {
			content, err = applySearchFilter(content, a.Search)
			if err != nil {
				res.Error = fmt.Sprintf("invalid search pattern: %s", err.Error())
				return fetchResult(res)
			}
		}
		res.TotalChars = len(content)
		res.WordCount = countWords(content)
		res.Content, res.Truncated = paginateContent(content, outputOffset, outputLimit)
		return fetchResult(res)
	}

	// HTML: parse URL for resolving relative links.
	parsedURL, _ := nurl.Parse(a.URL)
	if parsedURL == nil {
		parsedURL = &nurl.URL{}
	}

	switch mode {
	case "article":
		res = processArticleMode(res, rawBody, parsedURL, a.Search)
		res.TotalChars = len(res.Content)
		res.Content, res.Truncated = paginateContent(res.Content, outputOffset, outputLimit)

	case "full":
		md, convErr := htmltomarkdown.ConvertString(rawBody)
		if convErr != nil {
			res.Content = rawBody
			res.Note = "HTML-to-Markdown conversion failed; returning raw HTML."
		} else {
			content := strings.TrimSpace(md)
			if a.Search != "" {
				content, err = applySearchFilter(content, a.Search)
				if err != nil {
					res.Error = fmt.Sprintf("invalid search pattern: %s", err.Error())
					return fetchResult(res)
				}
			}
			res.Content = content
		}
		res.WordCount = countWords(res.Content)
		res.TotalChars = len(res.Content)
		res.Content, res.Truncated = paginateContent(res.Content, outputOffset, outputLimit)

	case "links":
		allLinks := extractLinks(rawBody, parsedURL)
		res.Links, res.Truncated = paginateLinks(allLinks, outputOffset, outputLimit)
		res.WordCount = len(allLinks) // total link count across all pages
		res.TotalChars = len(allLinks)
	}

	return fetchResult(res)
}

// headPreflight does a cheap HEAD request and returns a non-empty error string if the
// content type is binary. An empty string means "proceed with the full GET".
// Preflight failures (network errors, 405 Method Not Allowed) are silently ignored —
// the main GET will catch any real problems.
func headPreflight(ctx context.Context, url string, extraHeaders map[string]string) string {
	req, err := http.NewRequestWithContext(ctx, "HEAD", url, nil)
	if err != nil {
		return "" // can't build HEAD request — let GET proceed
	}
	req.Header.Set("User-Agent", "auxot-tools/1.0 (web_fetch)")
	for k, v := range extraHeaders {
		req.Header.Set(k, v)
	}

	resp, err := sharedClient.Do(req)
	if err != nil {
		return "" // HEAD failed — let GET proceed
	}
	resp.Body.Close()

	// 405 or other method-not-allowed: server doesn't support HEAD, let GET proceed.
	if resp.StatusCode == http.StatusMethodNotAllowed || resp.StatusCode == http.StatusNotImplemented {
		return ""
	}

	ct := resp.Header.Get("Content-Type")
	if ct != "" && !isAllowedContentType(ct) {
		return fmt.Sprintf(
			"preflight HEAD refused: content type %q is binary or unsupported. "+
				"Only text/html, application/json, and other text-based types are fetched.",
			ct,
		)
	}

	return ""
}

// paginateContent applies character-based offset and limit to a string.
// Returns the sliced content and a descriptive truncation hint (empty if not truncated).
func paginateContent(content string, offset, limit int) (string, string) {
	total := len(content)

	if offset >= total {
		if total == 0 {
			return "", ""
		}
		return "", fmt.Sprintf(
			"offset %d is past end of content (%d chars total). Use offset=0 to start from the beginning.",
			offset, total,
		)
	}

	content = content[offset:]
	if len(content) <= limit {
		return content, "" // fits within limit — no truncation
	}

	// Truncate at limit, but try to break at a newline within the last 200 chars
	// so we don't cut a Markdown line in half.
	cutAt := limit
	if nl := strings.LastIndexByte(content[:limit], '\n'); nl >= 0 && nl > limit-200 {
		cutAt = nl + 1
	}
	content = content[:cutAt]

	end := offset + cutAt
	hint := fmt.Sprintf(
		"output truncated to characters %d–%d of %d total. "+
			"Retry with offset=%d to continue reading.",
		offset, end-1, total, end,
	)
	return content, hint
}

// paginateLinks applies index-based offset and limit to a link slice.
func paginateLinks(links []webFetchLink, offset, limit int) ([]webFetchLink, string) {
	if limit <= 0 || limit > maxLinksLimit {
		limit = defaultLinksLimit
	}
	total := len(links)

	if offset >= total {
		if total == 0 {
			return nil, ""
		}
		return nil, fmt.Sprintf(
			"offset %d is past end of link list (%d links total). Use offset=0 to start from the beginning.",
			offset, total,
		)
	}

	links = links[offset:]
	if len(links) <= limit {
		return links, ""
	}

	links = links[:limit]
	end := offset + limit
	hint := fmt.Sprintf(
		"showing links %d–%d of %d total. Retry with offset=%d to see more.",
		offset, end-1, total, end,
	)
	return links, hint
}

// processArticleMode runs readability on the HTML, converts the article to
// Markdown, and populates the result. Falls back to full mode if readability
// yields too little content. Does NOT apply output truncation — caller handles that.
func processArticleMode(res webFetchResult, rawHTML string, pageURL *nurl.URL, search string) webFetchResult {
	article, err := readability.FromReader(strings.NewReader(rawHTML), pageURL)

	var markdown string
	if err != nil || strings.TrimSpace(article.TextContent) == "" {
		// Readability failed — fall back to full HTML → Markdown.
		md, convErr := htmltomarkdown.ConvertString(rawHTML)
		if convErr != nil {
			res.Content = rawHTML
			res.Note = "Readability extraction and HTML-to-Markdown conversion both failed; returning raw HTML. Consider mode='links'."
			return res
		}
		markdown = strings.TrimSpace(md)
		res.Note = "Readability could not extract article content; returning full page as Markdown. Consider mode='full' or mode='links'."
	} else {
		res.Title = article.Title
		md, convErr := htmltomarkdown.ConvertString(article.Content)
		if convErr != nil {
			markdown = strings.TrimSpace(article.TextContent)
		} else {
			markdown = strings.TrimSpace(md)
		}
	}

	if search != "" && markdown != "" {
		filtered, err := applySearchFilter(markdown, search)
		if err != nil {
			res.Error = fmt.Sprintf("invalid search pattern: %s", err.Error())
			return res
		}
		markdown = filtered
	}

	res.Content = markdown
	res.WordCount = countWords(markdown)

	if res.Note == "" && res.WordCount < 100 {
		res.Note = fmt.Sprintf(
			"Low content detected (%d words). The page may be dynamic or require login. Consider retrying with mode='full' or mode='links'.",
			res.WordCount,
		)
	}

	return res
}

// extractLinks parses HTML and returns all unique, non-fragment links with their text.
func extractLinks(rawHTML string, baseURL *nurl.URL) []webFetchLink {
	doc, err := html.Parse(strings.NewReader(rawHTML))
	if err != nil {
		return nil
	}

	var links []webFetchLink
	seen := map[string]bool{}

	var walk func(*html.Node)
	walk = func(n *html.Node) {
		if n.Type == html.ElementNode && n.Data == "a" {
			var href string
			for _, attr := range n.Attr {
				if attr.Key == "href" {
					href = strings.TrimSpace(attr.Val)
					break
				}
			}
			if href != "" &&
				!strings.HasPrefix(href, "#") &&
				!strings.HasPrefix(href, "javascript:") &&
				!strings.HasPrefix(href, "mailto:") {
				if parsed, err := nurl.Parse(href); err == nil && baseURL != nil {
					href = baseURL.ResolveReference(parsed).String()
				}
				if !seen[href] {
					seen[href] = true
					text := strings.TrimSpace(nodeText(n))
					if text == "" {
						text = href
					}
					links = append(links, webFetchLink{URL: href, Text: text})
				}
			}
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(doc)
	return links
}

// nodeText returns the concatenated text content of all descendant text nodes.
func nodeText(n *html.Node) string {
	var sb strings.Builder
	var walk func(*html.Node)
	walk = func(n *html.Node) {
		if n.Type == html.TextNode {
			sb.WriteString(n.Data)
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(n)
	return sb.String()
}

// countWords returns the number of whitespace-separated words in s.
func countWords(s string) int {
	return len(strings.Fields(s))
}

// resolveBody converts the raw body argument into an io.Reader and optional auto Content-Type.
//
// The LLM may pass body as:
//   - absent / null           → no body
//   - a JSON string           → treat the unquoted value as the literal body text
//   - a JSON object or array  → compact-serialise it; return "application/json" as Content-Type
//
// Returning autoContentType="" means "don't set Content-Type automatically".
func resolveBody(raw json.RawMessage) (io.Reader, string) {
	if len(raw) == 0 {
		return nil, ""
	}

	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 {
		return nil, ""
	}

	switch trimmed[0] {
	case '"':
		// JSON string — unquote it and use the string value as-is.
		var s string
		if err := json.Unmarshal(trimmed, &s); err != nil || s == "" {
			return nil, ""
		}
		return strings.NewReader(s), ""

	case '{', '[':
		// JSON object or array — compact-encode it as the request body.
		// This lets the LLM pass body: {"key": "value"} without having to
		// manually JSON.stringify the payload first.
		var compact bytes.Buffer
		if err := json.Compact(&compact, trimmed); err != nil {
			// Malformed JSON — try to use the raw bytes anyway.
			return bytes.NewReader(trimmed), "application/json"
		}
		return &compact, "application/json"

	default:
		// Naked number, boolean, or other JSON scalar — use raw bytes as body.
		return bytes.NewReader(trimmed), ""
	}
}

// isHTMLContentType returns true if the content type is HTML or XHTML.
func isHTMLContentType(ct string) bool {
	ct = strings.ToLower(ct)
	return strings.HasPrefix(ct, "text/html") ||
		strings.HasPrefix(ct, "application/xhtml")
}

// fetchResult marshals a webFetchResult into a Result.
func fetchResult(r webFetchResult) (Result, error) {
	out, err := json.Marshal(r)
	if err != nil {
		return Result{}, fmt.Errorf("marshaling web_fetch result: %w", err)
	}
	return Result{Output: out}, nil
}

// isAllowedContentType returns true if the content type is text-based.
func isAllowedContentType(ct string) bool {
	ct = strings.ToLower(ct)
	for _, allowed := range allowedContentTypes {
		if strings.HasPrefix(ct, allowed) {
			return true
		}
	}
	return false
}

// applySearchFilter keeps only lines matching the regex, plus 3 lines of context.
func applySearchFilter(body, pattern string) (string, error) {
	re, err := regexp.Compile(pattern)
	if err != nil {
		return "", err
	}

	lines := strings.Split(body, "\n")
	contextLines := 3

	keep := make([]bool, len(lines))
	for i, line := range lines {
		if re.MatchString(line) {
			start := max(0, i-contextLines)
			end := min(len(lines)-1, i+contextLines)
			for j := start; j <= end; j++ {
				keep[j] = true
			}
		}
	}

	var out []string
	prevKept := false
	for i, line := range lines {
		if keep[i] {
			if !prevKept && len(out) > 0 {
				out = append(out, "---")
			}
			out = append(out, line)
			prevKept = true
		} else {
			prevKept = false
		}
	}

	return strings.Join(out, "\n"), nil
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
