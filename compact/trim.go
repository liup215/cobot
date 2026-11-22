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
//   - triggers above the low watermark (60% of MaxTokens by default) AND only
//     while at least one round sits outside the protected window — a
//     conversation whose every round is recent cannot be helped by dropping
//     rounds, so a 60–80% conversation is left alone instead of being
//     "compacted" (no-op) on every loop iteration;
//   - keeps the leading system message and the first user message, so the
//     original task survives every compaction;
//   - keeps the most recent KeepRounds rounds verbatim — the current working
//     state is never cut;
//   - drops whole rounds oldest-first and stops as soon as the conversation
//     is back below the low watermark (below the watermark nothing is
//     touched at all), accounting for the notice it leaves behind;
//   - leaves a notice message describing what was dropped;
//   - if the conversation is still above the high watermark afterwards (a
//     single huge protected round), falls back to the gline hard trim
//     (Conversation.TrimToMaxTokens), which shrinks oversized tool results
//     to placeholders instead of deleting them silently.
//
// The result is ANCHORABLE: Trim's final shape is the same three-part shape
// Summarize produces — protected prefix + one notice row + a verbatim tail —
// so a host can persist {notice as summary, tail start as anchor} and reload
// the compacted view instead of the full transcript. Without that entry a
// reload restores every dropped round and the next run compacts again (the
// "compaction never reduces the session" treadmill).
type Trim struct {
	// LowWatermarkPercent triggers compaction above this percentage of
	// MaxTokens. 0 = DefaultLowWatermarkPercent.
	LowWatermarkPercent int

	// HighWatermarkPercent enables the hard-trim fallback above this
	// percentage of MaxTokens. 0 = DefaultHighWatermarkPercent.
	HighWatermarkPercent int

	// KeepRounds is the legacy count-based window: the number of most-recent
	// rounds that are never trimmed when KeepRecentTokens is 0.
	// 0 = DefaultKeepRounds.
	KeepRounds int

	// KeepRecentTokens is the token-sized verbatim window (pi's
	// keepRecentTokens): the newest rounds are kept until they accumulate
	// this budget, everything older is dropped. 0 = DefaultKeepRecentTokens.
	KeepRecentTokens int
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

// keepRecentTokens resolves the configured token-sized window.
func (t *Trim) keepRecentTokens() int {
	if t.KeepRecentTokens <= 0 {
		return DefaultKeepRecentTokens
	}
	return t.KeepRecentTokens
}

// ShouldCompact reports whether the conversation is above the low watermark
// AND there is something this strategy could actually do: at least one round
// outside the protected window, or — above the high watermark — the hard trim.
//
// The second half matters for loop behavior, not just for tidiness: a
// conversation made of a few enormous recent rounds stays above 60% forever,
// and reporting "compactable" there made the loop run a strategy that frees
// nothing on every iteration (a no-op "context compacted" notice per turn
// while the token count never moved).
func (t *Trim) ShouldCompact(conv *types.Conversation) bool {
	if conv == nil || conv.MaxTokens <= 0 {
		return false
	}
	if !conv.IsTokenAboveThreshold(t.lowWatermark()) {
		return false
	}
	if conv.GetTotalTokens() > watermark(conv, t.highWatermark()) {
		return true // the hard trim always has work above the high watermark
	}
	return t.hasCompactableRound(conv)
}

// hasCompactableRound reports whether dropping rounds would actually free
// tokens — not merely whether rounds exist outside the keep window. The
// projection is the same one Compact applies (including the notice it leaves
// behind), so the trigger and the action can never disagree: a conversation
// whose only droppable round is the tiny task turn is not "compactable", and
// the loop does not announce a compaction that changes nothing.
//
// The keep window is budget-aware (see keepWindow): when the newest rounds
// alone exceed the low watermark the window shrinks instead of pinning the
// conversation above it forever.
func (t *Trim) hasCompactableRound(conv *types.Conversation) bool {
	_, ok := t.planDrop(conv)
	return ok
}

// Compact drops the oldest rounds until the conversation is back below the
// low watermark, then applies the gline hard trim if that was not enough. The
// returned Result describes the persisted shape (see the type doc).
func (t *Trim) Compact(ctx context.Context, conv *types.Conversation) (Result, error) {
	if conv == nil || conv.MaxTokens <= 0 {
		return Result{}, nil
	}
	if conv.GetTotalTokens() <= watermark(conv, t.lowWatermark()) {
		return Result{}, nil // below the watermark: nothing is touched
	}

	res := t.dropOldestRounds(conv)
	// The hard trim rewrites message CONTENT in place (it never drops rows),
	// so the anchor computed above still describes the message shape.
	if err := t.hardTrimIfStillOver(conv); err != nil {
		return res, err
	}
	return res, nil
}

// trimPlan is a computed round drop: the message list to install plus the
// counts used for the notice and the persisted shape.
type trimPlan struct {
	body            []types.Message
	cut             int
	droppedRounds   int
	droppedMessages int
	msgs            []types.Message
	projected       int
}

// planDrop computes the round drop Compact would apply, without touching the
// conversation. ok=false means there is nothing to gain (no unprotected round,
// or the drop would not shrink the transcript).
func (t *Trim) planDrop(conv *types.Conversation) (trimPlan, bool) {
	sys := systemPrefix(conv)
	body := conv.Messages[sys:]
	rounds := SplitRounds(body)

	keep := keepWindow(body, rounds, t.keepRounds(), t.keepRecentTokens(), watermark(conv, t.lowWatermark()))
	if keep == 0 || len(rounds) <= keep {
		return trimPlan{}, false // only the hard trim can help
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

	// Drop the whole window that sits outside the keep budget, oldest-first.
	//
	// The cut is the keep boundary itself, not "whatever it takes to get back
	// under the low watermark": stopping at the last round needed leaves the
	// session sitting at the watermark, which is exactly what the next
	// pre-send check trips again. pi cuts at its keepRecentTokens boundary for
	// the same reason — one compaction should buy many turns, not one.
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
	}
	msgs, projected = candidate(cut, droppedRounds, droppedMessages)
	if projected > low {
		slog.Debug("compact: trim window alone is still above the low watermark",
			"projected", projected, "low", low)
	}

	// Nothing gained (e.g. only the protected task message sat in the dropped
	// rounds): leave the conversation alone.
	if droppedRounds == 0 || projected >= current {
		return trimPlan{}, false
	}
	return trimPlan{
		body:            body,
		cut:             cut,
		droppedRounds:   droppedRounds,
		droppedMessages: droppedMessages,
		msgs:            msgs,
		projected:       projected,
	}, true
}

// dropOldestRounds applies the plan and reports the resulting shape.
func (t *Trim) dropOldestRounds(conv *types.Conversation) Result {
	plan, ok := t.planDrop(conv)
	if !ok {
		return Result{}
	}
	preLen := len(conv.Messages) // the shape the result's indices address
	conv.ReplaceMessages(plan.msgs)
	slog.Info("compact: trim dropped oldest rounds",
		"rounds", plan.droppedRounds, "messages", plan.droppedMessages, "tokens", plan.projected)

	// The persisted shape: [system?] [task (when it was dropped)] [notice]
	// [kept tail]. Anchor = the last dropped row, i.e. the row right before
	// the tail starts.
	sys := systemPrefix(conv)
	firstUser := firstUserMessage(plan.body)
	prefix := make([]int, 0, sys+1)
	for i := 0; i < sys; i++ {
		prefix = append(prefix, i)
	}
	if firstUser >= 0 && firstUser < plan.cut {
		prefix = append(prefix, sys+firstUser)
	}
	keptFrom := sys + plan.cut
	anchorable := keptFrom > 0 && !containsIndex(prefix, keptFrom-1)
	return Result{
		Summary:           fmt.Sprintf(trimNoticeFormat, plan.droppedRounds, plan.droppedMessages),
		SummaryRole:       types.RoleUser,
		KeptFromIndex:     keptFrom,
		KeptPrefixIndices: prefix,
		Anchorable:        anchorable,
		PreLen:            preLen,
	}
}

// containsIndex reports whether idx is in list.
func containsIndex(list []int, idx int) bool {
	for _, v := range list {
		if v == idx {
			return true
		}
	}
	return false
}

// hardTrimIfStillOver applies the ported gline hard trim when the
// conversation is still above the high watermark after round dropping.
func (t *Trim) hardTrimIfStillOver(conv *types.Conversation) error {
	if conv.GetTotalTokens() > watermark(conv, t.highWatermark()) {
		conv.TrimToMaxTokens()
	}
	return nil
}
