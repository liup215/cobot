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

	// DefaultKeepRecentTokens is the size (in tokens) of the verbatim window
	// the built-in strategies keep when they compact — the pi
	// `keepRecentTokens` knob. The newest rounds are kept until they
	// accumulate this budget, and everything older is summarized/trimmed.
	//
	// It is a FIXED budget, deliberately not a fraction of the context
	// window: a compaction sized as "keep up to the watermark" leaves the
	// session exactly as full as it found it, so the next pre-send check
	// trips again, compacts again, and frees nothing — while the model keeps
	// carrying a working set that is the whole window. pi keeps ~20k of
	// verbatim recent turns and frees the rest on every compaction; that is
	// the behaviour this default reproduces.
	DefaultKeepRecentTokens = 20000

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

	// Compact rewrites conv in place to fit the token budget and reports
	// what it did (see Result).
	Compact(ctx context.Context, conv *types.Conversation) (Result, error)
}

// LowWatermarkPercent reports the trigger watermark a strategy compacts at, so
// the loop's pre-send budget check and the strategy can never disagree about
// when compaction starts (a host that asks for 80% must not be pre-gated at
// the 60% default). A Combo reports the EARLIEST watermark among its parts —
// the loop still AND-s this cheap check with ShouldCompact, so returning the
// minimum can never fire a compaction the strategy did not want.
// Nil, unknown and custom strategies report DefaultLowWatermarkPercent.
func LowWatermarkPercent(s Strategy) int {
	switch v := s.(type) {
	case *Summarize:
		if v != nil {
			return v.lowWatermark()
		}
	case *Trim:
		if v != nil {
			return v.lowWatermark()
		}
	case *Combo:
		if v != nil {
			lo := 0
			for _, sub := range []Strategy{v.Primary, v.Fallback} {
				if sub == nil {
					continue
				}
				if p := LowWatermarkPercent(sub); lo == 0 || p < lo {
					lo = p
				}
			}
			if lo > 0 {
				return lo
			}
		}
	}
	return DefaultLowWatermarkPercent
}

// Result reports what one compaction did — the typed alternative to making
// hosts reverse-engineer the outcome from message diffs.
type Result struct {
	// Summary is the exact content of the inserted summary row: what a
	// durable compaction entry must store to rebuild the transcript.
	Summary string
	// SummaryRole is that row's role (Summarize emits user).
	SummaryRole types.Role
	// KeptFromIndex is the PRE-compaction conversation index where the kept
	// tail begins. Rows [0, KeptFromIndex) were dropped, modulo the
	// protected prefix below.
	KeptFromIndex int
	// KeptPrefixIndices are PRE-compaction indices of protected rows kept
	// verbatim from BEFORE the cut (leading system rows, the original
	// task). A single anchor cannot express their non-contiguous keep —
	// a persisting host needs these to rebuild.
	KeptPrefixIndices []int
	// Anchorable reports whether this result can be persisted as a
	// {summary, kept-after, prefix} entry. Trim's interleaved survivors and
	// no-op runs are NOT anchorable — the host keeps the full transcript
	// and re-compacts on the next load (the safe status quo).
	Anchorable bool
	// PreLen is the message count of the transcript the indices above
	// address (the PRE-compaction conversation this strategy was handed).
	// Hosts persist anchors resolved against their own pre-compaction
	// snapshot, so a result whose PreLen does not match that snapshot's
	// length cannot be persisted — a mismatch means the indices point into
	// a different (shorter) shape and would anchor far too early, restoring
	// everything the compaction dropped.
	PreLen int
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

// keepWindow returns how many of the most-recent rounds a strategy keeps
// verbatim. It is the stricter of two independent windows:
//
//   - the count window (keepRounds, the legacy knob): at most N recent
//     rounds, and
//   - the token window (keepTokens, pi's keepRecentTokens): walk backwards
//     from the newest round accumulating estimated tokens and stop once the
//     window has grown to the budget.
//
// Taking the minimum is what makes a compaction actually free the session.
// With the count window alone, six tool-heavy rounds can be the whole context
// window: the strategy then has nothing it is allowed to drop, leaves the
// session exactly as full as it found it, and the next pre-send check trips
// again — the "it keeps compacting and nothing changes" treadmill. pi sizes
// its kept window in TOKENS (~20k) rather than in rounds for exactly this
// reason.
//
// Two invariants survive both modes:
//   - at least one round is kept (an empty window would delete the current
//     working state), and
//   - at most len(rounds)-1 rounds are kept, so a conversation that is over
//     budget always has something left to drop.
//
// limit is the hard ceiling (normally the low watermark): the window is
// shrunk further while the newest rounds alone exceed it, which keeps a
// single enormous round from pinning the session above the watermark.
func keepWindow(body []types.Message, rounds []Round, keepRounds, keepTokens, limit int) int {
	if len(rounds) == 0 {
		return keepRounds
	}
	last := len(rounds) - 1
	if last < 1 {
		return 0 // a single round: nothing can be dropped
	}

	keep := keepRounds
	if keep > last {
		keep = last
	} else if keep < 1 {
		keep = 1
	}

	if keepTokens > 0 {
		// pi-style walk-back: how many of the newest rounds it takes to
		// accumulate the budget (always at least one, never all of them).
		tokenKeep := 0
		acc := 0
		for tokenKeep < last {
			r := rounds[len(rounds)-1-tokenKeep]
			for _, m := range body[r.Start:r.End] {
				acc += types.MessageTokens(m)
			}
			tokenKeep++
			if acc >= keepTokens {
				break
			}
		}
		if tokenKeep < keep {
			keep = tokenKeep
		}
	}

	for keep > 1 {
		kept := 0
		for _, m := range body[rounds[len(rounds)-keep].Start:] {
			kept += types.MessageTokens(m)
		}
		if limit <= 0 || kept <= limit {
			break
		}
		keep--
	}
	return keep
}

// isCompactionNotice reports whether a message is the row an earlier
// compaction left behind — the carried-forward summary (Summarize's
// "[COMPACTED] ..." header or Trim's "[Context compacted ...]" notice).
//
// Those rows sit at the FRONT of the transcript, so a second compaction of a
// long session would summarize them last (see renderTranscript) and could drop
// them from the summarizer's own budget — silently throwing away everything the
// previous compactions had distilled. pi keeps the previous summary in a
// <previous-summary> block and tells the model to preserve it; the strategies
// here must at least never truncate it away.
func isCompactionNotice(m types.Message) bool {
	if m.Role != types.RoleUser {
		return false
	}
	c := strings.TrimSpace(m.Content)
	return strings.HasPrefix(c, "[COMPACTED]") || strings.HasPrefix(c, "[Context compacted")
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
