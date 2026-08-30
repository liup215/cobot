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
func (c *Combo) Compact(ctx context.Context, conv *types.Conversation) error {
	if c == nil || conv == nil {
		return nil
	}

	var primaryErr error
	if c.Primary != nil && c.Primary.ShouldCompact(conv) {
		if err := c.Primary.Compact(ctx, conv); err != nil {
			primaryErr = err
			slog.Warn("compact: primary strategy failed; falling back", "strategy", fmt.Sprintf("%T", c.Primary), "err", err)
		}
	}

	if c.Fallback == nil {
		return primaryErr
	}
	if primaryErr == nil && !c.Fallback.ShouldCompact(conv) {
		// The primary already brought the conversation below the fallback's
		// watermark.
		return nil
	}
	if err := c.Fallback.Compact(ctx, conv); err != nil {
		if primaryErr != nil {
			return fmt.Errorf("compact: primary failed (%v) and fallback failed: %w", primaryErr, err)
		}
		return err
	}
	return nil
}
