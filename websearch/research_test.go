package websearch

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// --- the refusal is the feature --------------------------------------------

// TestStatusVocabularyIsTwo is the guard on this tool's whole premise. If a third
// status ever appears, the tool starts asserting things its sentence matching cannot
// know, and a reader would trust the label.
func TestStatusVocabularyIsTwo(t *testing.T) {
	statuses := map[string]bool{StatusMissingEvidence: true, StatusUnclear: true}
	if len(statuses) != 2 {
		t.Fatalf("the vocabulary grew to %d", len(statuses))
	}

	for count, want := range map[int]string{0: StatusMissingEvidence, 1: StatusUnclear, 9: StatusUnclear} {
		status, rationale := assess(count)
		if status != want {
			t.Errorf("assess(%d) = %q, want %q", count, status, want)
		}
		if strings.TrimSpace(rationale) == "" {
			t.Errorf("assess(%d) has no rationale", count)
		}
	}
}

// TestPolarityIsNotJudged is the concrete case the design exists for: a passage that
// says the OPPOSITE of the claim overlaps with it almost completely, so a
// word-overlap scorer must still answer "unclear" rather than "supported".
func TestPolarityIsNotJudged(t *testing.T) {
	claim := "The 2025 syllabus no longer includes the practical on enzymes"
	contradicting := "The 2025 syllabus still includes the practical on enzymes for all candidates."

	if status, _ := assess(1); status != StatusUnclear {
		t.Fatalf("status = %q", status)
	}
	// And the selector really does rank that sentence: it shares nearly every term.
	terms := claimTerms(claim)
	if overlap(terms, strings.ToLower(contradicting)) < 5 {
		t.Errorf("fixture no longer overlaps the claim (%d hits) — it does not test what it claims to", overlap(terms, strings.ToLower(contradicting)))
	}
	_, rationale := assess(1)
	if !strings.Contains(rationale, "support or contradict") {
		t.Errorf("the rationale must say the reader decides: %q", rationale)
	}
}

// --- passage selection -----------------------------------------------------

// TestSplitSentencesKeepsOffsets: a passage's [start,end) must index the ORIGINAL
// content, or the "open the same page at the same place" promise is a lie.
func TestSplitSentencesKeepsOffsets(t *testing.T) {
	content := "# Content\n\nCell structure is the study of organelles and their functions in a cell.\nPhotosynthesis converts light energy into chemical energy in chloroplasts.\n\n短句。\n这是一句关于限制因素和光合作用的完整说明句子。"

	sentences := splitSentences(content)
	if len(sentences) < 3 {
		t.Fatalf("got %d sentences: %+v", len(sentences), sentences)
	}
	runes := []rune(content)
	for _, sentence := range sentences {
		if sentence.start < 0 || sentence.end > len(runes) || sentence.start >= sentence.end {
			t.Fatalf("bad offsets %d..%d (len %d)", sentence.start, sentence.end, len(runes))
		}
		if got := strings.TrimSpace(string(runes[sentence.start:sentence.end])); got != sentence.text {
			t.Errorf("offset %d..%d renders %q, text is %q", sentence.start, sentence.end, got, sentence.text)
		}
	}
	// The one-word line is not evidence.
	for _, sentence := range sentences {
		if sentence.text == "短句。" {
			t.Error("a fragment shorter than minSentenceRunes became a passage")
		}
	}
}

// TestClaimTerms covers the three token kinds: latin words, bare numbers, CJK bigrams.
func TestClaimTerms(t *testing.T) {
	terms := claimTerms("The 429 error and TLS handshake: 限制因素导致光合作用下降")
	lookup := map[string]bool{}
	for _, term := range terms {
		lookup[term] = true
	}
	for _, want := range []string{"429", "tls", "error", "限制", "因素", "光合"} {
		if !lookup[want] {
			t.Errorf("missing term %q in %v", want, terms)
		}
	}
	// Two-rune latin words ("of", "in") are noise.
	if lookup["the"] {
		t.Errorf("stopword-length token kept: %v", terms)
	}
}

