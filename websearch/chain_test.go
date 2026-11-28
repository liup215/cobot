package websearch

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeProvider scripts one chain link: availability, per-query behaviour and a
// call counter so retry/skip rules are asserted, not assumed.
type fakeProvider struct {
	name      string
	available bool

	mu    sync.Mutex
	calls []string

	// search answers one query. Returning a RateLimitError asks the client to
	// retry; any other error moves the query to the next provider.
	search func(ctx context.Context, q Query) ([]Result, error)
}

func (f *fakeProvider) Name() string    { return f.name }
func (f *fakeProvider) Available() bool { return f.available }

func (f *fakeProvider) Search(ctx context.Context, q Query) ([]Result, error) {
	f.mu.Lock()
	f.calls = append(f.calls, q.Text)
	f.mu.Unlock()
	if f.search == nil {
		return []Result{{Title: f.name, URL: "https://" + f.name + ".example", Snippet: "from " + f.name}}, nil
	}
	return f.search(ctx, q)
}

func (f *fakeProvider) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

func newTestClient(providers ...Provider) *Client {
	return NewClient(
		WithProviders(providers...),
		WithStore(NewStore(WithStoreLimits(8, 1_000_000))),
	)
}

// TestChainFallsThroughToNextProvider is the core promise of the chain: the
// second link answers when the first one breaks, and the answer names it.
func TestChainFallsThroughToNextProvider(t *testing.T) {
	broken := &fakeProvider{name: "broken", available: true, search: func(context.Context, Query) ([]Result, error) {
		return nil, errors.New("connection reset")
	}}
	working := &fakeProvider{name: "working", available: true}

	client := newTestClient(broken, working)
	response, err := client.Search(context.Background(), SearchRequest{Query: "cell structure"})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if got := response.Queries[0].Provider; got != "working" {
		t.Fatalf("provider = %q, want working", got)
	}
	if got := response.Queries[0].Results[0].Title; got != "working" {
		t.Errorf("result title = %q", got)
	}
	if len(response.Queries[0].Error) != 0 {
		t.Errorf("successful query kept an error: %q", response.Queries[0].Error)
	}
	if len(response.Providers) != 1 || response.Providers[0] != "working" {
		t.Errorf("providers = %v, want [working]", response.Providers)
	}
}

// TestQueryErrorsAreIsolated pins the fan-out rule: one query's failure never
// erases another query's results, and the error says which provider was tried.
func TestQueryErrorsAreIsolated(t *testing.T) {
	provider := &fakeProvider{name: "exa", available: true, search: func(_ context.Context, q Query) ([]Result, error) {
		if strings.Contains(q.Text, "bad") {
			return nil, errors.New("upstream timeout")
		}
		return []Result{{Title: "hit", URL: "https://example.org/" + q.Text, Snippet: "ok"}}, nil
	}}

	client := newTestClient(provider)
	response, err := client.Search(context.Background(), SearchRequest{Queries: []string{"good query", "bad query"}})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(response.Queries) != 2 {
		t.Fatalf("got %d queries, want 2", len(response.Queries))
	}

	good, bad := response.Queries[0], response.Queries[1]
	if len(good.Results) != 1 || good.Error != "" {
		t.Errorf("good query = %+v", good)
	}
	if bad.Error == "" || !strings.Contains(bad.Error, "exa") || !strings.Contains(bad.Error, "upstream timeout") {
		t.Errorf("bad query error = %q, want provider + cause", bad.Error)
	}
	if len(bad.Results) != 0 {
		t.Errorf("bad query returned results: %+v", bad.Results)
	}
}

// TestRateLimitRetriesOnce: a short advertised cooldown is waited out and the
// same provider is tried again, exactly once.
func TestRateLimitRetriesOnce(t *testing.T) {
	attempts := 0
	provider := &fakeProvider{name: "exa", available: true, search: func(context.Context, Query) ([]Result, error) {
		attempts++
		if attempts == 1 {
			return nil, &RateLimitError{Provider: "exa", RetryAfter: 5 * time.Millisecond}
		}
		return []Result{{Title: "second try", URL: "https://example.org/", Snippet: "ok"}}, nil
	}}

	client := newTestClient(provider)
	response, err := client.Search(context.Background(), SearchRequest{Query: "retry me"})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if attempts != 2 {
		t.Fatalf("attempts = %d, want 2", attempts)
	}
	if response.Queries[0].Results[0].Title != "second try" {
		t.Errorf("result = %+v", response.Queries[0].Results)
	}
}

