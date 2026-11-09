package transcript

// messages.go — append + load (with compaction rebuild and paging).

import (
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	cobottypes "github.com/liup215/cobot/types"
)

// tokenPair is one row's stored usage fact (the pi per-request baseline).
type tokenPair struct{ in, out int }

type toolCallRecord struct {
	ID    string          `json:"id"`
	Name  string          `json:"name"`
	Input json.RawMessage `json:"input"`
}

// Append inserts messages after the session's current max seq. When the
// session row is missing it is recreated first (FK self-heal: the active
// session can be deleted from the sidebar while a run still pushes to it —
// a resurrected row the user can delete again beats a silently lost message).
func (s *Store) Append(sessionID string, msgs []cobottypes.Message) error {
	if len(msgs) == 0 {
		return nil
	}
	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("transcript: begin: %w", err)
	}
	defer tx.Rollback()

	now := time.Now().Format(time.RFC3339)
	if _, err := tx.Exec(`INSERT OR IGNORE INTO sessions(id, title, created_at, updated_at) VALUES(?,?,?,?)`,
		sessionID, "", now, now); err != nil {
		return fmt.Errorf("transcript: ensure session: %w", err)
	}
	var maxSeq int
	if err := tx.QueryRow(`SELECT COALESCE(MAX(seq), -1) FROM messages WHERE session_id = ?`, sessionID).Scan(&maxSeq); err != nil {
		return fmt.Errorf("transcript: max seq: %w", err)
	}
	for i, m := range msgs {
		if err := insertMessageTx(tx, sessionID, m, maxSeq+1+i); err != nil {
			return fmt.Errorf("transcript: insert %d: %w", i, err)
		}
	}
	return tx.Commit()
}

func insertMessageTx(tx *sql.Tx, sessionID string, msg cobottypes.Message, seq int) error {
	records := make([]toolCallRecord, 0, len(msg.ToolCalls))
	for _, tc := range msg.ToolCalls {
		input := tc.Input
		if len(input) > 0 && !json.Valid(input) {
			// Raw text (not JSON): store as a JSON string so the column stays
			// parseable either way (desktop's original convention).
			wrapped, _ := json.Marshal(string(input))
			input = wrapped
		}
		records = append(records, toolCallRecord{ID: tc.ID, Name: tc.Name, Input: input})
	}
	toolCallsJSON, err := json.Marshal(records)
	if err != nil {
		return fmt.Errorf("transcript: marshal tool calls: %w", err)
	}
	created := msg.Timestamp
	if created.IsZero() {
		created = time.Now()
	}
	_, err = tx.Exec(`INSERT INTO messages(session_id, role, content, reasoning_content, tool_calls, tool_call_id, seq, usage_input, usage_output, created_at)
		VALUES(?,?,?,?,?,?,?,?,?,?)`,
		sessionID, string(msg.Role), msg.Content, msg.ReasoningContent, string(toolCallsJSON), msg.ToolCallID,
		seq, 0, 0, created.Format(time.RFC3339))
	if err != nil {
		return fmt.Errorf("transcript: insert message: %w", err)
	}
	return nil
}

const selectMessageCols = `SELECT seq, role, COALESCE(content,''), COALESCE(reasoning_content,''),
	COALESCE(tool_calls,''), COALESCE(tool_call_id,''), usage_input, usage_output, created_at`

