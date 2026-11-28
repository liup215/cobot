// Package websearch is cobot's web capability layer: an ordered provider
// chain for web search, plus the result store that keeps a search answer
// bounded.
//
// Design — ported from pi's pi-web-access, the shape this project's agents
// already use (see the "Why" notes; every one of these is a decision, not a
// detail):
//
//   - Every tool response is BOUNDED. A search returns at most BudgetRunes of
//     titles, URLs and snippets; the FULL provider text is stored under a
//     responseId and read back in pages (or located with findText) through the
//     get_search_content tool. Bounding the inline answer is the point: Exa
//     returns kilobytes of page text per hit, and four queries of that inside
//     one tool result is how a context window dies.
//   - Multiple queries fan out (up to MaxQueries, MaxConcurrency at a time) and
//     each query fails INDEPENDENTLY: one provider error never discards another
//     query's results, and the error text names what was tried.
//   - Providers are an interface behind an automatic chain. Per query, the
//     first provider that answers wins. A provider whose key is missing is
//     SKIPPED, never fatal — that is what makes zero configuration work.
//   - Zero configuration works: the default chain starts with Exa's public MCP
//     endpoint, which needs no key (this mirrors pi's own default chain).
//     Adding TAVILY_API_KEY (or COBOT_TAVILY_API_KEY) puts Tavily behind it.
//
// Environment (all optional):
//
//	COBOT_WEB_PROVIDER   ordered provider list, e.g. "tavily,exa" (default "exa,tavily")
//	COBOT_TAVILY_API_KEY Tavily key (TAVILY_API_KEY is accepted as a fallback)
//	COBOT_WEB_TIMEOUT    per-request timeout, Go duration (default 20s)
//
// Deliberately NOT here (the same omissions pi makes for its own reasons, and
// ones this project makes for its own):
//   - No Bing/Google HTML scraping. Bing's RSS output carries an explicit
//     "personal, non-commercial, RSS-aggregator-only" licence, and a product
//     must not ship it as a general search backend.
//   - No page fetching. fetch_content needs readability extraction, an SSRF
//     gate and a 5MB stream bound; that is a second capability layer, not a
//     flag on this one.
//   - No browser-cookie auth, no hosted unlockers, no per-call proxy chain.
package websearch

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"
)

// Environment variables read by Default. Named here so callers and tests do
// not re-spell them.
const (
	EnvProvider  = "COBOT_WEB_PROVIDER"
	EnvTavilyKey = "COBOT_TAVILY_API_KEY"
	// EnvTimeout is the per-request HTTP timeout, Go duration syntax.
	EnvTimeout = "COBOT_WEB_TIMEOUT"
)

// Client is one configured search stack: an ordered provider chain and the
// store shared with the get_search_content tool.
type Client struct {
	providers []Provider
	store     *Store

	maxQueries        int
	maxResults        int
	snippetRunes      int
	budgetRunes       int
	concurrency       int
	fetchExcerptRunes int

	// guard + fetchHTTP are the fetch_content path: a private-network gate and
	// the client that enforces it. Built once, in NewClient, so every fetch shares
	// the same policy (and the same connection pool).
	guard     FetchGuardOptions
	fetchHTTP *http.Client
}

// ClientOption configures a Client.
type ClientOption func(*Client)

// WithStore overrides the result store. Both web tools must share one store —
// the search writes the responseId the content tool reads.
func WithStore(s *Store) ClientOption {
	return func(c *Client) {
		if s != nil {
			c.store = s
		}
	}
}

// WithProviders replaces the chain. Providers are tried in order.
func WithProviders(p ...Provider) ClientOption {
	return func(c *Client) {
		c.providers = p
	}
}

// WithFetchGuard tunes the private-address gate used by fetch_content. The zero
// value blocks loopback and every private range; AllowLoopback exists for tests
// and self-hosted setups.
func WithFetchGuard(opts FetchGuardOptions) ClientOption {
	return func(c *Client) {
		c.guard = opts
	}
}

// NewClient builds a client. With no options it mirrors Default: Exa first
// (zero key), Tavily behind it when a key is present.
func NewClient(opts ...ClientOption) *Client {
	c := &Client{
		providers:         defaultProviders(),
		store:             NewStore(),
		maxQueries:        MaxQueries,
		maxResults:        MaxNumResults,
		snippetRunes:      SnippetRunes,
		budgetRunes:       BudgetRunes,
		concurrency:       MaxConcurrency,
		fetchExcerptRunes: ExcerptRunes,
	}
	for _, opt := range opts {
		opt(c)
	}
	// After the options: the guard they may have changed decides the client's
	// dialer policy, and rebuilding it later would leave an unguarded client
	// reachable from a goroutine.
	c.fetchHTTP = newFetchHTTPClient(c.guard, FetchTimeout, false)
	return c
}

