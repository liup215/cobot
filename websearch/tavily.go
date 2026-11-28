package websearch

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// tavilyDefaultEndpoint is Tavily's search API. Unlike Exa's public MCP it
// requires a key, so this provider is only in the chain when one is configured
// — availability is a property of the provider, not the caller's problem.
const tavilyDefaultEndpoint = "https://api.tavily.com/search"

// TavilyProvider searches Tavily's API. It exists as the second link in the
// chain for two reasons: its results are page-grounded snippets (closer to what
// a study agent quotes than a title list), and it accepts STRUCTURED recency and
// domain filters instead of encoding them in the query text.
type TavilyProvider struct {
	endpoint string
	key      string
	http     *http.Client
}

// NewTavilyProvider builds a keyed Tavily provider. An empty key yields a
// provider that reports itself unavailable, so a zero-config deployment simply
// does not include it.
func NewTavilyProvider(key string, opts ...ProviderOption) *TavilyProvider {
	cfg := newProviderConfig(opts, tavilyDefaultEndpoint)
	return &TavilyProvider{endpoint: cfg.endpoint, key: strings.TrimSpace(key), http: cfg.http}
}

// Name implements Provider.
func (p *TavilyProvider) Name() string { return "tavily" }

// Available implements Provider.
func (p *TavilyProvider) Available() bool { return p.key != "" }

// Search implements Provider.
func (p *TavilyProvider) Search(ctx context.Context, q Query) ([]Result, error) {
	if p.key == "" {
		return nil, fmt.Errorf("tavily: no API key (set %s)", EnvTavilyKey)
	}

	payload := map[string]any{
		"query":        q.Text,
		"max_results":  q.NumResults,
		"search_depth": "basic",
		"topic":        "general",
	}
	include, exclude := splitDomains(q.Domains)
	if len(include) > 0 {
		payload["include_domains"] = include
	}
	if len(exclude) > 0 {
		payload["exclude_domains"] = exclude
	}
	if q.Recency != "" {
		payload["time_range"] = q.Recency
	}

	body, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("tavily: encode request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("tavily: build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+p.key)

	resp, err := p.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("tavily: %w", err)
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if err != nil {
		return nil, fmt.Errorf("tavily: read response: %w", err)
	}

	switch {
	case resp.StatusCode == http.StatusTooManyRequests:
		return nil, &RateLimitError{
			Provider:   "tavily",
			RetryAfter: retryAfter(resp.Header.Get("Retry-After")),
			Detail:     snippetOf(raw, 200),
		}
	case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
		return nil, fmt.Errorf("tavily: key rejected (HTTP %d): %s", resp.StatusCode, snippetOf(raw, 200))
	case resp.StatusCode != http.StatusOK:
		return nil, fmt.Errorf("tavily: HTTP %d: %s", resp.StatusCode, snippetOf(raw, 300))
	}

	var doc struct {
		Results []struct {
			Title   string `json:"title"`
			URL     string `json:"url"`
			Content string `json:"content"`
		} `json:"results"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, fmt.Errorf("tavily: unreadable reply: %s", snippetOf(raw, 200))
	}

	results := make([]Result, 0, len(doc.Results))
	for _, item := range doc.Results {
		if item.URL == "" {
			continue
		}
		title := strings.TrimSpace(item.Title)
		if title == "" {
			title = hostLabel(item.URL)
		}
		results = append(results, Result{Title: title, URL: item.URL, Snippet: item.Content})
	}
	return results, nil
}

// splitDomains separates pi-notation entries ("-host" = exclude) into the two
// lists Tavily takes as structured parameters.
func splitDomains(domains []string) (include, exclude []string) {
	for _, domain := range domains {
		if strings.HasPrefix(domain, "-") {
			exclude = append(exclude, strings.TrimPrefix(domain, "-"))
			continue
		}
		include = append(include, domain)
	}
	return include, exclude
}