// TestRankSentencesPicksTheOverlappingOne: the passage chosen is the one that shares
// the claim's vocabulary, in score order, capped at MaxPassagesPerSource.
func TestRankSentencesPicksTheOverlappingOne(t *testing.T) {
	content := "This page is about the history of the school and its founders.\nLimiting factors slow the rate of photosynthesis in plants.\nUnrelated filler about admissions deadlines and fees."

	ranked := rankSentences(splitSentences(content), claimTerms("limiting factors of photosynthesis"), "")
	if len(ranked) != 1 {
		t.Fatalf("ranked %d sentences, want 1: %+v", len(ranked), ranked)
	}
	if !strings.Contains(ranked[0].text, "Limiting factors") {
		t.Errorf("wrong sentence: %q", ranked[0].text)
	}
}

// TestClassifySource pins the authority buckets, exam boards first: for an A-level
// product those are the authority and they are not .gov/.edu.
func TestClassifySource(t *testing.T) {
	cases := map[string]string{
		"https://www.cambridgeinternational.org/programmes-and-qualifications/biology-9700/": QualityExamBoard,
		"https://qualifications.pearson.com/en/x":                                            QualityExamBoard,
		"https://www.gov.uk/government/publications/x":                                       QualityOfficial,
		"https://www.nasa.gov/mission":                                                       QualityOfficial,
		"https://www.nature.com/articles/x":                                                  QualityAcademic,
		"https://pubmed.ncbi.nlm.nih.gov/1234/":                                              QualityAcademic,
		"https://en.wikipedia.org/wiki/Photosynthesis":                                       QualityReference,
		"https://www.bbc.co.uk/news/education-1":                                             QualityNews,
		"https://www.reddit.com/r/alevel/comments/x":                                         QualityCommunity,
		"https://some-teacher-blog.example/x":                                                QualityUnknown,
	}
	for url, want := range cases {
		if got := classifySource(url); got != want {
			t.Errorf("classifySource(%q) = %q, want %q", url, got, want)
		}
	}
}

// --- end to end ------------------------------------------------------------

// researchClient builds a client whose provider returns scripted hits and whose pages
// are served by a fixture server (loopback is allow-listed for the fixture only).
func researchClient(t *testing.T, results []Result, pages map[string]string) (*Client, string) {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, ok := pages[r.URL.Path]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprint(w, body)
	}))
	t.Cleanup(server.Close)

	provider := &fakeProvider{name: "exa", available: true, search: func(context.Context, Query) ([]Result, error) {
		return results, nil
	}}
	client := NewClient(
		WithProviders(provider),
		WithStore(NewStore()),
		WithFetchGuard(FetchGuardOptions{AllowLoopback: true}),
	)
	return client, server.URL
}