// fetchClient is the guarded HTTP client used by fetch_content.
func (c *Client) fetchClient() *http.Client { return c.fetchHTTP }

// Store exposes the result store so the get_search_content tool can read what
// a previous search wrote.
func (c *Client) Store() *Store { return c.store }

// Providers returns the chain's names, in order.
func (c *Client) Providers() []string {
	names := make([]string, 0, len(c.providers))
	for _, p := range c.providers {
		names = append(names, p.Name())
	}
	return names
}

var (
	defaultClient *Client
	defaultOnce   sync.Once
)

// Default returns the process-wide client. One instance means one store, which
// is what lets a search's responseId be read by the content tool constructed
// separately.
func Default() *Client {
	defaultOnce.Do(func() { defaultClient = NewClient() })
	return defaultClient
}

func requestTimeoutFromEnv() time.Duration {
	if raw := strings.TrimSpace(os.Getenv(EnvTimeout)); raw != "" {
		if d, err := time.ParseDuration(raw); err == nil && d > 0 {
			return d
		}
	}
	return DefaultTimeout
}

// defaultProviders builds the chain from the environment: the names in
// COBOT_WEB_PROVIDER that are AVAILABLE (a missing key skips, never fails),
// falling back to "exa,tavily" when the variable is unset or names nothing
// usable.
func defaultProviders() []Provider {
	known := map[string]func() Provider{
		"exa":    func() Provider { return NewExaProvider() },
		"tavily": func() Provider { return NewTavilyProvider(tavilyKey()) },
	}

	order := []string{"exa", "tavily"}
	if raw := strings.TrimSpace(os.Getenv(EnvProvider)); raw != "" {
		order = nil
		for _, name := range strings.Split(raw, ",") {
			if name = strings.ToLower(strings.TrimSpace(name)); name != "" {
				order = append(order, name)
			}
		}
	}

	providers := make([]Provider, 0, len(order))
	for _, name := range order {
		build, ok := known[name]
		if !ok {
			continue // unknown name: ignore rather than break every search
		}
		if p := build(); p.Available() {
			providers = append(providers, p)
		}
	}
	if len(providers) == 0 {
		// Exa needs no key, so this only happens when someone configured a
		// chain of key-gated providers without keys. Keep the tool usable.
		providers = append(providers, NewExaProvider())
	}
	return providers
}

func tavilyKey() string {
	if key := strings.TrimSpace(os.Getenv(EnvTavilyKey)); key != "" {
		return key
	}
	return strings.TrimSpace(os.Getenv("TAVILY_API_KEY"))
}

// SearchRequest is the tool-facing request, one-to-one with the JSON schema.
type SearchRequest struct {
	Query      string   `json:"query"`
	Queries    []string `json:"queries"`
	NumResults int      `json:"numResults"`
	Recency    string   `json:"recencyFilter"`
	Domains    []string `json:"domainFilter"`
	Provider   string   `json:"provider"`
}

// Search runs the request and returns a bounded, model-facing response. The
// full text of every hit is stored under the returned responseId.
func (c *Client) Search(ctx context.Context, req SearchRequest) (*SearchResponse, error) {
	queries, err := c.normalize(req)
	if err != nil {
		return nil, err
	}
	chain, err := c.chainFor(req.Provider)
	if err != nil {
		return nil, err
	}

	outcomes := make([]QueryOutcome, len(queries))
	sem := make(chan struct{}, c.concurrency)
	var wg sync.WaitGroup
	for i := range queries {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			select {
			case sem <- struct{}{}:
				defer func() { <-sem }()
			case <-ctx.Done():
				outcomes[i] = QueryOutcome{Query: queries[i].Text, Error: ctx.Err().Error()}
				return
			}
			outcomes[i] = c.searchOne(ctx, queries[i], chain)
		}(i)
	}
	wg.Wait()

	// Context cancellation is a real failure, not a partial answer: report it
	// as an error so the host can surface "stopped" instead of empty results.
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	// numResults is enforced ONCE, before storing and bounding: two separate
	// clamps drifted apart (the store kept 2 hits while the inline answer showed
	// 5) and the model then read a "capped" answer that was not capped.
	outcomes, dropped := capResults(queries, outcomes)
	id := c.storeSearch(queries, outcomes)
	bounded, omittedInline := c.bound(outcomes)

	return &SearchResponse{
		ResponseID:     id,
		Providers:      usedProviders(bounded),
		Queries:        bounded,
		OmittedResults: omittedInline,
		Hint:           hint(id, dropped, omittedInline, len(bounded)),
	}, nil
}

