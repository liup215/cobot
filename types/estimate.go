package types

import "unicode/utf8"

// TokenEstimator estimates the number of tokens contained in s.
type TokenEstimator func(s string) int

// Estimator is the package-level token estimator used by Conversation and
// every token-budget calculation in cobot. It defaults to
// HeuristicEstimator; callers may replace it with a model-specific exact
// counter (e.g. a bundled tokenizer) before running an agent.
//
// Estimator is safe to swap between runs but must not be changed while an
// agent loop is in flight.
var Estimator TokenEstimator = HeuristicEstimator

// HeuristicEstimator gives a conservative per-rune token estimate that
// accounts for whitespace, punctuation, and mixed-script text:
//
//   - consecutive ASCII word characters cost roughly ceil(len/3) tokens,
//   - whitespace is merged with adjacent tokens,
//   - non-ASCII runes (CJK, emoji, ...) cost ~1 token per rune.
func HeuristicEstimator(s string) int {
	if s == "" {
		return 0
	}
	total := 0
	wordRun := 0  // consecutive non-space ASCII characters
	nonASCII := 0 // non-ASCII runes
	for i := 0; i < len(s); {
		r, size := utf8.DecodeRuneInString(s[i:])
		if r < 128 {
			if r == ' ' || r == '\t' || r == '\n' || r == '\r' {
				if wordRun > 0 {
					total += (wordRun + 2) / 3
					wordRun = 0
				}
			} else {
				wordRun++
			}
		} else {
			if wordRun > 0 {
				total += (wordRun + 2) / 3
				wordRun = 0
			}
			nonASCII++
		}
		i += size
	}
	if wordRun > 0 {
		total += (wordRun + 2) / 3
	}
	return total + nonASCII
}

// estimateTokens routes through the active Estimator.
func estimateTokens(s string) int {
	if Estimator == nil {
		return HeuristicEstimator(s)
	}
	return Estimator(s)
}
