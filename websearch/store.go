package websearch

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"
)

// ErrNotFound is returned for an unknown or expired responseId. Callers must
// treat the two cases identically: telling a caller that an id once existed but
// expired is a distinction the model cannot act on, and the honest instruction
// is always "search again".
var ErrNotFound = errors.New("response not found")

// Entry is one stored response set.
type Entry struct {
	ID        string
	Kind      string
	CreatedAt time.Time
	Queries   []StoredQuery
	URLs      []StoredURL
	runes     int
}

// Store keeps search results alive for the length of a research turn.
//
// Why a store at all: the inline answer is intentionally truncated (see
// SearchResponse), so the ONLY way to read a provider's full text is to come
// back with the responseId. That makes the store part of the contract, not a
// cache —
//
//	an evicted entry must produce a clear "search again", never an empty page.
//
// Eviction is by age, entry count and total text size, oldest-use first, so a
// long session cannot grow without bound.
type Store struct {
	mu         sync.Mutex
	entries    map[string]*Entry
	order      []string // ids, least recently used first
	maxAge     time.Duration
	maxEntries int
	maxRunes   int
	usedRunes  int
	now        func() time.Time
}

// StoreOption configures a Store.
type StoreOption func(*Store)

// WithStoreLimits overrides the entry count and total rune caps.
func WithStoreLimits(entries, runes int) StoreOption {
	return func(s *Store) {
		if entries > 0 {
			s.maxEntries = entries
		}
		if runes > 0 {
			s.maxRunes = runes
		}
	}
}

// WithStoreAge overrides the entry lifetime.
func WithStoreAge(age time.Duration) StoreOption {
	return func(s *Store) {
		if age > 0 {
			s.maxAge = age
		}
	}
}

// WithStoreClock injects the clock (tests exercise expiry without sleeping).
func WithStoreClock(now func() time.Time) StoreOption {
	return func(s *Store) {
		if now != nil {
			s.now = now
		}
	}
}

// NewStore builds an empty store with the package defaults.
func NewStore(opts ...StoreOption) *Store {
	s := &Store{
		entries:    make(map[string]*Entry),
		maxAge:     StoreAge,
		maxEntries: StoreEntries,
		maxRunes:   StoreRunes,
		now:        time.Now,
	}
	for _, opt := range opts {
		opt(s)
	}
	return s
}

// StoredURL is one fetched page kept in the store.
//
// Content is the EXTRACTED text (markdown for HTML, plain text for PDF and
// text/*), never raw HTML — the point of fetch_content is that the model sees a
// readable page, and storing the markup would double the size for no gain.
type StoredURL struct {
	URL      string
	Title    string
	MIMEType string
	// Via names the pipeline that produced the text: "html" (readability +
	// markdown), "pdf", "document", "text", "raw". Surfaced in the result so a
	// reader can tell why a page looks the way it does.
	Via    string
	Status int
	Error  string
	// Content is the full extracted text; get_search_content pages it.
	Content string
}

// Put stores a set of query results and returns its responseId.
func (s *Store) Put(kind string, queries []StoredQuery) string {
	stored := make([]StoredQuery, len(queries))
	runes := 0
	for i, q := range queries {
		q.Content = flatten(q)
		runes += utf8RuneCount(q.Content)
		stored[i] = q
	}
	return s.insert(&Entry{Kind: kind, Queries: stored, runes: runes})
}

// PutFetch stores fetched documents and returns their responseId.
func (s *Store) PutFetch(urls []StoredURL) string {
	stored := make([]StoredURL, len(urls))
	runes := 0
	for i, u := range urls {
		u.Content = strings.TrimSpace(u.Content)
		runes += utf8RuneCount(u.Content)
		stored[i] = u
	}
	return s.insert(&Entry{Kind: KindFetch, URLs: stored, runes: runes})
}