// capResults trims each query to its numResults and reports how many hits were
// discarded, so the response can say so instead of quietly losing them.
func capResults(queries []Query, outcomes []QueryOutcome) ([]QueryOutcome, int) {
	dropped := 0
	for i := range outcomes {
		limit := queries[i].NumResults
		if limit <= 0 || len(outcomes[i].Results) <= limit {
			continue
		}
		dropped += len(outcomes[i].Results) - limit
		outcomes[i].Results = outcomes[i].Results[:limit]
	}
	return outcomes, dropped
}

// storeSearch keeps the untruncated provider text and returns the responseId.
func (c *Client) storeSearch(queries []Query, outcomes []QueryOutcome) string {
	stored := make([]StoredQuery, 0, len(outcomes))
	for i, outcome := range outcomes {
		results := make([]Result, 0, len(outcome.Results))
		for _, r := range outcome.Results {
			snippet := collapse(r.Snippet)
			if truncated, ok := truncateRunes(snippet, MaxStoredSnippetRunes); ok {
				snippet = truncated + " …"
			}
			results = append(results, Result{Title: r.Title, URL: r.URL, Snippet: snippet})
		}
		stored = append(stored, StoredQuery{
			Query:    queries[i].Text,
			Provider: outcome.Provider,
			Error:    outcome.Error,
			Results:  results,
		})
	}
	return c.store.Put(KindSearch, stored)
}

// bound trims the inline answer to the rune budget, in document order, so the
// model sees whole hits and a count of what it cannot see right now.
func (c *Client) bound(outcomes []QueryOutcome) ([]QueryOutcome, int) {
	bounded := make([]QueryOutcome, 0, len(outcomes))
	used, omitted := 0, 0
	for _, outcome := range outcomes {
		out := QueryOutcome{Query: outcome.Query, Provider: outcome.Provider, Error: outcome.Error}
		for _, r := range outcome.Results {
			snippet, cut := truncateRunes(collapse(r.Snippet), c.snippetRunes)
			if cut {
				snippet += " …"
			}
			cost := len([]rune(r.Title)) + len(r.URL) + len([]rune(snippet)) + 8
			if used+cost > c.budgetRunes {
				omitted++
				continue
			}
			used += cost
			out.Results = append(out.Results, Result{Title: r.Title, URL: r.URL, Snippet: snippet})
		}
		bounded = append(bounded, out)
	}
	return bounded, omitted
}

func usedProviders(outcomes []QueryOutcome) []string {
	seen := make(map[string]bool, 2)
	names := make([]string, 0, 2)
	for _, o := range outcomes {
		if o.Provider == "" || seen[o.Provider] {
			continue
		}
		seen[o.Provider] = true
		names = append(names, o.Provider)
	}
	return names
}

// hint replaces pi's "guidance" block: the model is told exactly which call
// reads the rest, because a bounded answer without that sentence invites a
// re-search instead of a page turn.
func hint(id string, droppedByNumResults, droppedByBudget, queryCount int) string {
	var b strings.Builder
	fmt.Fprintf(&b, `Full results are stored as responseId %q (%d quer`, id, queryCount)
	if queryCount == 1 {
		b.WriteString("y")
	} else {
		b.WriteString("ies")
	}
	if droppedByNumResults > 0 {
		fmt.Fprintf(&b, ", %d hit(s) beyond numResults not returned", droppedByNumResults)
	}
	if droppedByBudget > 0 {
		fmt.Fprintf(&b, ", %d hit(s) past the inline budget counted in omittedResults", droppedByBudget)
	}
	b.WriteString(")")
	fmt.Fprintf(&b, `. Read more with get_search_content({"responseId": %q, "queryIndex": 0, "limit": %d})`, id, DefaultPageRunes)
	b.WriteString(" — it returns the complete provider text for a query in pages, and findText locates passages without paging.")
	b.WriteString(" Snippets here are provider excerpts, NOT the page: to read a source in full, call fetch_content with its URL.")
	return b.String()
}

