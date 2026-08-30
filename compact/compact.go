// Package compact provides conversation compaction strategies for long agent
// runs.
//
// A Strategy watches a token watermark and rewrites the conversation in place
// so it fits the context budget again. Two strategies ship with cobot:
//
//   - Trim: the gline watermark compaction, ported to the Strategy interface.
//     It never calls the LLM. It drops the oldest rounds — always keeping the
//     system message, the first user message (the original task) and the most
//     recent KeepRounds rounds — until the conversation is back below the low
//     watermark, then leaves a notice telling the model what happened.
//   - Summarize: LLM summarization (the lossless-structure alternative). The
//     rounds that would be dropped are rendered into a transcript and handed
//     to a Summarizer — typically one non-streaming call over the session's
//     own provider — and replaced by a single structured summary message.
//
// NewCombo chains both and is what loop and the root Config use by default:
// summarize first, and fall back to Trim on any failure (provider error,
// empty summary, timeout, no summarizer configured).
//
// Compaction units are rounds, not single messages: dropping or summarizing
// whole rounds keeps every assistant tool_call paired with its tool result,
// which both the OpenAI-compatible and the Anthropic wire formats require.
package compact

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/liup215/cobot/types"
)

// Defaults shared by the built-in strategies. All of them can be overridden
// per strategy instance through the exported fields.
const (
	// DefaultKeepRounds is the number of most-recent conversation rounds the
	// built-in strategies keep verbatim. Maestro-style long tasks rely on
	// the latest working state being intact, so recent rounds are never
	// trimmed or summarized away.
	DefaultKeepRounds = 6

	// DefaultLowWatermarkPercent triggers compaction once the conversation
	// exceeds 60% of MaxTokens (the loop's budget-check hook).
	DefaultLowWatermarkPercent = 60

	// DefaultHighWatermarkPercent is the hard line (80% of MaxTokens) past
	// which Trim also applies the gline hard trim as a last resort.
	DefaultHighWatermarkPercent = 80

	// DefaultSummarizeTimeout bounds a single Summarizer call.
	DefaultSummarizeTimeout = 120 * time.Second

	// DefaultMaxSummaryTokens caps the summary text a Summarizer may return.
	DefaultMaxSummaryTokens = 2000

	// DefaultMaxTranscriptTokens caps the transcript handed to the
	// Summarizer, so a very large dropped history cannot exceed the
	// summarizer's own context window.
	DefaultMaxTranscriptTokens = 24000
)

// Sentinel errors reported by the built-in strategies. A Combo treats any
// error from its primary strategy as "fall back to the next strategy".
var (
	// ErrNoSummarizer is returned by Summarize when no Summarizer is set.
	ErrNoSummarizer = errors.New("compact: no summarizer configured")

	// ErrEmptySummary is returned when the Summarizer returned nothing.
	ErrEmptySummary = errors.New("compact: summarizer returned an empty summary")
)

// Strategy compacts a conversation when it outgrows its token budget.
//
// Implementations rewrite the conversation in place and must leave it
// structurally valid: a leading system message stays first, tool results stay
// adjacent to the assistant turn that requested them, and the result can be
// sent to a provider again.
//
// Both methods must tolerate being called repeatedly: a second Compact on an
// already-compacted conversation is a no-op. Errors mean "this strategy did
// not apply"; callers (see Combo) treat them as a signal to fall back rather
// than as a run failure.
type Strategy interface {
	// ShouldCompact reports whether conv is currently past this strategy's
	// trigger watermark and compaction would change something.
	ShouldCompact(conv *types.Conversation) bool

	// Compact rewrites conv in place to fit the token budget.
	Compact(ctx context.Context, conv *types.Conversation) error
}

// Round is one conversational turn: a user message together with everything
// the assistant produced in response to it (assistant messages and their tool
// results), up to the next user message.
type Round struct {
	// Start is the index of the round's first message in the slice that was
	// split (not necessarily in the conversation, see SplitRounds).
	Start int

	// End is one past the index of the round's last message.
	End int
}

// Len returns the number of messages in the round.
func (r Round) Len() int { return r.End - r.Start }

// SplitRounds groups messages into rounds. Every user message starts a new
// round; messages before the first user message form a round of their own; a
// leading system message belongs to no round at all.
func SplitRounds(msgs []types.Message) []Round {
	var rounds []Round
	start := -1
	for i := range msgs {
		role := msgs[i].Role
		if start < 0 && role == types.RoleSystem {
			// Leading system message: belongs to no round.
			continue
		}
		if start < 0 || role == types.RoleUser {
			if start >= 0 {
				rounds = append(rounds, Round{Start: start, End: i})
			}
			start = i
		}
	}
	if start >= 0 {
		rounds = append(rounds, Round{Start: start, End: len(msgs)})
	}
	return rounds
}

// systemPrefix returns the number of leading system messages (0 or 1) that
// must survive every compaction untouched.
func systemPrefix(conv *types.Conversation) int {
	if conv.HasSystemPrompt() {
		return 1
	}
	return 0
}

// firstUserMessage returns the index of the first user message in msgs, or -1.
// That message carries the original task and survives every compaction.
func firstUserMessage(msgs []types.Message) int {
	for i := range msgs {
		if msgs[i].Role == types.RoleUser {
			return i
		}
	}
	return -1
}

// watermark computes the token threshold at the given percentage of MaxTokens.
func watermark(conv *types.Conversation, percent int) int {
	if conv.MaxTokens <= 0 || percent <= 0 {
		return 0
	}
	return conv.MaxTokens * percent / 100
}

// truncateRunes returns at most max runes of s, appending an omission marker
// when it cut something.
func truncateRunes(s string, max int) string {
	if max <= 0 {
		return ""
	}
	runes := []rune(s)
	if len(runes) <= max {
		return s
	}
	return string(runes[:max]) + "\n[... truncated ...]"
}

// indent prefixes every line of s with pad so transcripts stay readable.
func indent(s, pad string) string {
	if s == "" {
		return ""
	}
	lines := strings.Split(s, "\n")
	for i, line := range lines {
		lines[i] = pad + line
	}
	return strings.Join(lines, "\n")
}
