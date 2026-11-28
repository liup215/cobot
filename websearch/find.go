package websearch

import (
	"fmt"
	"strings"
	"unicode"
)

// FindMode selects how findText matches.
type FindMode string

const (
	// FindExact matches the query's bytes as written.
	FindExact FindMode = "exact"
	// FindCaseInsensitive is the default: case-folded literal matching.
	FindCaseInsensitive FindMode = "case-insensitive"
	// FindFuzzy matches at paragraph granularity, tolerating typos and
	// inflection ("limiting factors" finds "limiting factor").
	FindFuzzy FindMode = "fuzzy"
)

// ParseFindMode normalizes the findMode parameter ("" = case-insensitive).
func ParseFindMode(value string) (FindMode, error) {
	switch FindMode(strings.ToLower(strings.TrimSpace(value))) {
	case "", FindCaseInsensitive:
		return FindCaseInsensitive, nil
	case FindExact:
		return FindExact, nil
	case FindFuzzy:
		return FindFuzzy, nil
	default:
		return "", fmt.Errorf("findMode must be exact, case-insensitive or fuzzy (got %q)", value)
	}
}

// Passage is one located match with its surrounding context.
type Passage struct {
	// Match is the query string this passage was found for.
	Match string `json:"match"`
	// Text is the context around the match, cut at FindContextRunes per side.
	Text string `json:"text"`
	// Offset is the match's RUNE offset in the stored content, so the caller
	// can page to it with get_search_content({offset}) if the passage is not
	// enough.
	Offset int `json:"offset"`
}

// FindResult is the get_search_content answer for a findText call.
type FindResult struct {
	Passages        []Passage `json:"passages"`
	TotalMatches    int       `json:"totalMatches"`
	ReturnedMatches int       `json:"returnedMatches"`
	Truncated       bool      `json:"truncated,omitempty"`
}

type span struct {
	start int // byte offset
	end   int // byte offset (exclusive)
	match string
}

// Find locates terms in content and returns bounded passages in document order.
//
// Why passages and not "the matching page": a 20KB page whose only relevant
// sentence is 300 characters in means the model either reads the whole page
// (context) or nothing (accuracy). Context windows of ±400 runes around each
// match are enough to judge a claim, and the caller can still page to
// Passage.Offset when it wants more.
func Find(content string, terms []string, mode FindMode, budgetRunes int) FindResult {
	if budgetRunes <= 0 {
		budgetRunes = MaxPageRunes
	}

	spans := make([]span, 0, 8)
	seen := make(map[string]bool, len(terms))
	for _, term := range terms {
		term = strings.TrimSpace(term)
		if term == "" {
			continue
		}
		key := strings.ToLower(term)
		if seen[key] {
			continue
		}
		seen[key] = true

		switch mode {
		case FindExact:
			spans = append(spans, literalSpans(content, term, false)...)
		case FindFuzzy:
			spans = append(spans, fuzzySpans(content, term)...)
		default:
			spans = append(spans, literalSpans(content, term, true)...)
		}
	}

	merged := mergeSpans(content, spans)
	result := FindResult{TotalMatches: len(spans)}

	used := 0
	for _, sp := range merged {
		if len(result.Passages) >= MaxFindPassages {
			result.Truncated = true
			break
		}
		text, offset := contextWindow(content, sp)
		cost := utf8RuneCount(text)
		if used+cost > budgetRunes {
			result.Truncated = true
			if len(result.Passages) > 0 {
				break
			}
			// Nothing fits yet: a first passage must still return something.
			text, _ = truncateRunes(text, budgetRunes)
			cost = utf8RuneCount(text)
		}
		used += cost
		result.Passages = append(result.Passages, Passage{Match: sp.match, Text: text, Offset: offset})
	}

	result.ReturnedMatches = len(result.Passages)
	if result.ReturnedMatches < result.TotalMatches {
		result.Truncated = true
	}
	if result.Passages == nil {
		result.Passages = []Passage{}
	}
	return result
}

