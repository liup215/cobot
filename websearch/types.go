package websearch

import (
	"context"
	"time"
)

// Recency windows accepted by the recencyFilter parameter. The provider
// translates them itself (Exa appends natural-language date words to the query
// because its public endpoint ignores structured filters; Tavily maps them to
// time_range).
const (
	RecencyDay   = "day"
	RecencyWeek  = "week"
	RecencyMonth = "month"
	RecencyYear  = "year"
)

// Result is one search hit.
//
// Snippet is the provider's own text excerpt (Exa calls it Highlights, Tavily
// content). It is stored in FULL — reading it back in pages is what
// get_search_content is for — and only truncated for the inline answer.
type Result struct {
	Title   string `json:"title"`
	URL     string `json:"url"`
	Snippet string `json:"snippet"`
}

// Query is a normalized search request.
type Query struct {
	Text       string
	NumResults int
	Recency    string
	// Domains carries include and exclude entries in pi's notation: a leading
	// "-" means exclude ("-pinterest.com").
	Domains []string
}

// QueryOutcome is one query's entry in the model-facing payload.
//
// Error is per query, never global: a chain that failed for this query leaves
// the OTHER queries' results intact and names every provider it tried.
type QueryOutcome struct {
	Query    string   `json:"query"`
	Provider string   `json:"provider,omitempty"`
	Error    string   `json:"error"`
	Results  []Result `json:"results"`
}

// SearchResponse is the web_search tool result.
//
// ResponseID is the handle for the stored, untruncated text. OmittedResults
// counts hits dropped by the inline budget so the model can tell "the web has
// nothing more" apart from "you are not being shown the rest".
type SearchResponse struct {
	ResponseID     string         `json:"responseId"`
	Providers      []string       `json:"providers"`
	Queries        []QueryOutcome `json:"queries"`
	OmittedResults int            `json:"omittedResults,omitempty"`
	Hint           string         `json:"hint"`
}

// StoredQuery is one query kept in the store: the same shape as
// QueryOutcome plus the flattened text that paging and findText walk.
type StoredQuery struct {
	Query    string   `json:"query"`
	Provider string   `json:"provider,omitempty"`
	Error    string   `json:"error,omitempty"`
	Results  []Result `json:"results"`
	// Content is the flattened, full-length rendering of Results:
	// title, URL and the complete snippet per hit, separated by blank lines.
	// Compute it with flatten (store.go) so paging offsets and findText
	// ranges always refer to the same bytes the model will read.
	Content string `json:"-"`
}

// Provider is one search backend.
type Provider interface {
	// Name is the selector used by the provider parameter and reported in
	// results, so a user can see which backend answered.
	Name() string
	// Available reports whether this provider can run right now. A missing
	// key means false — the chain skips it instead of failing the search.
	Available() bool
	// Search runs one query. Returning a RateLimitError asks the client to
	// retry once after the advertised delay.
	Search(ctx context.Context, q Query) ([]Result, error)
}

// RateLimitError is a provider's 429 with its advertised cooldown.
type RateLimitError struct {
	Provider   string
	RetryAfter time.Duration
	Detail     string
}

func (e *RateLimitError) Error() string {
	if e.Detail == "" {
		return e.Provider + ": rate limited"
	}
	return e.Provider + ": rate limited (" + e.Detail + ")"
}
