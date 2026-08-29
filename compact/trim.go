package compact

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/liup215/cobot/types"
)

// Trim is the gline watermark compaction ported to the Strategy interface.
// It never calls the LLM: it trades context for headroom by dropping the
// oldest conversation rounds.
//
// Behavior (gline baseline plus the round/keep guarantees cobot pins down):
//
//   - triggers above the low watermark (60% of MaxTokens by default);
//   - keeps the leading system message and the first user message, so the
//     original task survives every compaction;
//   - keeps the most recent KeepRounds rounds verbatim — the current working
//     state is never cut;
//   - drops whole rounds oldest-first and stops as soon as the conversation
//     is back below the low watermark (below the watermark nothing is
//     touched at all);
//   - leaves a notice message describing what was dropped;
//   - if the conversation is still above the high watermark afterwards (a
//     single huge protected round), falls back to the gline hard trim
//     (Conversation.TrimToMaxTokens), which shrinks oversized tool results
//     to placeholders instead of deleting them silently.
type Trim struct {
	// LowWatermarkPercent triggers compaction above this percentage of
	// MaxTokens. 0 = DefaultLowWatermarkPercent.
	LowWatermarkPercent int

	// HighWatermarkPercent enables the hard-trim fallback above this
	// percentage of MaxTokens. 0 = DefaultHighWatermarkPercent.
	HighWatermarkPercent int

	// KeepRounds is the number of most-recent rounds that are never
	// trimmed. 0 = DefaultKeepRounds.
	KeepRounds int
}

// NewTrim returns a Trim strategy with the default watermarks.
func NewTrim() *Trim { return &Trim{} }

// trimNoticeFormat is the message left behind after rounds are dropped. It is
// a user-role message so it stays inside the message flow on both wire
// formats: the Anthropic Messages API has no system role inside messages (a
// system-role notice would be hoisted into the top-level system prompt), and
// consecutive user turns are merged by the API.
const trimNoticeFormat = "[Context compacted: %d oldest round(s) (%d messages) were removed to fit the token budget. " +
	"Tool outputs from those rounds are gone; re-run a tool if you need its result again.]"

// lowWatermark resolves the configured low watermark percentage.
func (t *Trim) lowWatermark() int {
	if t.LowWatermarkPercent <= 0 {
		return DefaultLowWatermarkPercent
	}
	return t.LowWatermarkPercent
}

// highWatermark resolves the configured high watermark percentage.
func (t *Trim) highWatermark() int {
	if t.HighWatermarkPercent <= 0 {
		return DefaultHighWatermarkPercent
	}
	return t.HighWatermarkPercent
}

// keepRounds resolves the configured number of protected recent rounds.
func (t *Trim) keepRounds() int {
	if t.KeepRounds <= 0 {
		return DefaultKeepRounds
	}
	return t.KeepRounds
}

// ShouldCompact reports whether the conversation is above the low watermark.
func (t *Trim) ShouldCompact(conv *types.Conversation) bool {
	if conv == nil || conv.MaxTokens <= 0 {
		return false
	}
	return conv.IsTokenAboveThreshold(t.lowWatermark())
}

// Compact drops the oldest rounds until the conversation is back below the
// low watermark, then applies the gline hard trim if that was not enough.
func (t *Trim) Compact(ctx context.Context, conv *types.Conversation) error {
	if conv == nil || conv.MaxTokens <= 0 {
		return nil
	}
	if conv.GetTotalTokens() <= watermark(conv, t.lowWatermark()) {
		return nil // below the watermark: nothing is touched
	}

	// Each pass drops leading rounds; the pass is repeated while the
	// conversation is still above the low watermark, because the notice
	// message itself costs tokens and may eat the freed budget. A pass that
	// frees nothing ends the loop.
	for {
		before := conv.GetTotalTokens()
		if before <= watermark(conv, t.lowWatermark()) {
			break
		}
		if !t.dropOldestRounds(conv) {
			break // nothing compactable left: only the hard trim can help
		}
		if conv.GetTotalTokens() >= before {
			break // no progress possible: only the hard trim can help
		}
	}

	return t.hardTrimIfStillOver(conv)
}

// dropOldestRounds drops as many leading unprotected rounds as needed to get
// the conversation back below the low watermark. It reports whether anything
// was dropped.
func (t *Trim) dropOldestRounds(conv *types.Conversation) bool {
	sys := systemPrefix(conv)
	body := conv.Messages[sys:]
	rounds := SplitRounds(body)

	keep := t.keepRounds()
	if len(rounds) <= keep {
		return false
	}
	compactable := rounds[:len(rounds)-keep]

	// The first user message (the original task) is preserved even when it
	// sits inside a dropped round.
	firstUser := firstUserMessage(body)
	head := conv.Messages[:sys]

	// candidate builds the message list that would result from dropping
	// body[0:cut] (minus the protected task message) and leaving a notice for
	// the dropped counts, together with its exact token cost — so the
	// watermark comparison accounts for the notice this compaction leaves
	// behind.
	candidate := func(cut, droppedRounds, droppedMessages int) ([]types.Message, int) {
		msgs := make([]types.Message, 0, len(conv.Messages))
		msgs = append(msgs, head...)
		for i := 0; i < cut; i++ {
			if i == firstUser {
				msgs = append(msgs, body[i])
			}
		}
		msgs = append(msgs, types.Message{
			Role:    types.RoleUser,
			Content: fmt.Sprintf(trimNoticeFormat, droppedRounds, droppedMessages),
		})
		msgs = append(msgs, body[cut:]...)
		tokens := 0
		for _, m := range msgs {
			tokens += types.MessageTokens(m)
		}
		return msgs, tokens
	}

	// Drop leading rounds oldest-first, stopping as soon as the projected
	// total is back below the low watermark.
	low := watermark(conv, t.lowWatermark())
	current := 0
	for _, m := range conv.Messages {
		current += types.MessageTokens(m)
	}
	msgs, projected := candidate(0, 0, 0)
	cut, droppedRounds, droppedMessages := 0, 0, 0
	for _, r := range compactable {
		cut = r.End
		droppedRounds++
		for i := r.Start; i < r.End; i++ {
			if i == firstUser {
				continue // the task message survives
			}
			droppedMessages++
		}
		msgs, projected = candidate(cut, droppedRounds, droppedMessages)
		if projected <= low {
			break
		}
	}

	// Nothing gained (e.g. only the protected task message sat in the dropped
	// rounds): leave the conversation alone.
	if droppedRounds == 0 || projected >= current {
		return false
	}

	conv.ReplaceMessages(msgs)
	slog.Info("compact: trim dropped oldest rounds",
		"rounds", droppedRounds, "messages", droppedMessages, "tokens", projected)
	return true
}

// hardTrimIfStillOver applies the ported gline hard trim when the
// conversation is still above the high watermark after round dropping.
func (t *Trim) hardTrimIfStillOver(conv *types.Conversation) error {
	if conv.GetTotalTokens() > watermark(conv, t.highWatermark()) {
		conv.TrimToMaxTokens()
	}
	return nil
}
