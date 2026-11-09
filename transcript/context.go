package transcript

// context.go — the pi footer rule over stored FACTS:
//
//	"uses last assistant usage when available, then estimates tokens for
//	 trailing messages"
//
// The baseline is the newest row carrying a provider usage stamp; its input
// already measured the whole request (system prompt, tool table and — after
// a compaction — the post-compaction transcript), so compactions are
// reflected automatically. Rows produced after that request are estimated.
// No fact at all (a pre-usage legacy session before its first request) →
// estimate everything, actual=false marks the ≈ prefix in the UI.

import (
	"log/slog"

	cobottypes "github.com/liup215/cobot/types"
)

// ContextUsage reports the session's context occupancy in tokens. The model
// window is NOT owned here (catalog/config territory) — callers pair this
// number with their own budget for display and compaction decisions.
func (s *Store) ContextUsage(sessionID string) (used int, actual bool) {
	if err := s.ensure(); err != nil {
		return 0, false
	}

	// Newest usage fact = the baseline.
	var seq, in, out int
	err := s.db.QueryRow(`SELECT seq, usage_input, usage_output FROM messages
		WHERE session_id = ? AND (usage_input > 0 OR usage_output > 0)
		ORDER BY seq DESC LIMIT 1`, sessionID).Scan(&seq, &in, &out)
	if err == nil {
		used = in + out
		rows, qerr := s.db.Query(selectMessageCols+` FROM messages WHERE session_id = ? AND seq > ? ORDER BY seq ASC`,
			sessionID, seq)
		if qerr != nil {
			slog.Warn("transcript: trailing estimate failed", "session", sessionID, "err", qerr)
			return used, true
		}
		msgs, _, _, serr := scanMessages(rows)
		rows.Close()
		if serr != nil {
			return used, true
		}
		for _, m := range msgs {
			used += cobottypes.MessageTokens(m)
		}
		return used, true
	}

	// No facts yet: estimate the whole transcript.
	rows, err := s.db.Query(selectMessageCols+` FROM messages WHERE session_id = ? ORDER BY seq ASC`, sessionID)
	if err != nil {
		return 0, false
	}
	msgs, _, _, scanErr := scanMessages(rows)
	rows.Close()
	if scanErr != nil {
		return 0, false
	}
	for _, m := range msgs {
		used += cobottypes.MessageTokens(m)
	}
	return used, false
}
