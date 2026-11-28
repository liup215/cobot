package websearch

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// exaDefaultEndpoint is Exa's PUBLIC MCP endpoint. It needs no API key, which
// is why the default chain can work with zero configuration — the same reason
// pi treats it as its own out-of-the-box provider.
//
// We use the basic tool (web_search_exa) and NOT web_search_advanced_exa: the
// public endpoint exposes only web_search_exa and web_fetch_exa (verified
// against tools/list). The advanced tool is what accepts structured filters, so
// recency and domain filters are expressed in the QUERY TEXT instead — exactly
// what pi's buildMcpQuery does for the same reason.
const exaDefaultEndpoint = "https://mcp.exa.ai/mcp"

const exaToolName = "web_search_exa"

// ExaProvider searches through Exa's public MCP server.
type ExaProvider struct {
	endpoint string
	http     *http.Client
}

// ProviderOption configures a single provider (endpoint + HTTP client).
type ProviderOption func(*providerConfig)

type providerConfig struct {
	endpoint string
	http     *http.Client
}

// WithProviderEndpoint points a provider at a different endpoint (tests).
func WithProviderEndpoint(endpoint string) ProviderOption {
	return func(c *providerConfig) { c.endpoint = endpoint }
}

// WithProviderHTTP overrides a provider's HTTP client (tests, proxies).
func WithProviderHTTP(h *http.Client) ProviderOption {
	return func(c *providerConfig) { c.http = h }
}

func newProviderConfig(opts []ProviderOption, endpoint string) providerConfig {
	cfg := providerConfig{endpoint: endpoint}
	for _, opt := range opts {
		opt(&cfg)
	}
	if cfg.http == nil {
		cfg.http = &http.Client{Timeout: requestTimeoutFromEnv()}
	}
	return cfg
}

// NewExaProvider builds the zero-key Exa provider.
func NewExaProvider(opts ...ProviderOption) *ExaProvider {
	cfg := newProviderConfig(opts, exaDefaultEndpoint)
	return &ExaProvider{endpoint: cfg.endpoint, http: cfg.http}
}

// Name implements Provider.
func (p *ExaProvider) Name() string { return "exa" }

// Available implements Provider. Exa's public endpoint always is.
func (p *ExaProvider) Available() bool { return p.endpoint != "" }

// rpcEnvelope is the JSON-RPC response shape of the MCP endpoint. The reply
// arrives as an SSE stream, one `data:` payload per event.
type rpcEnvelope struct {
	Result *struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
		IsError bool `json:"isError"`
	} `json:"result"`
	Error *struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

// Search implements Provider.
func (p *ExaProvider) Search(ctx context.Context, q Query) ([]Result, error) {
	body, err := json.Marshal(map[string]any{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  "tools/call",
		"params": map[string]any{
			"name": exaToolName,
			"arguments": map[string]any{
				"query":      exaQuery(q),
				"numResults": q.NumResults,
			},
		},
	})
	if err != nil {
		return nil, fmt.Errorf("exa: encode request: %w", err)
	}

	endpoint := p.endpoint + "?tools=" + url.QueryEscape(exaToolName)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("exa: build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	// Provenance marker, as pi sends its own.
	req.Header.Set("x-exa-source", "cobot")

	resp, err := p.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("exa: %w", err)
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if err != nil {
		return nil, fmt.Errorf("exa: read response: %w", err)
	}

	if resp.StatusCode == http.StatusTooManyRequests {
		return nil, &RateLimitError{
			Provider:   "exa",
			RetryAfter: retryAfter(resp.Header.Get("Retry-After")),
			Detail:     snippetOf(raw, 200),
		}
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("exa: HTTP %d: %s", resp.StatusCode, snippetOf(raw, 300))
	}

	text, err := exaText(raw)
	if err != nil {
		return nil, err
	}
	return parseExaResults(text), nil
}

// exaText digs the tool's text payload out of the SSE-wrapped JSON-RPC reply.
func exaText(raw []byte) (string, error) {
	payload := ssePayload(raw)
	var env rpcEnvelope
	if err := json.Unmarshal(payload, &env); err != nil {
		return "", fmt.Errorf("exa: unreadable reply: %s", snippetOf(raw, 200))
	}
	if env.Error != nil {
		return "", fmt.Errorf("exa: %s", env.Error.Message)
	}
	if env.Result == nil {
		return "", fmt.Errorf("exa: reply carried no result: %s", snippetOf(raw, 200))
	}

	var b strings.Builder
	for _, part := range env.Result.Content {
		if part.Type == "text" && part.Text != "" {
			b.WriteString(part.Text)
		}
	}
	text := b.String()
	if env.Result.IsError {
		if text == "" {
			text = "tool reported an error"
		}
		return "", fmt.Errorf("exa: %s", strings.TrimSpace(text))
	}
	return text, nil
}

// ssePayload returns the last `data:` event payload, or the body itself when
// the server answered with plain JSON.
func ssePayload(raw []byte) []byte {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 {
		return trimmed
	}
	if trimmed[0] == '{' {
		return trimmed
	}
	var last []byte
	for _, line := range bytes.Split(raw, []byte("\n")) {
		line = bytes.TrimRight(line, "\r")
		if !bytes.HasPrefix(line, []byte("data:")) {
			continue
		}
		payload := bytes.TrimSpace(line[len("data:"):])
		if len(payload) == 0 || bytes.Equal(payload, []byte("[DONE]")) {
			continue
		}
		last = payload
	}
	if last == nil {
		return trimmed
	}
	return last
}

var (
	exaTitleLine = regexp.MustCompile(`(?m)^Title: `)
	exaURLNode   = regexp.MustCompile(`(?m)^URL: (.+)$`)
	exaTitleNode = regexp.MustCompile(`(?m)^Title: (.+)$`)
	exaHighligh  = regexp.MustCompile(`\nHighlights:\s*\n`)
	exaTextNode  = regexp.MustCompile(`\nText: `)
)

// parseExaResults turns the tool text into hits.
//
// The endpoint answers in one of two shapes and BOTH must work: the documented
// human-readable block form ("Title: … / URL: … / Highlights: …" repeated per
// hit), and a JSON document some deployments return. Block form is what the
// public endpoint currently sends.
func parseExaResults(text string) []Result {
	if results, ok := parseExaJSON(text); ok {
		return results
	}

	blocks := exaTitleLine.FindAllStringIndex(text, -1)
	results := make([]Result, 0, len(blocks))
	for i, span := range blocks {
		end := len(text)
		if i+1 < len(blocks) {
			end = blocks[i+1][0]
		}
		block := strings.TrimSpace(text[span[0]:end])
		if block == "" {
			continue
		}
		result := parseExaBlock(block)
		if result.URL == "" {
			continue
		}
		if result.Title == "" {
			result.Title = hostLabel(result.URL)
		}
		results = append(results, result)
	}
	return results
}

func parseExaBlock(block string) Result {
	result := Result{}
	if m := exaTitleNode.FindStringSubmatch(block); m != nil {
		result.Title = strings.TrimSpace(m[1])
	}
	if m := exaURLNode.FindStringSubmatch(block); m != nil {
		result.URL = strings.TrimSpace(m[1])
	}

	// Body starts after the Highlights/Text marker; metadata in the header
	// (Published/Author) must never be mistaken for page text, so the split
	// point is kept.
	cut := -1
	body := ""
	if loc := exaHighligh.FindStringIndex(block); loc != nil {
		cut = loc[1]
	} else if loc := exaTextNode.FindStringIndex(block); loc != nil {
		cut = loc[1]
	}
	if cut >= 0 {
		body = block[cut:]
	}
	body = strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(body), "---"))
	result.Snippet = body
	return result
}

