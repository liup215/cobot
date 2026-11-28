package websearch

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"unicode"
)

// source_check: gather EVIDENCE for a claim, and deliberately refuse to judge it.
//
// This is the one tool whose name overpromises, so the contract states it plainly:
//
//	It returns ranked sources and quoted passages with offsets and hashes.
//	It never returns "supported" or "contradicted".
//
// Why the refusal is structural rather than a matter of model discipline: the
// passage selection below is a WORD-OVERLAP heuristic, not entailment. A sentence
// that shares vocabulary with a claim can support it, contradict it, or be about
// something else entirely — "the syllabus no longer includes X" and "the syllabus
// still includes X" overlap almost completely. Any status this code could compute
// would therefore be a coin flip wearing a label, and a label is exactly what a
// reader would trust. So there are two statuses and no others:
//
//	missing-evidence  nothing was retrieved; the claim is unchecked
//	unclear           passages were retrieved; a human must read them
//
// The statuses live in ONE place (assess below) so "add supported later" would have
// to be a visible, deliberate edit to this comment and that function.
//
// The evidence itself is built to be checkable rather than persuasive: every page
// passage carries its RUNE offset in the stored text and a sha256 of the passage, so
// a reader can jump to it (get_search_content) and verify the quote; snippets the
// search provider generated are marked OriginProviderSummary, because a paraphrase
// around a citation is not page text and must never be quoted as if it were.

// Status values an artifact can carry. There is no "supported"/"contradicted" —
// see the file comment.
const (
	// StatusMissingEvidence: nothing was retrieved.
	StatusMissingEvidence = "missing-evidence"
	// StatusUnclear: passages were retrieved and a human must review them.
	StatusUnclear = "unclear"
)

// Passage origins.
const (
	// OriginPage marks a passage quoted from the page text we fetched.
	OriginPage = "page"
	// OriginProviderSummary marks the search provider's own summary. It is shown
	// (it is why the source matched) but flagged, because it is a paraphrase.
	OriginProviderSummary = "provider_summary"
)

// Source quality buckets. Kept coarse on purpose: the point is "does this look like
// an authority on the subject", not a numeric credibility score nobody can audit.
const (
	QualityExamBoard = "exam-board"
	QualityOfficial  = "official"
	QualityAcademic  = "academic"
	QualityReference = "reference"
	QualityNews      = "news"
	QualityCommunity = "community"
	QualityUnknown   = "unknown"
)

// Caps for source_check.
const (
	// MaxCheckQueries bounds the query fan-out (pi uses 8).
	MaxCheckQueries = 8
	// MaxCheckSources bounds the ranked source list.
	MaxCheckSources = 20
	// MaxCheckFetches is how many top sources get read in full. Three keeps the call
	// in the same time budget as one fetch_content while still giving real quotes.
	MaxCheckFetches = 3
	// MaxPassagesPerSource is how many sentences per source become evidence.
	MaxPassagesPerSource = 3
	// InlinePassageRunes / CheckBudgetRunes bound the model-facing artifact.
	InlinePassageRunes = 400
	CheckBudgetRunes   = 4_000
	// minSentenceRunes drops headings, table rules and one-word fragments. Kept low
	// because CJK is denser: 12 Chinese characters is already a full clause, so a
	// higher floor would throw away exactly the sentences a Chinese page offers.
	minSentenceRunes = 12
)

// SourceCheckRequest mirrors the source_check schema.
type SourceCheckRequest struct {
	Claim        string   `json:"claim"`
	Queries      []string `json:"queries"`
	NumResults   int      `json:"numResults"`
	FetchContent *bool    `json:"fetchContent"`
	Recency      string   `json:"recencyFilter"`
	Domains      []string `json:"domainFilter"`
}

// ResearchSource is one ranked source in the artifact.
type ResearchSource struct {
	Rank    int    `json:"rank"`
	URL     string `json:"url"`
	Title   string `json:"title"`
	Domain  string `json:"domain"`
	Quality string `json:"quality"`
	// Fetched reports whether the page itself was read (and therefore whether page
	// passages exist for it).
	Fetched bool `json:"fetched"`
	// Snippet is the provider's excerpt, kept because it explains why the source
	// matched. Its Origin says whether it is page text or a provider paraphrase.
	Snippet       string `json:"snippet,omitempty"`
	SnippetOrigin string `json:"snippetOrigin,omitempty"`
	// Error explains why a source could not be read (fetch failed, blocked, …).
	Error string `json:"error,omitempty"`
}

