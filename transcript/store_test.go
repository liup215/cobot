package transcript

// store_test.go — the acceptance suite ported from the desktop's chat tests
// (they were the oracle that rebuilt a destroyed file once; they are the
// oracle for this rewrite too): pragmas actually active, FK + orphan sweep,
// lazy session shells, compaction rebuild + paging, usage facts stamped in
// FIFO order, and the pi context rule.

import (
	"database/sql"
	"fmt"
	"path/filepath"
	"testing"

	cobottypes "github.com/liup215/cobot/types"
)

func openStore(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "transcript.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func msg(role cobottypes.Role, content string) cobottypes.Message {
	return cobottypes.Message{Role: role, Content: content}
}

// seed creates a session with n sequential user messages (m0..m{n-1}).
func seed(t *testing.T, s *Store, n int) string {
	t.Helper()
	meta := NewSession("seed")
	if err := s.Create(meta); err != nil {
		t.Fatalf("Create: %v", err)
	}
	msgs := make([]cobottypes.Message, n)
	for i := range msgs {
		msgs[i] = msg(cobottypes.RoleUser, fmt.Sprintf("m%d", i))
	}
	if err := s.Append(meta.ID, msgs); err != nil {
		t.Fatalf("Append: %v", err)
	}
	return meta.ID
}

func contents(msgs []cobottypes.Message) []string {
	out := make([]string, len(msgs))
	for i, m := range msgs {
		out[i] = m.Content
	}
	return out
}

// The flat DSN style is silently ignored by modernc — these three pragmas
// are the whole reason the rewrite pins _pragma= syntax.
func TestOpenPragmasActive(t *testing.T) {
	s := openStore(t)
	var journal, fk string
	var busy int
	if err := s.db.QueryRow("PRAGMA journal_mode").Scan(&journal); err != nil {
		t.Fatal(err)
	}
	if err := s.db.QueryRow("PRAGMA foreign_keys").Scan(&fk); err != nil {
		t.Fatal(err)
	}
	if err := s.db.QueryRow("PRAGMA busy_timeout").Scan(&busy); err != nil {
		t.Fatal(err)
	}
	if journal != "wal" || fk != "1" || busy != 5000 {
		t.Errorf("pragmas = journal:%q fk:%q busy:%d, want wal/1/5000", journal, fk, busy)
	}
}

func TestFKRejectsOrphanMessage(t *testing.T) {
	s := openStore(t)
	_, err := s.db.Exec(`INSERT INTO messages(session_id, role, content, seq, created_at)
		VALUES('ghost','user','orphan',0,'t')`)
	if err == nil {
		t.Fatal("orphan insert must fail once foreign_keys=1")
	}
}

