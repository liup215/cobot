package compact

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/liup215/cobot/types"
)

// Combo runs a primary strategy and falls back to a secondary one. It is the
// default cobot compaction: Summarize first (keeps the task, the decisions
// and the file list alive in condensed form), and Trim when summarization is
// impossible for any reason — no summarizer configured, provider error,
// empty summary, timeout.
//
// Fallback rules:
//
//   - Primary runs only when its own ShouldCompact says so; a primary error
//     never aborts the run, it triggers the fallback.
//   - The fallback runs when the primary failed or when the conversation is
//     still above the fallback's watermark after a successful primary run
//     (e.g. the retained rounds alone exceed the low watermark).
//   - If both fail, the combo returns a joined error; the loop logs it and
//     keeps going — the pre-send hard trim remains the safety net.
//   - A fallback that only SHRANK content in place (Trim's hard trim) leaves
//     the primary's message shape intact, so the primary's anchorable result
//     is what the host must persist: discarding it there was a persistence
//     hole (a summarized session reloaded as the full transcript).
//   - Once the primary has rewritten the conversation, the fallback's own
//     Result indices address the POST-primary shape; they must never be
//     persisted against the caller's pre-compaction snapshot (see the
//     coordinate-space note inside Compact), so the primary's result wins.
type Combo struct {
	// Primary runs first (typically *Summarize).
	Primary Strategy

	// Fallback runs when the primary fails or leaves the conversation above
	// its watermark (typically *Trim).
	Fallback Strategy
}

// NewCombo returns a Combo strategy with the given primary and fallback.
func NewCombo(primary, fallback Strategy) *Combo {
	return &Combo{Primary: primary, Fallback: fallback}
}

// ShouldCompact reports whether either strategy wants to compact.
func (c *Combo) ShouldCompact(conv *types.Conversation) bool {
	if c == nil || conv == nil {
		return false
	}
	if c.Primary != nil && c.Primary.ShouldCompact(conv) {
		return true
	}
	return c.Fallback != nil && c.Fallback.ShouldCompact(conv)
}

// Compact runs the primary strategy and falls back to the secondary one on
// failure or when the fallback watermark is still exceeded.
func (c *Combo) Compact(ctx context.Context, conv *types.Conversation) (Result, error) {
	if c == nil || conv == nil {
		return Result{}, nil
	}

	var primaryErr error
	var primaryResult Result
	if c.Primary != nil && c.Primary.ShouldCompact(conv) {
		res, err := c.Primary.Compact(ctx, conv)
		if err != nil {
			primaryErr = err
			slog.Warn("compact: primary strategy failed; falling back", "strategy", fmt.Sprintf("%T", c.Primary), "err", err)
		} else {
			primaryResult = res
		}
	}

	if c.Fallback == nil {
		return primaryResult, primaryErr
	}
	if primaryErr == nil && !c.Fallback.ShouldCompact(conv) {
		// The primary already brought the conversation below the fallback's
		// watermark — its (anchorable) result describes the final state.
		return primaryResult, nil
	}
	// The fallback now runs on top of (or instead of) the primary.
	//
	// CRITICAL: Result indices are pre-compaction indices — the host persists
	// them against the transcript it snapshotted BEFORE this Compact call.
	// Once the primary has REPLACED the conversation (it dropped rounds and
	// inserted a summary row), the fallback sees a different, shorter shape,
	// so its KeptFromIndex addresses the POST-primary transcript. Persisting
	// that number against the pre-compaction snapshot lands the anchor far
	// too early and the restored transcript brings back everything the
	// summarize had just dropped — on the desktop this reads as "compaction
	// completed, then the whole history came back".
	//
	// So: as soon as the primary rewrote the conversation, the primary's
	// anchorable result is the ONE result whose indices are in pre-compaction
	// coordinates. Whatever extra rounds the fallback drops afterwards are a
	// further optimization that is simply not persisted (the same safe
	// status quo as Trim's non-anchorable passes: the host keeps what it has
	// and the rows return on reload). Only when the primary did not touch the
	// conversation does the fallback's shape match the snapshot.
	res, err := c.Fallback.Compact(ctx, conv)
	if err != nil {
		if primaryErr != nil {
			return Result{}, fmt.Errorf("compact: primary failed (%v) and fallback failed: %w", primaryErr, err)
		}
		return Result{}, err
	}
	if primaryErr == nil && primaryResult.Anchorable {
		return primaryResult, nil
	}
	return res, nil
}
