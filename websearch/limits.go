package websearch

import "time"

// Caps. These are the whole point of this package — every one of them exists
// because the unbounded version of the same field is a real failure mode, so
// each carries its reason.
const (
	// DefaultNumResults is what the model gets when it does not ask.
	DefaultNumResults = 5

	// MaxNumResults is the schema maximum. pi allows 20; 10 is plenty for a
	// model that is going to fetch two or three pages anyway, and it halves
	// the worst-case inline size.
	MaxNumResults = 10

	// MaxQueries caps the fan-out. pi runs up to three concurrently and models
	// rarely produce more than four useful angles; extra queries buy duplicated
	// snippets, not coverage.
	MaxQueries = 4

	// MaxConcurrency bounds in-flight provider requests. Three is pi's number
	// and it keeps a fan-out from tripping provider per-key rate limits.
	MaxConcurrency = 3

	// SnippetRunes is the inline snippet cap (runes, not bytes: CJK pages must
	// not be cut to a third of everyone else's length).
	SnippetRunes = 400

	// BudgetRunes is the total inline budget for one web_search answer. Four
	// queries at ten hits each cannot fill it, which is intentional: the model
	// gets titles, URLs and the head of each snippet, then pages what it needs.
	BudgetRunes = 12_000

	// MaxStoredSnippetRunes caps one stored hit. Exa's highlights can run to
	// tens of kilobytes; the store must not grow without bound just because a
	// provider felt chatty.
	MaxStoredSnippetRunes = 20_000

	// DefaultPageRunes is the default get_search_content page size, and
	// MaxPageRunes its hard ceiling.
	DefaultPageRunes = 6_000
	MaxPageRunes     = 12_000

	// FindContextRunes is the context kept on each side of a findText match.
	FindContextRunes = 400

	// MaxFindPassages bounds how many passages one findText call returns.
	MaxFindPassages = 20

	// DefaultTimeout is the per-request HTTP timeout.
	DefaultTimeout = 20 * time.Second

	// maxRetryWait is the longest Retry-After the client will sleep through.
	// A longer cooldown moves the query to the next provider instead — parking
	// a tool call for a minute is worse than a second provider's answer.
	maxRetryWait = 3 * time.Second

	// maxResponseBytes bounds one provider response body.
	maxResponseBytes = 8 << 20
)

// Store caps. Exported so a host can size the store to its own session length.
const (
	// StoreEntries is how many responseIds stay readable at once.
	StoreEntries = 64
	// StoreAge is the lifetime of a stored response. An hour matches pi and is
	// long enough to finish a research turn, short enough that a chat resumed
	// tomorrow gets an honest "expired, search again".
	StoreAge = time.Hour
	// StoreRunes caps the total stored text (≈ 8MB of English).
	StoreRunes = 4_000_000
)

// KindSearch tags a store entry as a search result set (fetch results will use
// a different kind and a different prefix).
const KindSearch = "search"