// normalize validates and de-duplicates the request.
func (c *Client) normalize(req SearchRequest) ([]Query, error) {
	raw := make([]string, 0, 1+len(req.Queries))
	if s := strings.TrimSpace(req.Query); s != "" {
		raw = append(raw, s)
	}
	for _, q := range req.Queries {
		if s := strings.TrimSpace(q); s != "" {
			raw = append(raw, s)
		}
	}
	if len(raw) == 0 {
		return nil, fmt.Errorf("a query is required (pass query or a non-empty queries list)")
	}

	seen := make(map[string]bool, len(raw))
	texts := make([]string, 0, len(raw))
	for _, q := range raw {
		key := strings.ToLower(q)
		if seen[key] {
			continue
		}
		seen[key] = true
		texts = append(texts, q)
	}
	if len(texts) > c.maxQueries {
		return nil, fmt.Errorf("too many queries: %d (max %d — merge the angles instead)", len(texts), c.maxQueries)
	}

	numResults := req.NumResults
	if numResults == 0 {
		numResults = DefaultNumResults
	}
	if numResults < 1 {
		return nil, fmt.Errorf("numResults must be at least 1")
	}
	if numResults > c.maxResults {
		numResults = c.maxResults
	}

	recency := strings.ToLower(strings.TrimSpace(req.Recency))
	switch recency {
	case "", RecencyDay, RecencyWeek, RecencyMonth, RecencyYear:
	case "any", "all", "none":
		recency = ""
	default:
		return nil, fmt.Errorf("recencyFilter must be day, week, month or year (got %q)", req.Recency)
	}

	domains, err := normalizeDomains(req.Domains)
	if err != nil {
		return nil, err
	}

	queries := make([]Query, 0, len(texts))
	for _, text := range texts {
		queries = append(queries, Query{
			Text:       text,
			NumResults: numResults,
			Recency:    recency,
			Domains:    domains,
		})
	}
	return queries, nil
}

// chainFor resolves the request's provider selector: ""/"auto" is the full
// chain, a name pins that one provider, a comma list pins several in order.
func (c *Client) chainFor(selector string) ([]Provider, error) {
	selector = strings.ToLower(strings.TrimSpace(selector))
	if selector == "" || selector == "auto" {
		return c.providers, nil
	}

	wanted := make(map[string]bool, 2)
	order := make([]string, 0, 2)
	for _, name := range strings.Split(selector, ",") {
		if name = strings.TrimSpace(name); name != "" {
			wanted[name] = true
			order = append(order, name)
		}
	}

	selected := make([]Provider, 0, len(order))
	available := make(map[string]bool, len(c.providers))
	for _, p := range c.providers {
		available[p.Name()] = true
	}
	for _, name := range order {
		if !available[name] {
			return nil, fmt.Errorf("provider %q is not available (configured: %s)", name, strings.Join(c.Providers(), ", "))
		}
	}
	for _, p := range c.providers {
		if wanted[p.Name()] {
			selected = append(selected, p)
		}
	}
	return selected, nil
}

// searchOne runs one query down the chain.
//
// A provider that is not available (missing key) is SKIPPED and named in the
// error text: silently dropping it would make "tavily: no API key" look like
// "the web has nothing", and failing the call on it would break zero-config
// deployments.
func (c *Client) searchOne(ctx context.Context, q Query, chain []Provider) QueryOutcome {
	outcome := QueryOutcome{Query: q.Text}
	attempts := make([]string, 0, len(chain))
	for _, p := range chain {
		if !p.Available() {
			attempts = append(attempts, p.Name()+": not configured, skipped")
			continue
		}
		results, err := c.searchWith(ctx, p, q)
		if err == nil {
			outcome.Provider = p.Name()
			outcome.Results = results
			return outcome
		}
		if ctx.Err() != nil {
			outcome.Error = ctx.Err().Error()
			return outcome
		}
		attempts = append(attempts, fmt.Sprintf("%s: %v", p.Name(), err))
	}
	outcome.Error = strings.Join(attempts, "; ")
	if len(attempts) == 0 {
		outcome.Error = "no search provider is configured"
	}
	return outcome
}

// searchWith calls one provider, retrying once after a 429's Retry-After.
// A long cooldown is NOT parked on: the query moves to the next provider, the
// same trade-off pi documents for its own chain.
func (c *Client) searchWith(ctx context.Context, p Provider, q Query) ([]Result, error) {
	results, err := p.Search(ctx, q)
	var rate *RateLimitError
	if !errors.As(err, &rate) {
		return results, err
	}
	wait := rate.RetryAfter
	if wait <= 0 || wait > maxRetryWait {
		return nil, err
	}
	select {
	case <-time.After(wait):
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	return p.Search(ctx, q)
}