// Fossils from the era when foreign keys were silently off get swept on the
// first open; valid rows survive.
func TestOrphanSweepOnOpen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "legacy.db")
	raw, err := sql.Open("sqlite", path) // no pragmas — the historical state
	if err != nil {
		t.Fatal(err)
	}
	for _, stmt := range []string{
		`CREATE TABLE sessions (id TEXT PRIMARY KEY, title TEXT NOT NULL, created_at TEXT NOT NULL, updated_at TEXT NOT NULL)`,
		`CREATE TABLE messages (id INTEGER PRIMARY KEY AUTOINCREMENT, session_id TEXT NOT NULL, role TEXT NOT NULL,
			content TEXT NOT NULL, reasoning_content TEXT, tool_calls TEXT, tool_call_id TEXT, seq INTEGER NOT NULL,
			created_at TEXT NOT NULL)`,
		`INSERT INTO sessions VALUES ('keep','t','t','t')`,
		`INSERT INTO messages (session_id, role, content, seq, created_at) VALUES ('keep','user','valid',0,'t')`,
		`INSERT INTO messages (session_id, role, content, seq, created_at) VALUES ('ghost','user','orphan',0,'t')`,
	} {
		if _, err := raw.Exec(stmt); err != nil {
			raw.Close()
			t.Fatalf("seed legacy: %v", err)
		}
	}
	raw.Close()

	s, err := Open(path)
	if err != nil {
		t.Fatalf("Open legacy: %v", err)
	}
	defer s.Close()

	var orphans int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM messages WHERE session_id = 'ghost'`).Scan(&orphans); err != nil {
		t.Fatal(err)
	}
	if orphans != 0 {
		t.Fatalf("orphans survived the sweep: %d", orphans)
	}
	n, err := s.Count("keep")
	if err != nil || n != 1 {
		t.Fatalf("valid rows damaged: %d, %v", n, err)
	}
}

// Appending to a session deleted mid-run recreates the shell (FK self-heal)
// instead of silently dropping the message.
func TestAppendSelfHealsMissingSession(t *testing.T) {
	s := openStore(t)
	if err := s.Append("sess-gone", []cobottypes.Message{msg(cobottypes.RoleUser, "still here")}); err != nil {
		t.Fatalf("append to missing session: %v", err)
	}
	n, err := s.Count("sess-gone")
	if err != nil || n != 1 {
		t.Fatalf("message lost: %d, %v", n, err)
	}
	if _, err := s.LoadMeta("sess-gone"); err != nil {
		t.Fatalf("shell not recreated: %v", err)
	}
}

// A session with no messages is not history (List hides shells — clicking
// "new task" N times must not grow the sidebar).
func TestListHidesShells(t *testing.T) {
	s := openStore(t)
	shell := NewSession("shell")
	if err := s.Create(shell); err != nil {
		t.Fatal(err)
	}
	full := seed(t, s, 2)
	list, err := s.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 || list[0].ID != full {
		t.Fatalf("list = %+v, want only the message-bearing session", list)
	}
}

// Rebuild: [protected prefix] + [virtual summary] + [tail] — the exact shape
// cobot's Summarize produces; the dropped middle never reaches the caller.
func TestLoadRebuildsCompactedTranscript(t *testing.T) {
	s := openStore(t)
	id := seed(t, s, 12) // seq 0..11
	summary := "[summary] oldest rounds condensed"
	if err := s.SaveCompaction(&Compaction{
		SessionID: id, Summary: summary, SummaryRole: cobottypes.RoleUser,
		KeptAfterSeq: 4, KeptPrefixSeqs: []int{0},
	}); err != nil {
		t.Fatalf("SaveCompaction: %v", err)
	}

	msgs, seqs, err := s.LoadWithSeqs(id)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	want := []string{"m0", summary, "m5", "m6", "m7", "m8", "m9", "m10", "m11"}
	got := contents(msgs)
	if len(got) != len(want) {
		t.Fatalf("rebuilt = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("rebuilt[%d] = %q, want %q (all: %v)", i, got[i], want[i], got)
		}
	}
	// seqs: prefix keeps its stored seq, the summary is virtual (-1), tail
	// keeps theirs.
	wantSeqs := []int{0, -1, 5, 6, 7, 8, 9, 10, 11}
	for i := range wantSeqs {
		if seqs[i] != wantSeqs[i] {
			t.Fatalf("seqs = %v, want %v", seqs, wantSeqs)
		}
	}

	n, err := s.Count(id)
	if err != nil || n != len(want) {
		t.Errorf("Count = %d, %v; want %d", n, err, len(want))
	}

	// Upsert absorbs the previous summary (anchor only moves forward).
	if err := s.SaveCompaction(&Compaction{
		SessionID: id, Summary: "second summary", SummaryRole: cobottypes.RoleUser,
		KeptAfterSeq: 7, KeptPrefixSeqs: []int{0},
	}); err != nil {
		t.Fatal(err)
	}
	c, err := s.LoadCompaction(id)
	if err != nil || c == nil || c.Summary != "second summary" || c.KeptAfterSeq != 7 {
		t.Fatalf("upsert failed: %+v %v", c, err)
	}
}

// Pages walk the rebuilt window: tail first, prefix+summary on the oldest
// page, exact older-rows check instead of a minSeq heuristic.
func TestLoadPageRebuiltWindows(t *testing.T) {
	s := openStore(t)
	id := seed(t, s, 12)
	if err := s.SaveCompaction(&Compaction{
		SessionID: id, Summary: "[summary]", SummaryRole: cobottypes.RoleUser,
		KeptAfterSeq: 4, KeptPrefixSeqs: []int{0},
	}); err != nil {
		t.Fatal(err)
	}

	p1, err := s.LoadPage(id, 3, 0)
	if err != nil {
		t.Fatal(err)
	}
	if got := contents(p1.Messages); len(got) != 3 || got[0] != "m9" || got[2] != "m11" {
		t.Fatalf("page1 = %v, want [m9 m10 m11]", got)
	}
	if !p1.HasMore || p1.EarliestSeq != 9 || p1.TotalCount != 9 {
		t.Fatalf("page1 meta: hasMore=%v earliest=%d total=%d", p1.HasMore, p1.EarliestSeq, p1.TotalCount)
	}

	p2, err := s.LoadPage(id, 3, p1.EarliestSeq)
	if err != nil {
		t.Fatal(err)
	}
	if got := contents(p2.Messages); len(got) != 3 || got[0] != "m6" || got[2] != "m8" {
		t.Fatalf("page2 = %v, want [m6 m7 m8]", got)
	}
	if !p2.HasMore {
		t.Fatal("page2 must report older rows")
	}

	p3, err := s.LoadPage(id, 3, p2.EarliestSeq)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"m0", "[summary]", "m5"}
	got := contents(p3.Messages)
	if len(got) != 3 || got[0] != want[0] || got[1] != want[1] || got[2] != want[2] {
		t.Fatalf("page3 = %v, want %v", got, want)
	}
	if p3.HasMore || p3.EarliestSeq != 0 {
		t.Fatalf("page3 meta: hasMore=%v earliest=%d", p3.HasMore, p3.EarliestSeq)
	}
}

// Plain (uncompacted) windows keep the original paging contract.
func TestLoadPagePlainWindows(t *testing.T) {
	s := openStore(t)
	id := seed(t, s, 10) // m0..m9

	p1, err := s.LoadPage(id, 4, 0)
	if err != nil {
		t.Fatal(err)
	}
	if got := contents(p1.Messages); len(got) != 4 || got[0] != "m6" || got[3] != "m9" {
		t.Fatalf("page1 = %v, want [m6..m9]", got)
	}
	if !p1.HasMore || p1.EarliestSeq != 6 {
		t.Fatalf("page1 meta: hasMore=%v earliest=%d", p1.HasMore, p1.EarliestSeq)
	}

	p2, err := s.LoadPage(id, 4, p1.EarliestSeq)
	if err != nil {
		t.Fatal(err)
	}
	if got := contents(p2.Messages); len(got) != 4 || got[0] != "m2" || got[3] != "m5" {
		t.Fatalf("page2 = %v, want [m2..m5]", got)
	}
	p3, err := s.LoadPage(id, 4, p2.EarliestSeq)
	if err != nil {
		t.Fatal(err)
	}
	if got := contents(p3.Messages); len(got) != 2 || got[0] != "m0" || got[1] != "m1" {
		t.Fatalf("page3 = %v, want [m0 m1]", got)
	}
	if p3.HasMore {
		t.Fatal("page3 is the floor")
	}

	// Missing session → empty page, no error.
	empty, err := s.LoadPage("no-such", 4, 0)
	if err != nil || len(empty.Messages) != 0 || empty.HasMore {
		t.Fatalf("missing session page = %+v, %v", empty, err)
	}
}

// Usage facts: StampUsage targets the LATEST assistant row, survives reload,
// and never touches earlier turns.
func TestStampUsageFacts(t *testing.T) {
	s := openStore(t)
	meta := NewSession("usage")
	if err := s.Create(meta); err != nil {
		t.Fatal(err)
	}
	turn1 := []cobottypes.Message{
		msg(cobottypes.RoleUser, "q1"),
		msg(cobottypes.RoleAssistant, "a1"),
		msg(cobottypes.RoleUser, "q2"),
		msg(cobottypes.RoleAssistant, "a2"),
	}
	if err := s.Append(meta.ID, turn1); err != nil {
		t.Fatal(err)
	}

	if err := s.StampUsage(meta.ID, 1200, 30); err != nil {
		t.Fatalf("StampUsage: %v", err)
	}
	used, actual := s.ContextUsage(meta.ID)
	if !actual || used != 1230 {
		t.Fatalf("ContextUsage = %d/%v, want 1230/true", used, actual)
	}
	// Only the newest assistant row carries the stamp.
	var olderIn int
	if err := s.db.QueryRow(`SELECT usage_input FROM messages WHERE session_id = ? AND seq = 1`, meta.ID).Scan(&olderIn); err != nil {
		t.Fatal(err)
	}
	if olderIn != 0 {
		t.Errorf("older assistant row polluted: %d", olderIn)
	}

	// Facts survive reload (stored columns, not derived).
	msgs, err := s.Load(meta.ID)
	if err != nil || len(msgs) != 4 {
		t.Fatalf("Load: %v %v", len(msgs), err)
	}

	// No assistant row → error (caller logs; the estimate stays honest).
	alone := NewSession("no assistant")
	if err := s.Create(alone); err != nil {
		t.Fatal(err)
	}
	if err := s.Append(alone.ID, []cobottypes.Message{msg(cobottypes.RoleUser, "only a question")}); err != nil {
		t.Fatal(err)
	}
	if err := s.StampUsage(alone.ID, 10, 10); err == nil {
		t.Fatal("stamping a session with no assistant row must error")
	}
}

// The FIFO: usage enqueued AFTER the messages is applied only once they are
// on disk — the whole ordering guarantee behind StampUsage.
func TestQueueFIFOUsageStamp(t *testing.T) {
	s := openStore(t)
	meta := NewSession("fifo")
	if err := s.Create(meta); err != nil {
		t.Fatal(err)
	}
	if err := s.Push(meta.ID, []cobottypes.Message{msg(cobottypes.RoleUser, "q")}); err != nil {
		t.Fatal(err)
	}
	if err := s.Push(meta.ID, []cobottypes.Message{msg(cobottypes.RoleAssistant, "the assistant answer")}); err != nil {
		t.Fatal(err)
	}
	s.PushUsage(meta.ID, 999, 8)
	if err := s.Flush(); err != nil {
		t.Fatalf("Flush: %v", err)
	}

	used, actual := s.ContextUsage(meta.ID)
	if !actual || used != 1007 {
		t.Fatalf("after flush: used=%d actual=%v, want 1007/true", used, actual)
	}
	n, err := s.Count(meta.ID)
	if err != nil || n != 2 {
		t.Fatalf("count = %d, %v; want 2 (messages landed before the stamp)", n, err)
	}

	// Zero measurements never stamp.
	before, _ := s.ContextUsage(meta.ID)
	s.PushUsage(meta.ID, 0, 0)
	if err := s.Flush(); err != nil {
		t.Fatal(err)
	}
	after, _ := s.ContextUsage(meta.ID)
	if before != after {
		t.Fatalf("zero usage changed the baseline: %d -> %d", before, after)
	}
}

// The pi rule end-to-end: fact baseline + trailing estimate; pure estimate
// (marked approximate) when no fact exists yet.
func TestContextUsagePiRule(t *testing.T) {
	s := openStore(t)
	meta := NewSession("ctx")
	if err := s.Create(meta); err != nil {
		t.Fatal(err)
	}
	// No facts yet → estimate of everything, actual=false.
	if err := s.Append(meta.ID, []cobottypes.Message{msg(cobottypes.RoleUser, "some opening question about the task")}); err != nil {
		t.Fatal(err)
	}
	used, actual := s.ContextUsage(meta.ID)
	if actual || used <= 0 {
		t.Fatalf("estimate path: used=%d actual=%v, want >0/false", used, actual)
	}

	// With a fact on a NON-final row: baseline + estimation over the trailing
	// rows (they postdate the measured request).
	if err := s.Append(meta.ID, []cobottypes.Message{
		msg(cobottypes.RoleAssistant, "the measured answer"),
		msg(cobottypes.RoleTool, "a very long trailing tool result that definitely costs tokens beyond the measured request"),
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.StampUsage(meta.ID, 1000, 40); err != nil {
		t.Fatal(err)
	}
	used, actual = s.ContextUsage(meta.ID)
	if !actual {
		t.Fatalf("fact path must report actual=true: %+v", used)
	}
	if used <= 1040 {
		t.Fatalf("baseline 1040 + trailing estimate not applied: %d", used)
	}
}

func TestResolveSeqAndRewrite(t *testing.T) {
	s := openStore(t)
	id := seed(t, s, 3)

	seq, ok := s.ResolveSeq(id, cobottypes.RoleUser, "m2")
	if !ok || seq != 2 {
		t.Fatalf("ResolveSeq = %d, %v; want 2", seq, ok)
	}
	if _, ok := s.ResolveSeq(id, cobottypes.RoleUser, "no such content"); ok {
		t.Fatal("missing content must not resolve")
	}

	if err := s.UpdateMessage(id, 2, "m2 [stubbed]"); err != nil {
		t.Fatalf("UpdateMessage: %v", err)
	}
	msgs, err := s.Load(id)
	if err != nil {
		t.Fatal(err)
	}
	if msgs[2].Content != "m2 [stubbed]" {
		t.Errorf("rewrite not applied: %q", msgs[2].Content)
	}
	if err := s.UpdateMessage(id, 99, "x"); err == nil {
		t.Fatal("missing row rewrite must error")
	}
}

// Close drains and rejects later work; double Close is safe.
func TestCloseIdempotent(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "c.db"))
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if err := s.Push("x", []cobottypes.Message{msg(cobottypes.RoleUser, "late")}); err == nil {
		t.Fatal("push after close must be rejected")
	}
	if _, err := s.Load("x"); err == nil {
		t.Fatal("load after close must error")
	}
}

// TestNewSessionIDsUniqueWithinOneTick pins the collision fix: on Windows
// time.Now().UnixNano() repeats within ~15.6ms (measured), so two sessions
// born back-to-back MUST still get distinct ids — same-tick ids silently
// merged two conversations under one row set.
func TestNewSessionIDsUniqueWithinOneTick(t *testing.T) {
	seen := make(map[string]bool, 2000)
	for i := 0; i < 2000; i++ {
		id := NewSession("s").ID
		if seen[id] {
			t.Fatalf("duplicate session id at call %d: %s", i, id)
		}
		seen[id] = true
	}
}