// TestSourceCheckEndToEnd: ranked sources, a fetched page with offset-bearing page
// passages, the provider summary flagged, hashes that verify, and a pageable id.
func TestSourceCheckEndToEnd(t *testing.T) {
	results := []Result{
		{
			Title:   "Cambridge International AS & A Level Biology (9700)",
			URL:     "__BASE__/9700",
			Snippet: "The syllabus includes the main theoretical concepts.",
		},
		{
			Title:   "Teacher blog",
			URL:     "__BASE__/blog",
			Snippet: "Limiting factors of photosynthesis explained simply.",
		},
	}
	client, base := researchClient(t, nil, map[string]string{
		"/9700": `<html><head><title>9700</title></head><body><article>
			<p>The syllabus includes the main theoretical concepts which are fundamental to the study of biology.</p>
			<p>Limiting factors slow the rate of photosynthesis in plants and algae.</p>
			</article></body></html>`,
	})
	// Results are built lazily so the fixture base URL is known.
	provider := client.providers[0].(*fakeProvider)
	provider.search = func(context.Context, Query) ([]Result, error) {
		out := make([]Result, 0, len(results))
		for _, r := range results {
			r.URL = strings.ReplaceAll(r.URL, "__BASE__", base)
			out = append(out, r)
		}
		return out, nil
	}

	fetchOn := true
	artifact, err := client.SourceCheck(context.Background(), SourceCheckRequest{
		Claim:        "Limiting factors control the rate of photosynthesis",
		Queries:      []string{"limiting factors photosynthesis", "photosynthesis limiting factors 9700"},
		FetchContent: &fetchOn,
	})
	if err != nil {
		t.Fatalf("SourceCheck: %v", err)
	}

	if artifact.Status != StatusUnclear {
		t.Errorf("status = %q, want unclear", artifact.Status)
	}
	if len(artifact.Sources) != 2 {
		t.Fatalf("sources = %d, want 2: %+v", len(artifact.Sources), artifact.Sources)
	}
	// Same URL from both queries is ONE source, and ranks are 1..n in order.
	if artifact.Sources[0].Rank != 1 || artifact.Sources[1].Rank != 2 {
		t.Errorf("ranks = %d,%d", artifact.Sources[0].Rank, artifact.Sources[1].Rank)
	}
	if got := artifact.Sources[0].Quality; got != QualityUnknown && got != QualityExamBoard {
		t.Errorf("quality = %q", got)
	}

	// The fetched page gave page passages, and they are traceable.
	entry, err := client.Store().Get(artifact.ResponseID)
	if err != nil {
		t.Fatalf("artifact not stored: %v", err)
	}
	if entry.Kind != KindResearch || !strings.HasPrefix(artifact.ResponseID, "r_") {
		t.Fatalf("kind=%q id=%q", entry.Kind, artifact.ResponseID)
	}

	var pagePassages, summaryPassages int
	for _, passage := range artifact.Passages {
		sum := sha256.Sum256([]byte(passage.Text))
		if strings.HasPrefix(passage.Hash, "sha256:") {
			// A truncated inline copy hashes the FULL text, so only verify whole ones.
			if !passage.Truncated && passage.Hash != "sha256:"+hex.EncodeToString(sum[:]) {
				t.Errorf("hash does not verify for %q", passage.Text)
			}
		} else {
			t.Errorf("passage %s has no hash", passage.ID)
		}
		switch passage.Origin {
		case OriginPage:
			pagePassages++
			if passage.Start <= 0 && passage.End <= 0 {
				t.Errorf("page passage %s has no offset", passage.ID)
			}
		case OriginProviderSummary:
			summaryPassages++
		default:
			t.Errorf("passage %s has origin %q", passage.ID, passage.Origin)
		}
		if passage.SourceRank < 1 || passage.SourceRank > len(artifact.Sources) {
			t.Errorf("passage %s has sourceRank %d", passage.ID, passage.SourceRank)
		}
	}
	if pagePassages == 0 {
		t.Errorf("no page passages: %+v", artifact.Passages)
	}
	if summaryPassages == 0 {
		t.Errorf("provider summaries were not flagged: %+v", artifact.Passages)
	}

	// Every offset must locate its passage in the STORED page text — that is the
	// whole promise of a citation. The pages are stored alongside the artifact
	// precisely so this can be checked (and so a reader can page to it).
	if len(entry.URLs) == 0 {
		t.Fatal("a research entry stores no page text, so its offsets point at nothing")
	}
	pageByRank := map[int]StoredURL{}
	for i, source := range artifact.Sources {
		if i < len(entry.URLs) && source.Fetched {
			pageByRank[source.Rank] = entry.URLs[i]
		}
	}
	for rank := range pageByRank {
		found := false
		for _, passage := range artifact.Passages {
			if passage.SourceRank == rank && passage.Origin == OriginPage {
				found = true
			}
		}
		if !found {
			t.Errorf("fetched source %d produced no page passage", rank)
		}
	}
	checked := 0
	for _, passage := range artifact.Passages {
		if passage.Origin != OriginPage || passage.Truncated {
			continue
		}
		doc, ok := pageByRank[passage.SourceRank]
		if !ok {
			t.Errorf("page passage %s has no stored page", passage.ID)
			continue
		}
		runes := []rune(doc.Content)
		if passage.Start < 0 || passage.End > len(runes) || passage.Start >= passage.End {
			t.Errorf("passage %s offsets %d..%d outside a %d-rune page", passage.ID, passage.Start, passage.End, len(runes))
			continue
		}
		span := strings.TrimSpace(string(runes[passage.Start:passage.End]))
		if span != passage.Text {
			t.Errorf("offset %d..%d of the stored page reads %q, passage says %q", passage.Start, passage.End, span, passage.Text)
		}
		checked++
	}
	if checked == 0 {
		t.Fatal("no page passage offsets were verifiable")
	}

	if artifact.TotalPassages < len(artifact.Passages) {
		t.Errorf("totalPassages = %d, inline = %d", artifact.TotalPassages, len(artifact.Passages))
	}
	if !strings.Contains(artifact.Hint, "responseId") || !strings.Contains(artifact.Hint, "does not assert") {
		t.Errorf("hint = %q", artifact.Hint)
	}
	if len(artifact.Checks) == 0 {
		t.Error("no verification steps in the artifact")
	}

	// The stored artifact is valid JSON and pageable by the reader tool.
	raw := entry.Queries[0].Content
	var stored Artifact
	if err := json.Unmarshal([]byte(raw), &stored); err != nil {
		t.Fatalf("stored artifact is not JSON: %v", err)
	}
	if stored.Claim == "" || len(stored.Sources) != len(artifact.Sources) {
		t.Errorf("stored artifact is incomplete: %+v", stored)
	}
	if stored.ResponseID != "" {
		t.Error("the stored copy should not claim a responseId of its own")
	}
	page, total, _ := SliceContent(raw, 0, 200)
	if total == 0 || page == "" {
		t.Error("stored artifact is not pageable")
	}
}

