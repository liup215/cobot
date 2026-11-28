package websearch

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"
)

// TestLiveExaSearch exercises the REAL public endpoint. It is opt-in because a
// unit suite must never depend on the network — but it is worth keeping, since
// the endpoint is a third party that can change its reply shape without telling
// us (the parser already answers in two shapes for exactly that reason).
//
//	COBOT_WEB_LIVE=1 go test ./websearch/ -run Live -v
func TestLiveExaSearch(t *testing.T) {
	if os.Getenv("COBOT_WEB_LIVE") == "" {
		t.Skip("set COBOT_WEB_LIVE=1 to exercise the live Exa endpoint")
	}

	client := NewClient(WithProviders(NewExaProvider()))
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	response, err := client.Search(ctx, SearchRequest{
		Query:      "Cambridge International AS A Level Biology 9700 syllabus",
		NumResults: 3,
		Domains:    []string{"cambridgeinternational.org"},
	})
	if err != nil {
		t.Fatalf("live search: %v", err)
	}
	outcome := response.Queries[0]
	if outcome.Error != "" {
		t.Fatalf("live search failed: %s", outcome.Error)
	}
	if len(outcome.Results) == 0 {
		t.Fatalf("live search returned no hits: %+v", response)
	}
	t.Logf("provider=%s hits=%d responseId=%s", outcome.Provider, len(outcome.Results), response.ResponseID)

	// The domain filter must actually narrow: Exa ignores structured filters, so
	// this is the assertion that the query-text encoding works on the real API.
	for _, result := range outcome.Results {
		if !strings.Contains(result.URL, "cambridgeinternational.org") {
			t.Errorf("domainFilter leaked %q", result.URL)
		}
		if strings.TrimSpace(result.Snippet) == "" {
			t.Errorf("hit %q has no snippet — the block parser found no Highlights", result.URL)
		}
	}

	// And the stored text must be readable back in pages.
	entry, err := client.Store().Get(response.ResponseID)
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	page, total, next := SliceContent(entry.Queries[0].Content, 0, 200)
	if total == 0 || page == "" {
		t.Fatalf("stored content is empty (total=%d)", total)
	}
	if next == 0 && total > 200 {
		t.Errorf("nextOffset = 0 with total = %d", total)
	}
	t.Logf("stored %d runes, first page %d runes, next=%d", total, utf8RuneCount(page), next)

	// findText has to locate a term that the page actually contains.
	found := Find(entry.Queries[0].Content, []string{"9700"}, FindCaseInsensitive, MaxPageRunes)
	if found.TotalMatches == 0 {
		t.Errorf("findText found no \"9700\" in %d runes of stored text", total)
	}
}
