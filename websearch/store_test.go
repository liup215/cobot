package websearch

import (
	"errors"
	"strings"
	"testing"
	"time"
)

func sampleQueries() []StoredQuery {
	return []StoredQuery{
		{Query: "first", Provider: "exa", Results: []Result{
			{Title: "Cell structure", URL: "https://example.org/cell", Snippet: "Organelles carry out specific functions."},
			{Title: "Enzymes", URL: "https://example.org/enzymes", Snippet: "Limiting factors slow the rate of reaction."},
		}},
		{Query: "second", Provider: "tavily", Results: []Result{
			{Title: "Photosynthesis", URL: "https://example.org/ps", Snippet: "Light intensity affects the rate."},
		}},
	}
}

// TestStoreRoundTripAndFlatten pins the stored text layout: title, URL, blank
// line, snippet — the bytes paging and findText both walk.
func TestStoreRoundTripAndFlatten(t *testing.T) {
	store := NewStore()
	id := store.Put(KindSearch, sampleQueries())

	if !strings.HasPrefix(id, "s_") {
		t.Fatalf("id = %q, want s_ prefix", id)
	}

	entry, err := store.Get(id)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if len(entry.Queries) != 2 {
		t.Fatalf("queries = %d, want 2", len(entry.Queries))
	}

	content := entry.Queries[0].Content
	want := "Cell structure\nhttps://example.org/cell\n\nOrganelles carry out specific functions.\n\nEnzymes\nhttps://example.org/enzymes\n\nLimiting factors slow the rate of reaction."
	if content != want {
		t.Errorf("flattened content =\n%q\nwant\n%q", content, want)
	}
}

// TestStoreUnknownID: unknown and empty ids are the same answer.
func TestStoreUnknownID(t *testing.T) {
	store := NewStore()
	for _, id := range []string{"", "s_nope", "   "} {
		if _, err := store.Get(id); !errors.Is(err, ErrNotFound) {
			t.Errorf("Get(%q) err = %v, want ErrNotFound", id, err)
		}
	}
}

// TestStoreExpires: an entry older than the age limit is gone, and the caller
// is told "not found" — never handed an empty page.
func TestStoreExpires(t *testing.T) {
	now := time.Now()
	store := NewStore(WithStoreAge(time.Hour), WithStoreClock(func() time.Time { return now }))
	id := store.Put(KindSearch, sampleQueries())

	if _, err := store.Get(id); err != nil {
		t.Fatalf("fresh entry missing: %v", err)
	}

	now = now.Add(time.Hour + time.Second)
	if _, err := store.Get(id); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expired entry err = %v, want ErrNotFound", err)
	}
	if store.Len() != 0 {
		t.Errorf("expired entry still counts: Len = %d", store.Len())
	}
}

// TestStoreEvictsLeastRecentlyUsed: touching an entry protects it; the oldest
// untouched one goes first.
func TestStoreEvictsLeastRecentlyUsed(t *testing.T) {
	store := NewStore(WithStoreLimits(2, 1_000_000))
	first := store.Put(KindSearch, sampleQueries())
	second := store.Put(KindSearch, sampleQueries())

	if _, err := store.Get(first); err != nil { // touch first
		t.Fatalf("Get(first): %v", err)
	}
	third := store.Put(KindSearch, sampleQueries())

	if _, err := store.Get(first); err != nil {
		t.Errorf("recently used entry was evicted: %v", err)
	}
	if _, err := store.Get(third); err != nil {
		t.Errorf("newest entry was evicted: %v", err)
	}
	if _, err := store.Get(second); !errors.Is(err, ErrNotFound) {
		t.Errorf("least recently used entry survived: %v", err)
	}
}

// TestStoreSurvivesANewestEntryOverRuneCap: the entry just stored is never the
// one evicted, even when it alone exceeds the total budget.
func TestStoreKeepsNewestEntryOverRuneCap(t *testing.T) {
	store := NewStore(WithStoreLimits(8, 10))
	id := store.Put(KindSearch, sampleQueries())
	if _, err := store.Get(id); err != nil {
		t.Fatalf("the entry just stored was evicted: %v", err)
	}
}

// TestSliceContentIsRuneBased: offsets count characters, so a Chinese page pages
// at the same size as an English one.
func TestSliceContentIsRuneBased(t *testing.T) {
	content := strings.Repeat("细胞结构", 100) // 400 runes, 1200 bytes

	page, total, next := SliceContent(content, 0, 50)
	if total != 400 {
		t.Fatalf("total = %d, want 400 runes", total)
	}
	if utf8RuneCount(page) != 50 {
		t.Fatalf("page = %d runes, want 50", utf8RuneCount(page))
	}
	if next != 50 {
		t.Errorf("nextOffset = %d, want 50", next)
	}
	if page != strings.Repeat("细胞结构", 12)+"细胞" {
		t.Errorf("page content = %q", page)
	}

	last, _, next := SliceContent(content, 380, 50)
	if utf8RuneCount(last) != 20 {
		t.Errorf("tail page = %d runes, want 20", utf8RuneCount(last))
	}
	if next != 0 {
		t.Errorf("nextOffset at the end = %d, want 0", next)
	}

	// Out-of-range and silly values degrade instead of panicking.
	if page, _, _ := SliceContent(content, 9999, 0); page != "" {
		t.Errorf("offset past the end returned %q", page)
	}
	big, _, _ := SliceContent(content, 0, 999_999)
	if utf8RuneCount(big) != 400 {
		t.Errorf("oversized limit returned %d runes, want the default page cap", utf8RuneCount(big))
	}
}