// literalSpans scans for a literal needle.
func literalSpans(content, needle string, foldCase bool) []span {
	haystack, target := content, needle
	if foldCase {
		haystack = strings.ToLower(content)
		target = strings.ToLower(needle)
	}
	if target == "" {
		return nil
	}

	spans := make([]span, 0, 4)
	step := len(target)
	if step == 0 {
		step = 1
	}
	for at := 0; at < len(haystack); {
		index := strings.Index(haystack[at:], target)
		if index < 0 {
			break
		}
		start := at + index
		spans = append(spans, span{start: start, end: start + len(target), match: needle})
		at = start + step
	}
	return spans
}

// fuzzySpans matches paragraph by paragraph, the way pi's finder does: a
// paragraph qualifies when enough of the query's tokens appear in it, each
// within a length-scaled edit distance.
//
// CJK exception: a Chinese/Japanese query is one whitespace-free run, so token
// edit distance cannot find its substrings. For those the literal containment
// test is both cheaper and more accurate, so it is used instead.
func fuzzySpans(content, needle string) []span {
	tokens := queryTokens(needle)
	if len(tokens) == 0 || isUnspacedCJK(needle) {
		return literalSpans(content, needle, true)
	}

	required := len(tokens)
	if required > 1 {
		required = (required*3 + 4) / 5 // ceil(60%)
	}

	spans := make([]span, 0, 4)
	for _, paragraph := range paragraphs(content) {
		words := paragraphWords(content, paragraph)
		if len(words) == 0 {
			continue
		}
		matched := 0
		firstStart := -1
		for _, token := range tokens {
			found := -1
			for _, word := range words {
				if withinEditDistance(token, strings.ToLower(word.text), maxEdits(len(token))) {
					found = word.start
					break
				}
			}
			if found < 0 {
				continue
			}
			matched++
			if firstStart < 0 || found < firstStart {
				firstStart = found
			}
		}
		if matched < required || firstStart < 0 {
			continue
		}
		spans = append(spans, span{start: firstStart, end: firstStart + 1, match: needle})
	}
	return spans
}

type byteRange struct{ start, end int }

// paragraphs returns the paragraph spans of content: runs of non-empty lines.
func paragraphs(content string) []byteRange {
	ranges := make([]byteRange, 0, 8)
	start := -1
	lineStart := 0
	for i := 0; i <= len(content); i++ {
		if i < len(content) && content[i] != '\n' {
			continue
		}
		line := content[lineStart:i]
		blank := strings.TrimSpace(line) == ""
		if blank && start >= 0 {
			ranges = append(ranges, byteRange{start: start, end: lineStart})
			start = -1
		} else if !blank && start < 0 {
			start = lineStart
		}
		lineStart = i + 1
	}
	if start >= 0 {
		ranges = append(ranges, byteRange{start: start, end: len(content)})
	}
	return ranges
}

type wordSpan struct {
	text  string
	start int
}

// paragraphWords tokenizes a paragraph, keeping each word's byte offset.
func paragraphWords(content string, paragraph byteRange) []wordSpan {
	words := make([]wordSpan, 0, 32)
	text := content[paragraph.start:paragraph.end]
	start := -1
	for i, r := range text {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			if start < 0 {
				start = i
			}
			continue
		}
		if start >= 0 {
			words = append(words, wordSpan{text: text[start:i], start: paragraph.start + start})
			start = -1
		}
	}
	if start >= 0 {
		words = append(words, wordSpan{text: text[start:], start: paragraph.start + start})
	}
	return words
}

// queryTokens splits a query into lowercase word tokens.
func queryTokens(query string) []string {
	tokens := make([]string, 0, 4)
	start := -1
	for i, r := range query {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			if start < 0 {
				start = i
			}
			continue
		}
		if start >= 0 {
			tokens = append(tokens, strings.ToLower(query[start:i]))
			start = -1
		}
	}
	if start >= 0 {
		tokens = append(tokens, strings.ToLower(query[start:]))
	}
	// Single-character tokens match almost anything; keep only meaningful ones.
	kept := tokens[:0]
	for _, token := range tokens {
		if utf8RuneCount(token) >= 2 {
			kept = append(kept, token)
		}
	}
	return kept
}

// isUnspacedCJK reports whether the needle is a whitespace-free non-ASCII run
// (Chinese, Japanese, Korean text without spaces).
func isUnspacedCJK(s string) bool {
	if strings.ContainsAny(s, " \t\n") {
		return false
	}
	nonASCII := 0
	runes := 0
	for _, r := range s {
		runes++
		if r > unicode.MaxASCII {
			nonASCII++
		}
	}
	return runes > 0 && nonASCII*2 > runes
}