// ResearchPassage is one quoted piece of evidence.
type ResearchPassage struct {
	ID         string `json:"passageId"`
	SourceRank int    `json:"sourceRank"`
	URL        string `json:"url"`
	Text       string `json:"text"`
	Origin     string `json:"origin"`
	// Start/End are RUNE offsets in the stored page text (page passages only), so a
	// reader can open the same page at the same place.
	Start int `json:"start,omitempty"`
	End   int `json:"end,omitempty"`
	// Hash is sha256 of the FULL passage text (see Truncated: an inline copy may be
	// cut, and the hash always describes the stored original).
	Hash string `json:"hash"`
	// Truncated marks an inline copy that was cut to InlinePassageRunes; the stored
	// artifact holds the whole passage.
	Truncated bool `json:"truncated,omitempty"`
}

// Artifact is the source_check result.
type Artifact struct {
	Claim string `json:"claim"`
	// Status is one of StatusMissingEvidence / StatusUnclear, and nothing else.
	Status string `json:"status"`
	// Rationale says why, in words a reader can act on.
	Rationale string `json:"rationale"`
	// Providers names the search backends that answered.
	Providers []string          `json:"providers,omitempty"`
	Sources   []ResearchSource  `json:"sources"`
	Passages  []ResearchPassage `json:"passages"`
	// TotalPassages counts every passage found, including ones the inline budget
	// dropped; OmittedSources counts sources past MaxCheckSources.
	TotalPassages  int  `json:"totalPassages"`
	OmittedSources int  `json:"omittedSources,omitempty"`
	Truncated      bool `json:"truncated,omitempty"`
	// Checks lists the concrete things a reader should verify — written by this code,
	// not by a model, so the artifact carries no opinion.
	Checks []string `json:"checks"`
	// ResponseID pages the FULL artifact (get_search_content), empty when nothing
	// was retrieved.
	ResponseID string `json:"responseId,omitempty"`
	Hint       string `json:"hint,omitempty"`
}

// SourceCheck gathers evidence for a claim.
func (c *Client) SourceCheck(ctx context.Context, req SourceCheckRequest) (*Artifact, error) {
	claim := strings.TrimSpace(req.Claim)
	if claim == "" {
		return nil, fmt.Errorf("a claim is required (the statement to gather evidence about)")
	}

	queries, err := c.checkQueries(claim, req)
	if err != nil {
		return nil, err
	}
	chain, err := c.chainFor("")
	if err != nil {
		return nil, err
	}

	outcomes, err := c.searchOutcomes(ctx, queries, chain)
	if err != nil {
		return nil, err
	}

	sources, omitted := collectSources(outcomes, req.NumResults)
	artifact := &Artifact{Claim: claim, Sources: sources, Providers: usedProviders(outcomes), OmittedSources: omitted}

	fetched := map[string]StoredURL{}
	storeDocs := make([]StoredURL, 0, MaxCheckFetches)
	if wantFetch(req.FetchContent) && len(sources) > 0 {
		for _, doc := range c.fetchForCheck(ctx, sources) {
			fetched[doc.URL] = doc
			if doc.Error == "" && doc.Content != "" {
				storeDocs = append(storeDocs, doc)
			}
			// Record WHY a page has no page passages, on the source itself: an
			// unexplained gap looks like "the page said nothing about it".
			for i := range artifact.Sources {
				if artifact.Sources[i].URL != doc.URL {
					continue
				}
				artifact.Sources[i].Fetched = doc.Error == "" && doc.Content != ""
				if doc.Error != "" {
					artifact.Sources[i].Error = doc.Error
				}
			}
		}
	}

	artifact.Passages, artifact.TotalPassages = buildPassages(artifact.Sources, fetched, claim)
	artifact.Status, artifact.Rationale = assess(artifact.TotalPassages)
	artifact.Checks = checkList(claim, artifact)

	// The stored artifact is complete; the returned one is bounded and pageable. A
	// retrieval that produced no evidence gets NO id: an id pointing at an empty
	// artifact invites paging after nothing, and "nothing to page" is the honest
	// answer there.
	if artifact.TotalPassages > 0 {
		artifact.ResponseID = c.storeArtifact(artifact, storeDocs)
		artifact.Hint = checkHint(artifact.ResponseID, artifact)
	}
	artifact.Passages, artifact.Truncated = boundPassages(artifact.Passages)
	return artifact, nil
}