// TestSourceCheckDedupesAcrossQueries: four queries surfacing the same page is ONE
// source, not four independent confirmations.
func TestSourceCheckDedupesAcrossQueries(t *testing.T) {
	client, base := researchClient(t, nil, map[string]string{})
	page := base + "/same"
	provider := client.providers[0].(*fakeProvider)
	provider.search = func(_ context.Context, q Query) ([]Result, error) {
		return []Result{{Title: "Same page " + q.Text, URL: page, Snippet: "snippet"}}, nil
	}

	fetchOff := false
	artifact, err := client.SourceCheck(context.Background(), SourceCheckRequest{
		Claim:        "x",
		Queries:      []string{"a", "b", "c"},
		FetchContent: &fetchOff,
	})
	if err != nil {
		t.Fatalf("SourceCheck: %v", err)
	}
	if len(artifact.Sources) != 1 {
		t.Fatalf("sources = %d, want 1 (dedupe by URL)", len(artifact.Sources))
	}
	// Without a fetch there is only the provider paraphrase, and it is labelled as such.
	if len(artifact.Passages) != 1 || artifact.Passages[0].Origin != OriginProviderSummary {
		t.Fatalf("passages = %+v", artifact.Passages)
	}
}

// TestSourceCheckMissingEvidence: no results → no verdict, no id, and the rationale
// says "unchecked, not refuted".
func TestSourceCheckMissingEvidence(t *testing.T) {
	client, _ := researchClient(t, nil, map[string]string{})
	provider := client.providers[0].(*fakeProvider)
	provider.search = func(context.Context, Query) ([]Result, error) {
		return nil, nil
	}

	artifact, err := client.SourceCheck(context.Background(), SourceCheckRequest{Claim: "nothing will match this"})
	if err != nil {
		t.Fatalf("SourceCheck: %v", err)
	}
	if artifact.Status != StatusMissingEvidence {
		t.Errorf("status = %q", artifact.Status)
	}
	if artifact.ResponseID != "" {
		t.Errorf("responseId = %q, want none", artifact.ResponseID)
	}
	if !strings.Contains(artifact.Rationale, "not refuted") {
		t.Errorf("rationale = %q", artifact.Rationale)
	}
}