// TestLongRateLimitMovesToNextProvider: a cooldown too long to sit through is
// not waited on — the chain advances instead of parking the tool call.
func TestLongRateLimitMovesToNextProvider(t *testing.T) {
	slow := &fakeProvider{name: "exa", available: true, search: func(context.Context, Query) ([]Result, error) {
		return nil, &RateLimitError{Provider: "exa", RetryAfter: time.Hour}
	}}
	second := &fakeProvider{name: "tavily", available: true}

	client := newTestClient(slow, second)
	response, err := client.Search(context.Background(), SearchRequest{Query: "x"})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if slow.callCount() != 1 {
		t.Errorf("rate-limited provider called %d times, want 1", slow.callCount())
	}
	if got := response.Queries[0].Provider; got != "tavily" {
		t.Errorf("provider = %q, want tavily", got)
	}
}

// TestUnavailableProviderIsSkippedNotFatal: a keyless deployment must still
// search, and the reason must be visible when nothing is left.
func TestUnavailableProviderIsSkippedNotFatal(t *testing.T) {
	keyless := NewTavilyProvider("")
	if keyless.Available() {
		t.Fatal("a Tavily provider without a key reported itself available")
	}

	client := newTestClient(keyless, &fakeProvider{name: "exa", available: true})
	response, err := client.Search(context.Background(), SearchRequest{Query: "x"})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if got := response.Queries[0].Provider; got != "exa" {
		t.Errorf("provider = %q, want exa", got)
	}

	// With nothing available the query fails — with an explanation, never a
	// silent empty result list.
	only := newTestClient(keyless)
	failed, err := only.Search(context.Background(), SearchRequest{Query: "x"})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if !strings.Contains(failed.Queries[0].Error, "not configured, skipped") {
		t.Errorf("error = %q, want a skip explanation", failed.Queries[0].Error)
	}
}

// TestProviderSelectorUnknownFailsBeforeAnyRequest: a typo must not look like
// "the web had nothing".
func TestProviderSelectorUnknownFailsBeforeAnyRequest(t *testing.T) {
	client := newTestClient(&fakeProvider{name: "exa", available: true})
	_, err := client.Search(context.Background(), SearchRequest{Query: "x", Provider: "gogle"})
	if err == nil || !strings.Contains(err.Error(), "not available") || !strings.Contains(err.Error(), "exa") {
		t.Fatalf("err = %v, want an error naming the configured providers", err)
	}
}

// TestProviderSelectorPinsOneProvider: "provider" must not fall back to other
// links; asking for one backend and silently getting another is worse than an
// error.
func TestProviderSelectorPinsOneProvider(t *testing.T) {
	first := &fakeProvider{name: "exa", available: true, search: func(context.Context, Query) ([]Result, error) {
		return nil, errors.New("broken")
	}}
	second := &fakeProvider{name: "tavily", available: true}

	client := newTestClient(first, second)
	response, err := client.Search(context.Background(), SearchRequest{Query: "x", Provider: "exa"})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if second.callCount() != 0 {
		t.Errorf("pinned search called tavily %d times", second.callCount())
	}
	if !strings.Contains(response.Queries[0].Error, "exa: broken") {
		t.Errorf("error = %q", response.Queries[0].Error)
	}
}

// TestNormalizeRejectsBadQueries pins the input rules the schema documents.
func TestNormalizeRejectsBadQueries(t *testing.T) {
	client := newTestClient(&fakeProvider{name: "exa", available: true})

	cases := []struct {
		name    string
		request SearchRequest
		want    string
	}{
		{"no query", SearchRequest{}, "a query is required"},
		{"blank queries", SearchRequest{Queries: []string{"  ", ""}}, "a query is required"},
		{"too many", SearchRequest{Queries: []string{"a", "b", "c", "d", "e"}}, "too many queries"},
		{"bad numResults", SearchRequest{Query: "x", NumResults: -1}, "numResults"},
		{"bad recency", SearchRequest{Query: "x", Recency: "decade"}, "recencyFilter"},
		{"empty domain", SearchRequest{Query: "x", Domains: []string{"-"}}, "domainFilter"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := client.Search(context.Background(), tc.request)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want %q", err, tc.want)
			}
		})
	}
}

