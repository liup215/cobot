package transcript

// facts.go — immutable measurements and compaction entries (the pi model:
// facts are written once and never derived backwards).

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"time"

	cobottypes "github.com/liup215/cobot/types"
)

// StampUsage attaches a turn's usage FACT to the session's most recent
// assistant row — the row that closes the request the usage measured. Call
// timing guarantees "most recent" is the right turn (next stream start / run
// end, before the next assistant row exists); the queue's FIFO guarantees the
// row is already on disk.
func (s *Store) StampUsage(sessionID string, input, output int) error {
	if err := s.ensure(); err != nil {
		return err
	}
	var seq int
	if err := s.db.QueryRow(`SELECT seq FROM messages WHERE session_id = ? AND role = 'assistant'
		ORDER BY seq DESC LIMIT 1`, sessionID).Scan(&seq); err != nil {
		return fmt.Errorf("transcript: find turn assistant row: %w", err)
	}
	res, err := s.db.Exec(`UPDATE messages SET usage_input = ?, usage_output = ? WHERE session_id = ? AND seq = ?`,
		input, output, sessionID, seq)
	if err != nil {
		return fmt.Errorf("transcript: stamp usage: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("transcript: no assistant row at seq %d", seq)
	}
	return nil
}

// SaveCompaction upserts the session's compaction entry (latest wins — an
// older summary is absorbed into the new one, so the anchor only moves
// forward). Stored originals are never rewritten.
func (s *Store) SaveCompaction(c *Compaction) error {
	if err := s.ensure(); err != nil {
		return err
	}
	prefix := c.KeptPrefixSeqs
	if prefix == nil {
		prefix = []int{}
	}
	prefixJSON, err := json.Marshal(prefix)
	if err != nil {
		return fmt.Errorf("transcript: marshal prefix: %w", err)
	}
	created := c.CreatedAt
	if created.IsZero() {
		created = time.Now()
	}
	_, err = s.db.Exec(`INSERT INTO compactions(session_id, summary, summary_role, kept_after_seq, kept_prefix_seqs, created_at)
		VALUES(?,?,?,?,?,?)
		ON CONFLICT(session_id) DO UPDATE SET
			summary = excluded.summary,
			summary_role = excluded.summary_role,
			kept_after_seq = excluded.kept_after_seq,
			kept_prefix_seqs = excluded.kept_prefix_seqs,
			created_at = excluded.created_at`,
		c.SessionID, c.Summary, string(c.SummaryRole), c.KeptAfterSeq, string(prefixJSON), created.Format(time.RFC3339))
	if err != nil {
		return fmt.Errorf("transcript: save compaction: %w", err)
	}
	return nil
}

// LoadCompaction returns the session's compaction entry; (nil, nil) when the
// session was never compacted.
func (s *Store) LoadCompaction(id string) (*Compaction, error) {
	if err := s.ensure(); err != nil {
		return nil, err
	}
	var c Compaction
	var role, prefixJSON, created string
	err := s.db.QueryRow(`SELECT session_id, summary, summary_role, kept_after_seq, kept_prefix_seqs, created_at
		FROM compactions WHERE session_id = ?`, id).
		Scan(&c.SessionID, &c.Summary, &role, &c.KeptAfterSeq, &prefixJSON, &created)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("transcript: load compaction: %w", err)
	}
	c.SummaryRole = cobottypes.Role(role)
	c.CreatedAt, _ = time.Parse(time.RFC3339, created)
	if err := json.Unmarshal([]byte(prefixJSON), &c.KeptPrefixSeqs); err != nil {
		return nil, fmt.Errorf("transcript: parse prefix: %w", err)
	}
	return &c, nil
}

// ResolveSeq looks up the latest stored seq of an exact (role, content) row —
// fallback for in-flight rows whose seq the caller never observed (the queue
// assigns seq at flush time). Used by compaction anchoring and deliver-once
// stub rewrites.
func (s *Store) ResolveSeq(sessionID string, role cobottypes.Role, content string) (int, bool) {
	if err := s.ensure(); err != nil {
		return 0, false
	}
	var seq int
	err := s.db.QueryRow(`SELECT seq FROM messages WHERE session_id = ? AND role = ? AND content = ?
		ORDER BY seq DESC LIMIT 1`, sessionID, string(role), content).Scan(&seq)
	if err != nil {
		return 0, false
	}
	return seq, true
}

// UpdateMessage rewrites one row's content in place (deliver-once image
// stubbing: a targeted UPDATE leaves the append-only queue untouched).
func (s *Store) UpdateMessage(sessionID string, seq int, content string) error {
	if err := s.ensure(); err != nil {
		return err
	}
	res, err := s.db.Exec(`UPDATE messages SET content = ? WHERE session_id = ? AND seq = ?`, content, sessionID, seq)
	if err != nil {
		return fmt.Errorf("transcript: update message: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("transcript: no row for session %s seq %d", sessionID, seq)
	}
	return nil
}

// ensure gives read paths a clear error after Close (db handle is gone).
func (s *Store) ensure() error {
	if s.closed {
		return fmt.Errorf("transcript: store closed")
	}
	return nil
}