// scanMessages decodes rows (callers of DESC queries reverse afterwards) and
// fills Timestamp from created_at. COALESCE keeps legacy rows with NULL text
// columns readable — an unscannable row would be silently dropped otherwise.
func scanMessages(rows *sql.Rows) ([]cobottypes.Message, []int, []tokenPair, error) {
	var msgs []cobottypes.Message
	var seqs []int
	var usage []tokenPair
	for rows.Next() {
		var msg cobottypes.Message
		var seq, usageIn, usageOut int
		var role, content, reasoning, toolCalls, toolCallID, created string
		if err := rows.Scan(&seq, &role, &content, &reasoning, &toolCalls, &toolCallID, &usageIn, &usageOut, &created); err != nil {
			continue
		}
		msg.Role = cobottypes.Role(role)
		msg.Content = content
		msg.ReasoningContent = reasoning
		msg.ToolCallID = toolCallID
		msg.Timestamp, _ = time.Parse(time.RFC3339, created)
		if toolCalls != "" {
			var records []toolCallRecord
			if err := json.Unmarshal([]byte(toolCalls), &records); err == nil {
				msg.ToolCalls = make([]cobottypes.ToolCall, len(records))
				for i, r := range records {
					input := r.Input
					// Legacy rows base64-encoded the input as a JSON string.
					if len(input) > 0 && input[0] == '"' {
						var str string
						if err := json.Unmarshal(input, &str); err == nil {
							if decoded, err := base64.StdEncoding.DecodeString(str); err == nil {
								input = decoded
							}
						}
					}
					msg.ToolCalls[i] = cobottypes.ToolCall{ID: r.ID, Name: r.Name, Input: input}
				}
			}
		}
		msgs = append(msgs, msg)
		seqs = append(seqs, seq)
		usage = append(usage, tokenPair{usageIn, usageOut})
	}
	return msgs, seqs, usage, rows.Err()
}

// Load returns the REBUILT transcript: protected prefix rows + the virtual
// summary row + tail (see the package doc). The dropped middle never reaches
// the caller — reloading a compacted session does not pay for another
// summarize call.
func (s *Store) Load(id string) ([]cobottypes.Message, error) {
	msgs, _, err := s.LoadWithSeqs(id)
	return msgs, err
}

// LoadWithSeqs is Load plus the stored seq of each row (the virtual summary
// row carries -1). The compaction anchor layer maps conversation indices
// onto these seqs.
func (s *Store) LoadWithSeqs(id string) ([]cobottypes.Message, []int, error) {
	if err := s.ensure(); err != nil {
		return nil, nil, err
	}
	rows, err := s.db.Query(selectMessageCols+` FROM messages WHERE session_id = ? ORDER BY seq ASC`, id)
	if err != nil {
		return nil, nil, fmt.Errorf("transcript: load: %w", err)
	}
	msgs, seqs, _, err := scanMessages(rows)
	rows.Close()
	if err != nil {
		return nil, nil, err
	}
	comp, err := s.LoadCompaction(id)
	if err != nil {
		return nil, nil, err
	}
	if comp == nil {
		return msgs, seqs, nil
	}
	return rebuild(msgs, seqs, comp)
}

func rebuild(msgs []cobottypes.Message, seqs []int, c *Compaction) ([]cobottypes.Message, []int, error) {
	prefixSet := make(map[int]bool, len(c.KeptPrefixSeqs))
	for _, sq := range c.KeptPrefixSeqs {
		prefixSet[sq] = true
	}
	pm := make([]cobottypes.Message, 0, len(c.KeptPrefixSeqs))
	ps := make([]int, 0, len(c.KeptPrefixSeqs))
	tm := make([]cobottypes.Message, 0, len(msgs))
	ts := make([]int, 0, len(msgs))
	for i, m := range msgs {
		switch {
		case seqs[i] > c.KeptAfterSeq:
			tm = append(tm, m)
			ts = append(ts, seqs[i])
		case prefixSet[seqs[i]]:
			pm = append(pm, m)
			ps = append(ps, seqs[i])
		}
		// everything else was summarized away
	}
	summary := cobottypes.Message{Role: c.SummaryRole, Content: c.Summary, Timestamp: c.CreatedAt}
	outM := append(append(append([]cobottypes.Message{}, pm...), summary), tm...)
	outS := append(append(append([]int{}, ps...), -1), ts...)
	return outM, outS, nil
}