// wantFetch defaults to TRUE: without page text every passage would be a provider
// paraphrase, and a paraphrase with no offset is not evidence a reader can check.
func wantFetch(requested *bool) bool {
	if requested == nil {
		return true
	}
	return *requested
}

// checkQueries normalizes queries (default: the claim itself) and the search knobs.
func (c *Client) checkQueries(claim string, req SourceCheckRequest) ([]Query, error) {
	raw := make([]string, 0, 1+len(req.Queries))
	for _, q := range req.Queries {
		if s := strings.TrimSpace(q); s != "" {
			raw = append(raw, s)
		}
	}
	if len(raw) == 0 {
		raw = append(raw, claim)
	}
	if len(raw) > MaxCheckQueries {
		return nil, fmt.Errorf("too many queries: %d (max %d)", len(raw), MaxCheckQueries)
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

	numResults := req.NumResults
	if numResults <= 0 {
		numResults = DefaultNumResults
	}
	if numResults > c.maxResults {
		numResults = c.maxResults
	}

	queries := make([]Query, 0, len(raw))
	seen := make(map[string]bool, len(raw))
	for _, text := range raw {
		key := strings.ToLower(text)
		if seen[key] {
			continue
		}
		seen[key] = true
		queries = append(queries, Query{Text: text, NumResults: numResults, Recency: recency, Domains: domains})
	}
	return queries, nil
}

// collectSources de-duplicates every query's hits by URL, in first-seen order, and
// caps the list. Deduplication matters here more than in web_search: the same
// syllabus page surfacing for four queries must not become four "independent sources".
func collectSources(outcomes []QueryOutcome, numResults int) ([]ResearchSource, int) {
	seen := make(map[string]bool, len(outcomes)*4)
	sources := make([]ResearchSource, 0, MaxCheckSources)
	omitted := 0
	for _, outcome := range outcomes {
		limit := outcome.Results
		if numResults > 0 && len(limit) > numResults {
			limit = limit[:numResults]
		}
		for _, result := range limit {
			key := strings.TrimSpace(result.URL)
			if key == "" || seen[key] {
				continue
			}
			seen[key] = true
			if len(sources) >= MaxCheckSources {
				omitted++
				continue
			}
			sources = append(sources, ResearchSource{
				Rank:    len(sources) + 1,
				URL:     key,
				Title:   strings.TrimSpace(result.Title),
				Domain:  hostLabel(key),
				Quality: classifySource(key),
				Snippet: strings.TrimSpace(result.Snippet),
				// Every snippet that reaches here came from the search provider, not
				// from a page we read.
				SnippetOrigin: OriginProviderSummary,
			})
		}
	}
	return sources, omitted
}

// fetchForCheck reads the top sources in full, reusing the guarded fetch pipeline.
func (c *Client) fetchForCheck(ctx context.Context, sources []ResearchSource) []StoredURL {
	limit := MaxCheckFetches
	if len(sources) < limit {
		limit = len(sources)
	}
	docs := make([]StoredURL, limit)
	sem := make(chan struct{}, c.concurrency)
	done := make(chan struct{}, limit)
	for i := 0; i < limit; i++ {
		go func(i int) {
			defer func() { done <- struct{}{} }()
			sem <- struct{}{}
			defer func() { <-sem }()
			docs[i] = c.fetchOne(ctx, sources[i].URL, FetchModeReadable)
		}(i)
	}
	for i := 0; i < limit; i++ {
		<-done
	}
	return docs
}

// buildPassages produces the evidence list: page sentences for fetched sources, the
// provider snippet otherwise. Returns the passages and the TOTAL found before the
// inline budget.
func buildPassages(sources []ResearchSource, fetched map[string]StoredURL, claim string) ([]ResearchPassage, int) {
	terms := claimTerms(claim)
	passages := make([]ResearchPassage, 0, len(sources)*2)

	for _, source := range sources {
		doc, havePage := fetched[source.URL]
		havePage = havePage && doc.Error == "" && strings.TrimSpace(doc.Content) != ""

		if havePage {
			sentences := splitSentences(doc.Content)
			ranked := rankSentences(sentences, terms, source.Snippet)
			for i, hit := range ranked {
				passages = append(passages, ResearchPassage{
					ID:         passageID(source.Rank, i+1),
					SourceRank: source.Rank,
					URL:        source.URL,
					Text:       hit.text,
					Origin:     OriginPage,
					Start:      hit.start,
					End:        hit.end,
					Hash:       hashText(hit.text),
				})
			}
		}

		if source.Snippet != "" {
			passages = append(passages, ResearchPassage{
				ID:         passageID(source.Rank, 0),
				SourceRank: source.Rank,
				URL:        source.URL,
				Text:       source.Snippet,
				Origin:     OriginProviderSummary,
				Hash:       hashText(source.Snippet),
			})
		}
	}
	return passages, len(passages)
}

// boundPassages trims the inline list to the documented budget: whole passages only,
// each cut at InlinePassageRunes, total at CheckBudgetRunes.
func boundPassages(passages []ResearchPassage) ([]ResearchPassage, bool) {
	out := make([]ResearchPassage, 0, len(passages))
	used, truncated := 0, false
	for _, passage := range passages {
		text, cut := truncateRunes(passage.Text, InlinePassageRunes)
		cost := utf8RuneCount(text) + 40 // + ids/urls/metadata
		if used+cost > CheckBudgetRunes {
			truncated = true
			continue
		}
		used += cost
		if cut {
			passage.Truncated = true
			// Hash stays over the full text (the stored copy), so a reader who pages the
			// artifact still gets the verifiable original.
			passage.Text = text + " …"
		}
		out = append(out, passage)
	}
	return out, truncated || len(out) < len(passages)
}

// assess is the ONLY place a status is produced.
//
// Note what it does not do: it never compares a passage to the claim's polarity, and
// it never guesses confidence. Adding a third status means editing this function and
// the file comment above, on purpose.
func assess(passageCount int) (status, rationale string) {
	if passageCount == 0 {
		return StatusMissingEvidence,
			"No passages were retrieved for this claim. It is unchecked — not refuted. Try different queries, or narrower ones."
	}
	return StatusUnclear,
		fmt.Sprintf("%d passage(s) were retrieved. Deciding whether they support or contradict the claim is yours: this tool selects by word overlap, which cannot tell agreement from disagreement (\"no longer includes X\" and \"still includes X\" overlap almost completely).", passageCount)
}

// checkList writes the verification steps into the artifact, so the reader is handed
// a procedure instead of an opinion.
func checkList(claim string, artifact *Artifact) []string {
	checks := []string{
		"Read each passage with its origin: «page» is text we fetched, «provider_summary» is the search engine's paraphrase and is not a quote.",
		"For page passages, open the same source at the given offset with get_search_content and confirm the sentence in context.",
	}
	if artifact.OmittedSources > 0 {
		checks = append(checks, fmt.Sprintf("%d further source(s) were found but not listed (cap %d) — widen the query if the claim needs more coverage.", artifact.OmittedSources, MaxCheckSources))
	}
	checks = append(checks, fmt.Sprintf("Confirm the claim's own wording against the passages (%q): check numbers, dates, exam-board names and negation.", claim))
	return checks
}

// storeArtifact keeps the COMPLETE artifact (all passages, untruncated) AND the pages
// its page-passages quote, so every offset in the artifact can be opened. Returns the
// responseId, or "" when the artifact could not be encoded.
func (c *Client) storeArtifact(artifact *Artifact, fetched []StoredURL) string {
	full := *artifact
	full.Passages = append([]ResearchPassage(nil), artifact.Passages...)
	// No self-reference: the stored copy must not claim an id it already had.
	full.ResponseID = ""
	full.Hint = ""
	encoded, err := json.MarshalIndent(full, "", "  ")
	if err != nil {
		return ""
	}
	return c.store.PutResearch(artifact.Claim, string(encoded), fetched)
}

// checkHint tells the reader where the rest is, and — importantly — what this tool
// did NOT do.
func checkHint(id string, artifact *Artifact) string {
	if id == "" {
		return ""
	}
	return fmt.Sprintf(
		`The complete evidence set (%d passage(s), %d source(s)) is stored as responseId %q — page it with get_search_content({"responseId": %q, "limit": %d}). This artifact contains evidence only; it does not assert whether the claim is true.`,
		artifact.TotalPassages, len(artifact.Sources), id, id, DefaultPageRunes,
	)
}

func passageID(sourceRank, index int) string {
	return fmt.Sprintf("p-%d-%d", sourceRank, index)
}

// hashText is the passage hash: "sha256:<hex>".
func hashText(text string) string {
	sum := sha256.Sum256([]byte(text))
	return "sha256:" + hex.EncodeToString(sum[:])
}

// --- sentence selection ----------------------------------------------------

type scoredSentence struct {
	text  string
	start int // rune offset in the page content
	end   int
	score int
}

// splitSentences cuts a page into candidate passages: one per line, and long lines
// further at sentence terminators. Rune offsets point into the ORIGINAL content so a
// reader can page to the same place.
func splitSentences(content string) []scoredSentence {
	runes := []rune(content)
	out := make([]scoredSentence, 0, 64)
	lineStart := 0
	for i := 0; i <= len(runes); i++ {
		if i < len(runes) && runes[i] != '\n' {
			// Long lines are split further, at sentence terminators.
			if isSentenceEnd(runes[i]) && i+1 < len(runes) && (runes[i+1] == ' ' || runes[i+1] == '\t') {
				out = appendSentence(out, runes, lineStart, i+1)
				lineStart = i + 1
			}
			continue
		}
		out = appendSentence(out, runes, lineStart, i)
		lineStart = i + 1
	}
	return out
}

func appendSentence(out []scoredSentence, runes []rune, start, end int) []scoredSentence {
	if start >= end {
		return out
	}
	text := strings.TrimSpace(string(runes[start:end]))
	// Trim in the rune space so the offsets keep pointing at the same characters.
	for start < end && unicode.IsSpace(runes[start]) {
		start++
	}
	for end > start && unicode.IsSpace(runes[end-1]) {
		end--
	}
	if utf8RuneCount(text) < minSentenceRunes {
		return out
	}
	if isNoiseLine(text) {
		return out
	}
	return append(out, scoredSentence{text: text, start: start, end: end})
}

// isSentenceEnd covers the terminators of both writing systems.
func isSentenceEnd(r rune) bool {
	switch r {
	case '.', '!', '?', ';', '。', '！', '？', '；':
		return true
	}
	return false
}

// isNoiseLine drops markdown furniture that would otherwise become "evidence".
func isNoiseLine(text string) bool {
	trimmed := strings.TrimSpace(text)
	if trimmed == "" {
		return true
	}
	switch {
	case strings.HasPrefix(trimmed, "```"), strings.HasPrefix(trimmed, "---"):
		return true
	case strings.HasPrefix(trimmed, "|") && strings.HasSuffix(trimmed, "|"):
		// A table row is kept: it often holds exactly the number being checked.
		return false
	case strings.Trim(trimmed, "-|: ") == "":
		return true
	}
	return false
}

// rankSentences keeps the sentences that share the most with the claim, then with the
// provider snippet (which is why the source matched at all), and returns at most
// MaxPassagesPerSource of them.
func rankSentences(sentences []scoredSentence, claimTerms []string, snippet string) []scoredSentence {
	snippetTerms := claimTermsFrom(snippet)
	keep := make([]scoredSentence, 0, MaxPassagesPerSource)
	for _, sentence := range sentences {
		lower := strings.ToLower(sentence.text)
		sentence.score = overlap(claimTerms, lower)*10 + overlap(snippetTerms, lower)
		if sentence.score == 0 {
			continue
		}
		keep = append(keep, sentence)
	}
	sort.SliceStable(keep, func(i, j int) bool {
		if keep[i].score != keep[j].score {
			return keep[i].score > keep[j].score
		}
		return keep[i].start < keep[j].start
	})
	if len(keep) > MaxPassagesPerSource {
		keep = keep[:MaxPassagesPerSource]
	}
	return keep
}

// claimTermsFrom extracts comparison tokens: latin words of 3+ runes that are not
// stopwords, any bare number, and CJK bigrams (Chinese has no spaces, so a whole
// sentence would otherwise be one token).
func claimTermsFrom(text string) []string {
	lower := strings.ToLower(text)
	terms := make([]string, 0, 16)
	seen := make(map[string]bool, 16)
	add := func(term string) {
		if term == "" || seen[term] {
			return
		}
		seen[term] = true
		terms = append(terms, term)
	}

	var word strings.Builder
	var cjk strings.Builder
	flushWord := func() {
		if word.Len() == 0 {
			return
		}
		token := word.String()
		word.Reset()
		if isAllDigits(token) {
			add(token)
			return
		}
		if utf8RuneCount(token) >= 3 && !stopWords[token] {
			add(token)
		}
	}
	flushCJK := func() {
		if cjk.Len() == 0 {
			return
		}
		runes := []rune(cjk.String())
		cjk.Reset()
		if len(runes) < 2 {
			return
		}
		for i := 0; i+1 < len(runes); i++ {
			add(string(runes[i : i+2]))
		}
	}

	for _, r := range lower {
		switch {
		case unicode.Is(unicode.Han, r) || unicode.Is(unicode.Hiragana, r) || unicode.Is(unicode.Katakana, r):
			flushWord()
			cjk.WriteRune(r)
		case unicode.IsLetter(r) || unicode.IsDigit(r):
			flushCJK()
			word.WriteRune(r)
		default:
			flushWord()
			flushCJK()
		}
	}
	flushWord()
	flushCJK()
	return terms
}

// stopWords are the English words that appear in every sentence, so keeping them
// would make every sentence look relevant. "not"/"no" are deliberately absent:
// negation is exactly what a reader must notice, even though this scorer does not
// interpret it.
var stopWords = map[string]bool{
	"the": true, "and": true, "for": true, "with": true, "that": true,
	"this": true, "from": true, "are": true, "was": true, "were": true,
	"has": true, "have": true, "had": true, "into": true, "its": true,
	"their": true, "than": true, "then": true, "when": true, "which": true,
	"what": true, "who": true, "why": true, "how": true, "all": true,
	"any": true, "can": true, "may": true, "must": true, "should": true,
	"would": true, "could": true, "will": true, "shall": true, "does": true,
	"did": true, "done": true, "being": true, "been": true, "these": true,
	"those": true, "such": true, "some": true, "more": true, "most": true,
	"only": true, "over": true, "under": true, "about": true, "after": true,
	"before": true, "between": true, "during": true, "other": true, "there": true,
	"where": true, "also": true, "but": true, "you": true, "your": true,
}

// claimTerms is claimTermsFrom for the claim itself. Numbers and short acronyms count
// ("429", "API"), which is why digits are not subject to the 3-rune rule.
func claimTerms(claim string) []string { return claimTermsFrom(claim) }

func overlap(terms []string, lowerText string) int {
	hits := 0
	for _, term := range terms {
		if strings.Contains(lowerText, term) {
			hits++
		}
	}
	return hits
}

func isAllDigits(value string) bool {
	if value == "" {
		return false
	}
	for _, r := range value {
		if !unicode.IsDigit(r) {
			return false
		}
	}
	return true
}

// classifySource buckets a URL by authority. Deliberately coarse: a numeric
// "credibility score" would look precise and mean nothing.
//
// Order matters: exam boards first (for an A-level product they ARE the authority and
// they are not .gov/.edu), then named academic hosts, then government/education
// suffixes, then reference works, news, and finally user-generated sites.
func classifySource(rawURL string) string {
	host := strings.ToLower(hostLabel(rawURL))
	host = strings.TrimPrefix(host, "www.")

	matches := func(patterns []string) bool {
		for _, pattern := range patterns {
			if host == pattern || strings.HasSuffix(host, "."+pattern) {
				return true
			}
			if strings.HasPrefix(pattern, ".") && (host == strings.TrimPrefix(pattern, ".") || strings.HasSuffix(host, pattern)) {
				return true
			}
		}
		return false
	}

	if matches([]string{
		"cambridgeinternational.org", "aqa.org.uk", "ocr.org.uk", "pearson.com",
		"edexcel.com", "ibo.org", "collegeboard.org", "eduqas.co.uk", "wjec.co.uk",
		"ccea.org.uk", "qualifications.pearson.com",
	}) {
		return QualityExamBoard
	}
	if matches([]string{
		"nature.com", "science.org", "sciencedirect.com", "springer.com", "wiley.com",
		"tandfonline.com", "jstor.org", "pubmed.ncbi.nlm.nih.gov", "ncbi.nlm.nih.gov",
		"biorxiv.org", "arxiv.org", "frontiersin.org", "plos.org", "cell.com",
	}) || strings.HasSuffix(host, ".edu") || strings.HasSuffix(host, ".ac.uk") || strings.HasSuffix(host, ".edu.cn") {
		return QualityAcademic
	}
	if matches([]string{".gov", ".gov.uk", ".gov.cn", ".mil", "who.int", "un.org", "europa.eu"}) {
		return QualityOfficial
	}
	if matches([]string{"wikipedia.org", "britannica.com", "khanacademy.org", "openstax.org"}) {
		return QualityReference
	}
	if matches([]string{"bbc.co.uk", "bbc.com", "reuters.com", "apnews.com", "theguardian.com", "nytimes.com", "xinhuanet.com", "people.com.cn"}) {
		return QualityNews
	}
	if matches([]string{"reddit.com", "quora.com", "zhihu.com", "stackexchange.com", "stackoverflow.com", "baike.baidu.com", "weibo.com", "medium.com"}) {
		return QualityCommunity
	}
	return QualityUnknown
}
