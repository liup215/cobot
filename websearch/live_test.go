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

// TestLiveFetchContent exercises the REAL fetch pipeline: a live page through
// readability, a live PDF through docparse, and the private-address gate. Opt-in
// for the same reason as the search test — and because a page that renders
// differently for bots is a moving target for an assertion.
//
//	COBOT_WEB_LIVE=1 go test ./websearch/ -run Live -v
func TestLiveFetchContent(t *testing.T) {
	if os.Getenv("COBOT_WEB_LIVE") == "" {
		t.Skip("set COBOT_WEB_LIVE=1 to exercise the live fetch pipeline")
	}

	client := NewClient(
		WithProviders(NewExaProvider()),
		WithStore(NewStore()),
	)
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	response, err := client.Fetch(ctx, FetchRequest{URLs: []string{
		"https://www.cambridgeinternational.org/programmes-and-qualifications/cambridge-international-as-and-a-level-biology-9700/",
		"https://www.w3.org/WAI/ER/tests/xhtml/testfiles/resources/pdf/dummy.pdf",
	}})
	if err != nil {
		t.Fatalf("live fetch: %v", err)
	}
	for i, summary := range response.URLs {
		t.Logf("url[%d] via=%s status=%d chars=%d title=%q err=%q", i, summary.Via, summary.Status, summary.Chars, summary.Title, summary.Error)
	}

	page := response.URLs[0]
	if page.Error != "" || page.Via != "html" {
		t.Fatalf("live page: %+v", page)
	}
	// Assert on what the page actually guarantees: the TITLE carries the syllabus
	// code, the BODY carries the subject. (The first version of this check looked for
	// "9700" in the excerpt and failed against the real page — the body never repeats
	// the code, only the title does.)
	if !strings.Contains(page.Title, "9700") {
		t.Errorf("live page title does not carry the syllabus code: %q", page.Title)
	}
	if !strings.Contains(strings.ToLower(page.Excerpt), "biology") {
		t.Errorf("live page excerpt does not look like the syllabus body: %q", truncateRunesOrBytes(page.Excerpt, 200))
	}

	pdf := response.URLs[1]
	if pdf.Error != "" || pdf.Via != "pdf" {
		t.Fatalf("live pdf: %+v", pdf)
	}
	if !strings.Contains(strings.ToLower(pdf.Excerpt), "dummy") {
		t.Errorf("live pdf text does not contain its own content: %q", pdf.Excerpt)
	}

	// The gate must hold on the real client too (no network needed: it refuses
	// before dialing).
	blocked, err := client.Fetch(ctx, FetchRequest{URL: "http://169.254.169.254/latest/meta-data/"})
	if err != nil {
		t.Fatalf("gate probe: %v", err)
	}
	if !strings.Contains(blocked.URLs[0].Error, "private") {
		t.Errorf("metadata endpoint was not refused: %+v", blocked.URLs[0])
	}
}

// TestLiveSourceCheck runs source_check against the real web: search, fetch the top
// sources, quote sentences from the page, and refuse to judge.
//
//	COBOT_WEB_LIVE=1 go test ./websearch/ -run Live -v
func TestLiveSourceCheck(t *testing.T) {
	if os.Getenv("COBOT_WEB_LIVE") == "" {
		t.Skip("set COBOT_WEB_LIVE=1 to exercise the live evidence pipeline")
	}

	client := NewClient(WithProviders(NewExaProvider()), WithStore(NewStore()))
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	artifact, err := client.SourceCheck(ctx, SourceCheckRequest{
		Claim:   "Cambridge International AS & A Level Biology 9700 is examined from 2025 to 2027",
		Queries: []string{"Cambridge 9700 Biology syllabus 2025-2027", "9700 syllabus update 2028-2030"},
	})
	if err != nil {
		t.Fatalf("source_check: %v", err)
	}
	t.Logf("status=%s sources=%d passages=%d (total %d) id=%s", artifact.Status, len(artifact.Sources), len(artifact.Passages), artifact.TotalPassages, artifact.ResponseID)

	if artifact.Status != StatusUnclear && artifact.Status != StatusMissingEvidence {
		t.Fatalf("status = %q", artifact.Status)
	}
	if len(artifact.Sources) == 0 {
		t.Fatalf("no sources: %+v", artifact)
	}
	if artifact.Status == StatusUnclear && len(artifact.Passages) == 0 {
		t.Fatalf("unclear with no passages")
	}

	pagePassages := 0
	for _, passage := range artifact.Passages {
		if passage.Origin == OriginPage {
			pagePassages++
		}
	}
	t.Logf("page passages: %d of %d inline", pagePassages, len(artifact.Passages))

	// Whatever the ranking produced, the offsets must locate the text in the stored
	// page — that is what makes the citation usable.
	entry, err := client.Store().Get(artifact.ResponseID)
	if err != nil {
		t.Fatalf("artifact store: %v", err)
	}
	if len(entry.URLs) == 0 {
		t.Fatalf("no page text stored, so no offset can be opened")
	}
	verified := 0
	for _, passage := range artifact.Passages {
		if passage.Origin != OriginPage || passage.Truncated {
			continue
		}
		var content string
		for _, doc := range entry.URLs {
			if doc.URL == passage.URL {
				content = doc.Content
			}
		}
		if content == "" {
			t.Errorf("page for passage %s was not stored (%s)", passage.ID, passage.URL)
			continue
		}
		runes := []rune(content)
		if passage.End > len(runes) {
			t.Errorf("passage %s end %d > page %d runes", passage.ID, passage.End, len(runes))
			continue
		}
		if got := strings.TrimSpace(string(runes[passage.Start:passage.End])); got != passage.Text {
			t.Errorf("offset %d..%d reads %q, passage says %q", passage.Start, passage.End, got, passage.Text)
		}
		verified++
	}
	t.Logf("verified %d page-passage offsets", verified)
}