// LoadPage windows the rebuilt transcript: pages walk the tail (seq > anchor)
// plus protected prefix rows; the virtual summary row is spliced in on the
// oldest page, right after the prefix (its chronological slot).
func (s *Store) LoadPage(id string, limit, beforeSeq int) (*Page, error) {
	if err := s.ensure(); err != nil {
		return nil, err
	}
	if limit <= 0 {
		limit = defaultPageLimit
	}
	if limit > maxPageLimit {
		limit = maxPageLimit
	}
	comp, err := s.LoadCompaction(id)
	if err != nil {
		return nil, err
	}
	var where string
	var args []interface{}
	if comp != nil {
		where, args, err = compactionWhere(comp)
		if err != nil {
			return nil, err
		}
	} else {
		where = `session_id = ?`
		args = []interface{}{id}
	}
	if beforeSeq > 0 {
		where += ` AND seq < ?`
		args = append(args, beforeSeq)
	}
	queryArgs := append([]interface{}{}, args...)
	queryArgs = append(queryArgs, limit)
	rows, err := s.db.Query(selectMessageCols+` FROM messages WHERE `+where+` ORDER BY seq DESC LIMIT ?`, queryArgs...)
	if err != nil {
		return nil, fmt.Errorf("transcript: page query: %w", err)
	}
	msgs, seqs, _, err := scanMessages(rows)
	rows.Close()
	if err != nil {
		return nil, err
	}
	// Newest-first → conversation order (ascending seq).
	for i, j := 0, len(msgs)-1; i < j; i, j = i+1, j-1 {
		msgs[i], msgs[j] = msgs[j], msgs[i]
		seqs[i], seqs[j] = seqs[j], seqs[i]
	}

	// Precise "older rows exist?" when the page filled up (a minSeq>0
	// heuristic misreports the floor when a protected prefix row has seq 0).
	hasMore := false
	if len(msgs) == limit && limit > 0 {
		existsArgs := append([]interface{}{}, args...)
		existsArgs = append(existsArgs, seqs[0])
		var exists int
		q := `SELECT EXISTS(SELECT 1 FROM messages WHERE ` + where + ` AND seq < ?)`
		if err := s.db.QueryRow(q, existsArgs...).Scan(&exists); err != nil {
			return nil, fmt.Errorf("transcript: older-rows check: %w", err)
		}
		hasMore = exists == 1
	}
	complete := !hasMore

	// Splice the virtual summary row in its chronological slot — only once
	// every kept row has been fetched, right after the protected prefix.
	summaryShown := false
	if comp != nil && complete {
		prefixSet := make(map[int]bool, len(comp.KeptPrefixSeqs))
		for _, sq := range comp.KeptPrefixSeqs {
			prefixSet[sq] = true
		}
		idx := 0
		for _, sq := range seqs {
			if prefixSet[sq] {
				idx++
			}
		}
		summary := cobottypes.Message{Role: comp.SummaryRole, Content: comp.Summary, Timestamp: comp.CreatedAt}
		msgs = append(msgs[:idx:idx], append([]cobottypes.Message{summary}, msgs[idx:]...)...)
		seqs = append(seqs[:idx:idx], append([]int{-1}, seqs[idx:]...)...)
		summaryShown = true
	}

	page := &Page{Messages: msgs, TotalCount: -1, EarliestSeq: -1, HasMore: hasMore && !summaryShown}
	if len(seqs) > 0 {
		page.EarliestSeq = seqs[0]
	}
	if n, err := s.Count(id); err == nil {
		page.TotalCount = n
	}
	return page, nil
}

// Count returns the REBUILT message count (what a load materializes).
func (s *Store) Count(id string) (int, error) {
	if err := s.ensure(); err != nil {
		return 0, err
	}
	comp, err := s.LoadCompaction(id)
	if err != nil {
		return 0, err
	}
	if comp == nil {
		var n int
		if err := s.db.QueryRow(`SELECT COUNT(*) FROM messages WHERE session_id = ?`, id).Scan(&n); err != nil {
			return 0, fmt.Errorf("transcript: count: %w", err)
		}
		return n, nil
	}
	where, args, err := compactionWhere(comp)
	if err != nil {
		return 0, err
	}
	var n int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM messages WHERE `+where, args...).Scan(&n); err != nil {
		return 0, fmt.Errorf("transcript: count rebuilt: %w", err)
	}
	return n + 1, nil // + the virtual summary row
}

func compactionWhere(c *Compaction) (string, []interface{}, error) {
	where := `session_id = ? AND (seq > ?`
	args := []interface{}{c.SessionID, c.KeptAfterSeq}
	if len(c.KeptPrefixSeqs) > 0 {
		where += ` OR seq IN (` + placeholders(len(c.KeptPrefixSeqs)) + `)`
		for _, sq := range c.KeptPrefixSeqs {
			args = append(args, sq)
		}
	}
	where += `)`
	return where, args, nil
}

func placeholders(n int) string {
	return strings.Repeat("?,", n-1) + "?"
}