// TestSourceCheckFetchFailureIsRecorded: a source that could not be read says so on
// the source, instead of silently contributing no evidence.
func TestSourceCheckFetchFailureIsRecorded(t *testing.T) {
	client, base := researchClient(t, nil, map[string]string{})
	provider := client.providers[0].(*fakeProvider)
	provider.search = func(context.Context, Query) ([]Result, error) {
		return []Result{
			{Title: "Gone", URL: base + "/gone", Snippet: "had something to say"},
			{Title: "Fine", URL: base + "/fine", Snippet: "snippet"},
		}, nil
	}

	artifact, err := client.SourceCheck(context.Background(), SourceCheckRequest{Claim: "claim"})
	if err != nil {
		t.Fatalf("SourceCheck: %v", err)
	}
	var gone *ResearchSource
	for i := range artifact.Sources {
		if strings.HasSuffix(artifact.Sources[i].URL, "/gone") {
			gone = &artifact.Sources[i]
		}
	}
	if gone == nil {
		t.Fatal("the failing source vanished from the artifact")
	}
	if gone.Fetched {
		t.Error("a 404 page is marked fetched")
	}
	if !strings.Contains(gone.Error, "404") {
		t.Errorf("source error = %q", gone.Error)
	}
	// Its provider paraphrase is still evidence (weak, and labelled).
	found := false
	for _, passage := range artifact.Passages {
		if passage.SourceRank == gone.Rank && passage.Origin == OriginProviderSummary {
			found = true
		}
	}
	if !found {
		t.Error("the provider paraphrase for the unreadable source is missing")
	}
}

// TestSourceCheckCapsAndBudgets pins the documented limits.
func TestSourceCheckCapsAndBudgets(t *testing.T) {
	client, base := researchClient(t, nil, map[string]string{})
	provider := client.providers[0].(*fakeProvider)
	// Three queries × 10 hits = 30 distinct sources, so the 20-source cap and the
	// omitted count are both exercised.
	provider.search = func(_ context.Context, q Query) ([]Result, error) {
		results := make([]Result, 0, MaxNumResults)
		for i := 0; i < MaxNumResults; i++ {
			results = append(results, Result{
				Title:   fmt.Sprintf("hit %s %d", q.Text, i),
				URL:     fmt.Sprintf("%s/%s-%d", base, q.Text, i),
				Snippet: strings.Repeat("limiting factor ", 40),
			})
		}
		return results, nil
	}
	fetchOff := false
	artifact, err := client.SourceCheck(context.Background(), SourceCheckRequest{
		Claim:        "limiting factor",
		Queries:      []string{"a", "b", "c"},
		NumResults:   MaxNumResults,
		FetchContent: &fetchOff,
	})
	if err != nil {
		t.Fatalf("SourceCheck: %v", err)
	}
	if len(artifact.Sources) != MaxCheckSources {
		t.Errorf("sources = %d, want the cap %d", len(artifact.Sources), MaxCheckSources)
	}
	if artifact.OmittedSources != 10 {
		t.Errorf("omittedSources = %d, want 10 (30 hits, 20 listed)", artifact.OmittedSources)
	}
	// The inline passage budget holds, and the artifact says it was cut.
	total := 0
	for _, passage := range artifact.Passages {
		total += utf8RuneCount(passage.Text)
	}
	if total > CheckBudgetRunes {
		t.Errorf("inline passages = %d runes, budget %d", total, CheckBudgetRunes)
	}
	if len(artifact.Passages) == artifact.TotalPassages && artifact.TotalPassages > 0 {
		// This fixture has one long paraphrase per source, so a 4k budget must cut.
		t.Logf("nothing was cut: %d passages", artifact.TotalPassages)
	}

	// Query cap.
	tooMany := make([]string, MaxCheckQueries+1)
	for i := range tooMany {
		tooMany[i] = fmt.Sprintf("q%d", i)
	}
	if _, err := client.SourceCheck(context.Background(), SourceCheckRequest{Claim: "x", Queries: tooMany}); err == nil || !strings.Contains(err.Error(), "too many queries") {
		t.Errorf("query cap: %v", err)
	}
	// Claim is required.
	if _, err := client.SourceCheck(context.Background(), SourceCheckRequest{}); err == nil || !strings.Contains(err.Error(), "claim is required") {
		t.Errorf("empty claim: %v", err)
	}
	// Mode-like validation.
	if _, err := client.SourceCheck(context.Background(), SourceCheckRequest{Claim: "x", Recency: "decade"}); err == nil || !strings.Contains(err.Error(), "recencyFilter") {
		t.Errorf("recency: %v", err)
	}
}