// maxEdits is the tolerated edit distance for a token of this length: none for
// short tokens (where a single edit changes the word), one from five runes, two
// from nine.
func maxEdits(length int) int {
	switch {
	case length >= 9:
		return 2
	case length >= 5:
		return 1
	default:
		return 0
	}
}

// withinEditDistance is a bounded Levenshtein check: it gives up as soon as a
// whole row exceeds the budget, so comparing a long token to thousands of words
// stays linear.
func withinEditDistance(left, right string, maximum int) bool {
	if maximum == 0 {
		return left == right
	}
	l, r := []rune(left), []rune(right)
	if abs(len(l)-len(r)) > maximum {
		return false
	}
	previous := make([]int, len(r)+1)
	current := make([]int, len(r)+1)
	for j := range previous {
		previous[j] = j
	}
	for i := 1; i <= len(l); i++ {
		current[0] = i
		rowMin := i
		for j := 1; j <= len(r); j++ {
			cost := 1
			if l[i-1] == r[j-1] {
				cost = 0
			}
			current[j] = min(previous[j]+1, current[j-1]+1, previous[j-1]+cost)
			if current[j] < rowMin {
				rowMin = current[j]
			}
		}
		if rowMin > maximum {
			return false
		}
		previous, current = current, previous
	}
	return previous[len(r)] <= maximum
}

func abs(v int) int {
	if v < 0 {
		return -v
	}
	return v
}

// mergeSpans expands each match to its context window, merges overlaps and
// returns them in document order (pi's finder does the same; without it, two
// adjacent matches return the same paragraph twice).
func mergeSpans(content string, spans []span) []span {
	if len(spans) == 0 {
		return nil
	}
	expanded := make([]span, 0, len(spans))
	for _, sp := range spans {
		start, end := expandToContext(content, sp.start, sp.end)
		expanded = append(expanded, span{start: start, end: end, match: sp.match})
	}

	sortSpans(expanded)
	merged := make([]span, 0, len(expanded))
	for _, sp := range expanded {
		if n := len(merged); n > 0 && sp.start <= merged[n-1].end {
			if sp.end > merged[n-1].end {
				merged[n-1].end = sp.end
			}
			continue
		}
		merged = append(merged, sp)
	}
	return merged
}

func sortSpans(spans []span) {
	for i := 1; i < len(spans); i++ {
		for j := i; j > 0 && spans[j].start < spans[j-1].start; j-- {
			spans[j], spans[j-1] = spans[j-1], spans[j]
		}
	}
}

// expandToContext widens a byte range by FindContextRunes on each side, without
// splitting a rune.
func expandToContext(content string, start, end int) (int, int) {
	forward := 0
	for i := end; i < len(content) && forward < FindContextRunes; i++ {
		if isRuneStart(content, i) {
			forward++
		}
		end = i
	}
	backward := 0
	for i := start; i > 0 && backward < FindContextRunes; i-- {
		if isRuneStart(content, i) {
			backward++
		}
		start = i
	}
	return start, end
}

func isRuneStart(s string, i int) bool {
	if i >= len(s) {
		return false
	}
	return s[i]&0xC0 != 0x80
}

// contextWindow renders one span plus its rune offset in the content.
func contextWindow(content string, sp span) (string, int) {
	text := collapse(content[sp.start:sp.end])
	offset := utf8RuneCount(content[:sp.start])
	if utf8RuneCount(text) > 2*FindContextRunes+200 {
		text, _ = truncateRunes(text, 2*FindContextRunes+200)
	}
	return text, offset
}

// SliceContent pages through a stored content string.
//
// offsets are RUNES. Bytes would page a Chinese page at a third of the size of
// an English one for no reason, and every caller here is a model reading text,
// not a byte-oriented protocol.
func SliceContent(content string, offset, limit int) (page string, total, next int) {
	runes := []rune(content)
	total = len(runes)
	if offset < 0 {
		offset = 0
	}
	if limit <= 0 || limit > MaxPageRunes {
		limit = DefaultPageRunes
	}
	if offset > total {
		offset = total
	}
	end := offset + limit
	if end > total {
		end = total
	}
	page = string(runes[offset:end])
	if end < total {
		next = end
	}
	return page, total, next
}