// parseExaJSON reads the JSON variant: {"results":[{"title","url","text","highlights"}]}.
func parseExaJSON(text string) ([]Result, bool) {
	trimmed := strings.TrimSpace(text)
	if trimmed == "" || (trimmed[0] != '{' && trimmed[0] != '[') {
		return nil, false
	}
	var doc struct {
		Results []struct {
			Title      string   `json:"title"`
			URL        string   `json:"url"`
			Text       string   `json:"text"`
			Highlights []string `json:"highlights"`
		} `json:"results"`
	}
	if err := json.Unmarshal([]byte(trimmed), &doc); err != nil {
		return nil, false
	}
	if len(doc.Results) == 0 {
		return nil, true
	}
	results := make([]Result, 0, len(doc.Results))
	for _, item := range doc.Results {
		if item.URL == "" {
			continue
		}
		snippet := strings.Join(item.Highlights, " ")
		if strings.TrimSpace(snippet) == "" {
			snippet = item.Text
		}
		title := strings.TrimSpace(item.Title)
		if title == "" {
			title = hostLabel(item.URL)
		}
		results = append(results, Result{Title: title, URL: item.URL, Snippet: snippet})
	}
	return results, true
}

// exaQuery composes the text query. Exa's public endpoint ignores structured
// filters, so domain and recency constraints ride along as query terms —
// "site:host", "-site:host", "past week". This is pi's buildMcpQuery, kept
// deliberately: dropping the filters would silently widen the search.
func exaQuery(q Query) string {
	parts := []string{q.Text}
	for _, domain := range q.Domains {
		if strings.HasPrefix(domain, "-") {
			parts = append(parts, "-site:"+strings.TrimPrefix(domain, "-"))
			continue
		}
		parts = append(parts, "site:"+domain)
	}
	switch q.Recency {
	case RecencyDay:
		parts = append(parts, "past 24 hours")
	case RecencyWeek:
		parts = append(parts, "past week")
	case RecencyMonth:
		parts = append(parts, time.Now().Format("January 2006"))
	case RecencyYear:
		parts = append(parts, strconv.Itoa(time.Now().Year()))
	}
	return strings.Join(parts, " ")
}

// retryAfter parses a Retry-After header (seconds or HTTP date).
func retryAfter(value string) time.Duration {
	value = strings.TrimSpace(value)
	if value == "" {
		return 0
	}
	if seconds, err := strconv.Atoi(value); err == nil {
		if seconds <= 0 {
			return 0
		}
		return time.Duration(seconds) * time.Second
	}
	if when, err := http.ParseTime(value); err == nil {
		if d := time.Until(when); d > 0 {
			return d
		}
	}
	return 0
}

// snippetOf renders a bounded one-line excerpt of an error body.
func snippetOf(raw []byte, max int) string {
	return truncateRunesOrBytes(collapse(string(raw)), max)
}

// hostLabel is the fallback title for a hit the provider did not name.
func hostLabel(rawURL string) string {
	parsed, err := url.Parse(rawURL)
	if err != nil || parsed.Host == "" {
		return rawURL
	}
	return parsed.Host
}
