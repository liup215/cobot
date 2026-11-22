package transcript

// context.go — the pi footer rule over stored FACTS:
//
//	"uses last assistant usage when available, then estimates tokens for
//	 trailing messages"
//
// The measurement is taken over the REBUILT transcript (see Load), not over
// the raw rows: a compaction drops the summarized middle from the view the
// model ever sees again, so counting those rows would report the session as
// permanently full. The baseline is the newest row — among the rows the view
// keeps — carrying a provider usage stamp; its input already measured the
// whole request (system prompt, tool table and the post-compaction
// transcript). Rows after that request are estimated.
//
// No fact at all (a provider that does not report stream usage, or a legacy
// session before its first request) → estimate the rebuilt view, and
// actual=false marks the ≈ prefix in the UI.

import (
	"log/slog"

	cobottypes "github.com/liup215/cobot/types"
)

// ContextUsage reports the session's context occupancy in tokens, measured
// over the rebuilt (compaction-aware) transcript. The model window is NOT
// owned here (catalog/config territory) — callers pair this number with their
// own budget for display and compaction decisions.
func (s *Store) ContextUsage(sessionID string) (used int, actual bool) {
	if err := s.ensure(); err != nil {
		return 0, false
	}

	// The rows the caller would actually send: protected prefix + virtual
	// summary + tail. Rows dropped by a compaction never count.
	msgs, seqs, err := s.LoadWithSeqs(sessionID)
	if err != nil {
		slog.Warn("transcript: context usage load failed", "session", sessionID, "err", err)
		return 0, false
	}
	if len(msgs) == 0 {
		return 0, false
	}

	// Usage facts by seq. Only rows present in the rebuilt view can serve as
	// a baseline: a fact inside the summarized middle measured a request that
	// no longer exists.
	facts := make(map[int]tokenPair, len(msgs))
	rows, qerr := s.db.Query(`SELECT seq, usage_input, usage_output FROM messages
		WHERE session_id = ? AND (usage_input > 0 OR usage_output > 0)`, sessionID)
	if qerr != nil {
		slog.Warn("transcript: usage facts query failed", "session", sessionID, "err", qerr)
	} else {
		for rows.Next() {
			var seq, in, out int
			if serr := rows.Scan(&seq, &in, &out); serr != nil {
				break
			}
			facts[seq] = tokenPair{in: in, out: out}
		}
		rows.Close()
	}

	// Newest visible row carrying a fact = the baseline.
	base := -1
	for i := len(seqs) - 1; i >= 0; i-- {
		if seqs[i] < 0 {
			continue // the virtual summary row is never stored
		}
		if _, ok := facts[seqs[i]]; ok {
			base = i
			break
		}
	}
	if base >= 0 {
		f := facts[seqs[base]]
		used = f.in + f.out
		for i := base + 1; i < len(msgs); i++ {
			used += cobottypes.MessageTokens(msgs[i])
		}
		return used, true
	}

	// No fact for any visible row: estimate the rebuilt view.
	for _, m := range msgs {
		used += cobottypes.MessageTokens(m)
	}
	return used, false
}