// insert assigns the id, stamps the time and evicts to fit the caps.
func (s *Store) insert(entry *Entry) string {
	entry.ID = newResponseID(entry.Kind)
	entry.CreatedAt = s.now()

	s.mu.Lock()
	defer s.mu.Unlock()

	s.pruneExpiredLocked()
	s.entries[entry.ID] = entry
	s.order = append(s.order, entry.ID)
	s.usedRunes += entry.runes

	// Evict oldest-use first, never the entry just stored (it is the one the
	// caller is about to read).
	for (len(s.entries) > s.maxEntries || s.usedRunes > s.maxRunes) && len(s.order) > 1 {
		oldest := s.order[0]
		if oldest == entry.ID {
			break
		}
		s.order = s.order[1:]
		s.dropLocked(oldest)
	}
	return entry.ID
}

// Get returns a stored entry and marks it recently used.
func (s *Store) Get(id string) (*Entry, error) {
	id = strings.TrimSpace(id)
	if id == "" {
		return nil, ErrNotFound
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	entry, ok := s.entries[id]
	if !ok {
		return nil, ErrNotFound
	}
	if s.maxAge > 0 && s.now().Sub(entry.CreatedAt) > s.maxAge {
		s.dropLocked(id)
		return nil, ErrNotFound
	}
	s.touchLocked(id)
	return entry, nil
}

// Len reports how many entries are stored (tests, diagnostics).
func (s *Store) Len() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.entries)
}

func (s *Store) touchLocked(id string) {
	for i, existing := range s.order {
		if existing == id {
			s.order = append(s.order[:i], s.order[i+1:]...)
			break
		}
	}
	s.order = append(s.order, id)
}

func (s *Store) pruneExpiredLocked() {
	if s.maxAge <= 0 {
		return
	}
	now := s.now()
	expired := make([]string, 0, 2)
	for id, entry := range s.entries {
		if now.Sub(entry.CreatedAt) > s.maxAge {
			expired = append(expired, id)
		}
	}
	for _, id := range expired {
		s.dropLocked(id)
	}
}

func (s *Store) dropLocked(id string) {
	entry, ok := s.entries[id]
	if !ok {
		return
	}
	s.usedRunes -= entry.runes
	delete(s.entries, id)
	for i, existing := range s.order {
		if existing == id {
			s.order = append(s.order[:i], s.order[i+1:]...)
			break
		}
	}
}

// flatten renders one query's hits into the text paging and findText walk.
//
// The format is fixed and boring on purpose: a title line, the URL, a blank
// line, the full snippet, a blank line, repeat. Every consumer (paging,
// findText, a human reading a page) then sees the same bytes, so an offset
// returned by one call still means the same thing in the next.
func flatten(q StoredQuery) string {
	var b strings.Builder
	for i, r := range q.Results {
		if i > 0 {
			b.WriteString("\n\n")
		}
		title := strings.TrimSpace(r.Title)
		if title == "" {
			title = hostLabel(r.URL)
		}
		b.WriteString(title)
		b.WriteString("\n")
		b.WriteString(r.URL)
		if snippet := strings.TrimSpace(r.Snippet); snippet != "" {
			b.WriteString("\n\n")
			b.WriteString(snippet)
		}
	}
	return b.String()
}

// newResponseID generates "s_" / "f_" + 12 hex chars. The prefix carries the
// kind so a mixed transcript shows at a glance what an id refers to.
func newResponseID(kind string) string {
	prefix := "r_"
	switch kind {
	case KindSearch:
		prefix = "s_"
	case KindFetch:
		prefix = "f_"
	}
	buf := make([]byte, 6)
	if _, err := rand.Read(buf); err != nil {
		// crypto/rand failing is not a reason to lose results: fall back to
		// the clock, accepting the (theoretical) collision.
		return fmt.Sprintf("%s%d", prefix, time.Now().UnixNano())
	}
	return prefix + hex.EncodeToString(buf)
}