// TestNormalizeDedupesAndCapsResults: the same query twice is a wasted provider
// call, and numResults above the cap is clamped rather than refused.
func TestNormalizeDedupesAndCapsResults(t *testing.T) {
	provider := &fakeProvider{name: "exa", available: true}
	client := newTestClient(provider)

	response, err := client.Search(context.Background(), SearchRequest{
		Query:      "Cell Structure",
		Queries:    []string{"cell structure", "photosynthesis"},
		NumResults: 99,
	})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(response.Queries) != 2 {
		t.Fatalf("got %d queries, want 2 (dedupe is case-insensitive)", len(response.Queries))
	}
	if provider.callCount() != 2 {
		t.Errorf("provider calls = %d, want 2", provider.callCount())
	}

	queries, err := client.normalize(SearchRequest{Query: "x", NumResults: 99})
	if err != nil {
		t.Fatalf("normalize: %v", err)
	}
	if queries[0].NumResults != MaxNumResults {
		t.Errorf("numResults = %d, want %d", queries[0].NumResults, MaxNumResults)
	}
}

// TestCancelledContextIsAnError: "the user stopped me" must not be reported as a
// successful empty search.
func TestCancelledContextIsAnError(t *testing.T) {
	provider := &fakeProvider{name: "exa", available: true, search: func(ctx context.Context, _ Query) ([]Result, error) {
		<-ctx.Done()
		return nil, ctx.Err()
	}}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	client := newTestClient(provider)
	if _, err := client.Search(ctx, SearchRequest{Query: "x"}); err == nil {
		t.Fatal("cancelled context returned no error")
	}
}

// TestHintNamesTheContentTool: the bounded answer is only useful if it says how
// to read the rest. This is the one sentence that stops a re-search loop.
func TestHintNamesTheContentTool(t *testing.T) {
	client := newTestClient(&fakeProvider{name: "exa", available: true})
	response, err := client.Search(context.Background(), SearchRequest{Query: "x"})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if !strings.HasPrefix(response.ResponseID, "s_") {
		t.Errorf("responseId = %q, want the s_ prefix", response.ResponseID)
	}
	for _, want := range []string{"get_search_content", response.ResponseID, "findText"} {
		if !strings.Contains(response.Hint, want) {
			t.Errorf("hint %q is missing %q", response.Hint, want)
		}
	}
}

// TestStoredTextExceedsInlineSnippet is the anti-regression for the whole
// design: the store must keep the FULL snippet even though the inline answer is
// truncated to SnippetRunes.
func TestStoredTextExceedsInlineSnippet(t *testing.T) {
	long := strings.Repeat("limiting factor ", 200) // ~3000 runes
	client := newTestClient(&fakeProvider{name: "exa", available: true, search: func(context.Context, Query) ([]Result, error) {
		return []Result{{Title: "Enzymes", URL: "https://example.org/enzymes", Snippet: long}}, nil
	}})

	response, err := client.Search(context.Background(), SearchRequest{Query: "enzymes"})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	inline := response.Queries[0].Results[0].Snippet
	if utf8RuneCount(inline) > SnippetRunes+2 {
		t.Fatalf("inline snippet is %d runes, want <= %d", utf8RuneCount(inline), SnippetRunes+2)
	}

	entry, err := client.Store().Get(response.ResponseID)
	if err != nil {
		t.Fatalf("store lookup: %v", err)
	}
	stored := entry.Queries[0].Results[0].Snippet
	if utf8RuneCount(stored) <= SnippetRunes {
		t.Errorf("stored snippet is %d runes, want the full %d", utf8RuneCount(stored), utf8RuneCount(long))
	}
}

// TestBudgetDropsWholeResultsAndCountsThem: the inline budget must cut whole
// hits (never half a JSON value) and report the count.
func TestBudgetDropsWholeResultsAndCountsThem(t *testing.T) {
	client := newTestClient(&fakeProvider{name: "exa", available: true, search: func(context.Context, Query) ([]Result, error) {
		results := make([]Result, 0, 10)
		for i := 0; i < 10; i++ {
			results = append(results, Result{
				Title:   fmt.Sprintf("hit %d", i),
				URL:     fmt.Sprintf("https://example.org/%d", i),
				Snippet: strings.Repeat("x", 300),
			})
		}
		return results, nil
	}})
	client.budgetRunes = 1000

	response, err := client.Search(context.Background(), SearchRequest{Query: "x", NumResults: 10})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	shown := len(response.Queries[0].Results)
	if shown == 0 || shown == 10 {
		t.Fatalf("shown = %d, want a partial set", shown)
	}
	if response.OmittedResults != 10-shown {
		t.Errorf("omittedResults = %d, want %d", response.OmittedResults, 10-shown)
	}
	if !strings.Contains(response.Hint, "stored") {
		t.Errorf("hint does not mention the store: %q", response.Hint)
	}
}