// TestFindCaseInsensitiveDefault finds a fact without exact casing.
func TestFindCaseInsensitiveDefault(t *testing.T) {
	content := "Limiting factors slow the rate of reaction. Temperature matters most."

	found := Find(content, []string{"LIMITING FACTORS"}, FindCaseInsensitive, MaxPageRunes)
	if found.TotalMatches != 1 || len(found.Passages) != 1 {
		t.Fatalf("found = %+v", found)
	}
	if !strings.Contains(found.Passages[0].Text, "Temperature matters most") {
		t.Errorf("passage lacks context: %q", found.Passages[0].Text)
	}
	if found.Passages[0].Offset != 0 {
		t.Errorf("offset = %d, want 0", found.Passages[0].Offset)
	}
}

// TestFindExactIsCaseSensitive: "exact" must actually be exact.
func TestFindExactIsCaseSensitive(t *testing.T) {
	content := "Limiting factors slow the rate."
	if found := Find(content, []string{"limiting"}, FindExact, MaxPageRunes); found.TotalMatches != 0 {
		t.Errorf("exact match found %d hits for lower-case query", found.TotalMatches)
	}
	if found := Find(content, []string{"Limiting"}, FindExact, MaxPageRunes); found.TotalMatches != 1 {
		t.Errorf("exact match found %d hits, want 1", found.TotalMatches)
	}
}

// TestFindFuzzyToleratesInflection: the model asks for "limiting factors" and
// the page says "limiting factor".
func TestFindFuzzyToleratesInflection(t *testing.T) {
	content := "A limiting factor is anything that slows a reaction.\n\nThe rate rises with temperature."

	found := Find(content, []string{"limiting factors"}, FindFuzzy, MaxPageRunes)
	if found.TotalMatches == 0 {
		t.Fatalf("fuzzy search found nothing: %+v", found)
	}
	if !strings.Contains(found.Passages[0].Text, "limiting factor") {
		t.Errorf("passage = %q", found.Passages[0].Text)
	}
}

// TestFindFuzzyFallsBackForCJK: a Chinese query has no whitespace-separated
// tokens, so fuzzy must behave as containment instead of matching nothing.
func TestFindFuzzyFallsBackForCJK(t *testing.T) {
	content := "光合作用的限制因素包括光照强度和二氧化碳浓度。\n\n温度也会影响酶活性。"

	found := Find(content, []string{"限制因素"}, FindFuzzy, MaxPageRunes)
	if found.TotalMatches != 1 {
		t.Fatalf("CJK fuzzy found %d matches, want 1", found.TotalMatches)
	}
	if !strings.Contains(found.Passages[0].Text, "限制因素") {
		t.Errorf("passage = %q", found.Passages[0].Text)
	}
}

// TestFindMergesAdjacentContext: two nearby matches must not return the same
// paragraph twice.
func TestFindMergesAdjacentContext(t *testing.T) {
	content := "alpha beta gamma delta epsilon"

	found := Find(content, []string{"alpha", "beta"}, FindCaseInsensitive, MaxPageRunes)
	if len(found.Passages) != 1 {
		t.Fatalf("passages = %d, want 1 merged passage", len(found.Passages))
	}
	if found.TotalMatches != 2 {
		t.Errorf("totalMatches = %d, want 2", found.TotalMatches)
	}
}

// TestFindRespectsBudget: a tiny budget truncates with a flag rather than
// silently returning everything.
func TestFindRespectsBudget(t *testing.T) {
	content := strings.Repeat("needle and hay. ", 200)

	found := Find(content, []string{"needle"}, FindCaseInsensitive, 300)
	if !found.Truncated {
		t.Errorf("Truncated = false, want true")
	}
	if found.ReturnedMatches == 0 {
		t.Fatalf("nothing returned at all: %+v", found)
	}
	total := 0
	for _, passage := range found.Passages {
		total += utf8RuneCount(passage.Text)
	}
	if total > 300 {
		t.Errorf("returned %d runes, budget was 300", total)
	}
}

// TestFindNoMatchReturnsEmptySlice: JSON must contain [] rather than null so a
// host renderer can iterate without a nil check.
func TestFindNoMatchReturnsSlice(t *testing.T) {
	found := Find("nothing here", []string{"absent"}, FindCaseInsensitive, MaxPageRunes)
	if found.TotalMatches != 0 {
		t.Errorf("totalMatches = %d", found.TotalMatches)
	}
	if found.Passages == nil {
		t.Error("Passages is nil, want an empty slice")
	}
}

// TestParseFindModeRejectsUnknown mirrors the schema enum.
func TestParseFindModeRejectsUnknown(t *testing.T) {
	if mode, err := ParseFindMode(""); err != nil || mode != FindCaseInsensitive {
		t.Errorf("empty mode = %v, %v", mode, err)
	}
	if _, err := ParseFindMode("regex"); err == nil {
		t.Error("unknown mode was accepted")
	}
}

// TestNormalizeDomains: schemes and paths are stripped, "-" excludes, junk is an
// error (an emptied filter would silently widen the search).
func TestNormalizeDomains(t *testing.T) {
	got, err := normalizeDomains([]string{"CambridgeInternational.org", "-https://pinterest.com/pin/1", "example.com/"})
	if err != nil {
		t.Fatalf("normalizeDomains: %v", err)
	}
	want := []string{"cambridgeinternational.org", "-pinterest.com", "example.com"}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got %v, want %v", got, want)
		}
	}

	if _, err := normalizeDomains([]string{"-"}); err == nil {
		t.Error("a bare - was accepted")
	}
}
