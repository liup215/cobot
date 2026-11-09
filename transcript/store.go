// Package transcript is cobot's durable conversation store — the agent's
// memory across restarts, host-independent and self-contained.
//
// Why this lives in cobot (not in a host app): resume, compaction
// durability, usage facts and context accounting are agent semantics every
// host needs. The desktop previously hand-rolled all of it; any second host
// would have re-implemented it again.
//
// Design notes:
//
//   - Instance-based, no package globals: Open(path) returns a Store; tests
//     point it at a temp file instead of hacking process-wide state.
//   - Schema is byte-compatible with the desktop's history.db (sessions,
//     messages with usage_input/usage_output, compactions) so existing user
//     data opens unchanged — no migration step.
//   - Pragmas MUST use modernc's _pragma=name(value) syntax; the flat
//     _journal=/_busy_timeout=/_foreign_keys= style is silently ignored
//     (measured: those databases ran in DELETE mode with FKs and busy
//     timeout off). WAL + foreign keys + busy timeout are asserted by test.
//   - Reads are synchronous and see the committed state; writes go through
//     a FIFO write-behind queue (Push/PushUsage) so a turn's usage stamp is
//     applied strictly after its messages land. Flush() makes the queue
//     deterministic for tests and shutdown.
//   - Compaction is a FACT, not a rewrite: SaveCompaction upserts
//     {summary, kept_after_seq, kept_prefix_seqs}; Load/LoadPage return the
//     REBUILT transcript (protected prefix + virtual summary row + tail) and
//     never touch the stored originals (pi's model).
package transcript

import (
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"fmt"
	"sync"
	"time"

	cobottypes "github.com/liup215/cobot/types"
	_ "modernc.org/sqlite"
)

// SessionMeta is the session header row (no messages — a session is born
// with its first message; List hides shells).
type SessionMeta struct {
	ID        string
	Title     string
	CreatedAt time.Time
	UpdatedAt time.Time
}

// Page is one window of a session in conversation order.
type Page struct {
	Messages []cobottypes.Message
	// TotalCount is the REBUILT message count (tail + protected prefix + the
	// virtual summary row when a compaction exists).
	TotalCount int
	// EarliestSeq is the stored seq of Messages[0] (-1 when empty) — pass it
	// as beforeSeq for the next older page.
	EarliestSeq int
	HasMore     bool
}

// Compaction is the persisted result of one summarize event. KeptAfterSeq
// anchors the tail (rows with seq > KeptAfterSeq); KeptPrefixSeqs lists the
// protected rows kept from BEFORE the cut (the original task / system rows)
// that a single anchor cannot express. Summary is the exact post-compaction
// summary row content.
type Compaction struct {
	SessionID      string
	Summary        string
	SummaryRole    cobottypes.Role
	KeptAfterSeq   int
	KeptPrefixSeqs []int
	CreatedAt      time.Time
}

// Store is a transcript database handle. Zero value unusable — Open it.
type Store struct {
	db *sql.DB

	// write-behind queue (see queue.go)
	tasks     chan task
	done      chan struct{}
	mu        sync.RWMutex // guards closed around queue sends
	closed    bool
	closeOnce sync.Once
}

const (
	defaultPageLimit = 500
	maxPageLimit     = 2000
)

// Open opens (creating/migrating as needed) the transcript database at path.
// The pragmas, schema, additive migrations and the orphan sweep all run here
// — see the package doc for why _pragma= syntax matters.
func Open(path string) (*Store, error) {
	db, err := sql.Open("sqlite", path+"?_pragma=journal_mode(WAL)&_pragma=foreign_keys(1)&_pragma=busy_timeout(5000)")
	if err != nil {
		return nil, fmt.Errorf("transcript: open: %w", err)
	}
	// Serialize connections: SQLite has a single writer anyway, and a single
	// connection removes the whole class of pool-contention BUSY failures.
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	if err := db.Ping(); err != nil {
		db.Close()
		return nil, fmt.Errorf("transcript: ping: %w", err)
	}
	s := &Store{db: db, tasks: make(chan task, 1000), done: make(chan struct{})}
	if err := s.init(); err != nil {
		db.Close()
		return nil, err
	}
	go s.worker()
	return s, nil
}

func (s *Store) init() error {
	stmts := []string{
		`CREATE TABLE IF NOT EXISTS sessions (
			id TEXT PRIMARY KEY,
			title TEXT NOT NULL,
			created_at TEXT NOT NULL,
			updated_at TEXT NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS messages (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			session_id TEXT NOT NULL,
			role TEXT NOT NULL,
			content TEXT NOT NULL,
			reasoning_content TEXT,
			tool_calls TEXT,
			tool_call_id TEXT,
			seq INTEGER NOT NULL,
			created_at TEXT NOT NULL,
			FOREIGN KEY(session_id) REFERENCES sessions(id) ON DELETE CASCADE
		)`,
		`CREATE INDEX IF NOT EXISTS idx_messages_session_seq ON messages(session_id, seq)`,
		`CREATE TABLE IF NOT EXISTS compactions (
			session_id TEXT PRIMARY KEY,
			summary TEXT NOT NULL,
			summary_role TEXT NOT NULL,
			kept_after_seq INTEGER NOT NULL,
			kept_prefix_seqs TEXT NOT NULL DEFAULT '[]',
			created_at TEXT NOT NULL
		)`,
	}
	for _, stmt := range stmts {
		if _, err := s.db.Exec(stmt); err != nil {
			return fmt.Errorf("transcript: schema: %w", err)
		}
	}
	// Additive migrations (measured: flat DSN pragmas never applied, so
	// databases created before this package may lack these columns).
	for _, col := range []string{"usage_input", "usage_output"} {
		if err := addColumnIfMissing(s.db, "messages", col); err != nil {
			return fmt.Errorf("transcript: migrate %s: %w", col, err)
		}
	}
	// Orphans are the fossil of the era when foreign keys were silently off;
	// FKs now prevent new ones.
	if _, err := s.db.Exec(`DELETE FROM messages WHERE session_id NOT IN (SELECT id FROM sessions)`); err != nil {
		return fmt.Errorf("transcript: orphan sweep: %w", err)
	}
	return nil
}

func addColumnIfMissing(db *sql.DB, table, column string) error {
	rows, err := db.Query(fmt.Sprintf(`PRAGMA table_info(%s)`, table))
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var cid, notNull, pk int
		var name, typ string
		var dflt sql.NullString
		if err := rows.Scan(&cid, &name, &typ, &notNull, &dflt, &pk); err != nil {
			return err
		}
		if name == column {
			return rows.Err()
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	_, err = db.Exec(fmt.Sprintf(`ALTER TABLE %s ADD COLUMN %s INTEGER NOT NULL DEFAULT 0`, table, column))
	return err
}

// ---------------------------------------------------------------------------
// Sessions (metadata)
// ---------------------------------------------------------------------------

// NewSession builds an unsaved session header.
//
// The id carries wall time PLUS4 random bytes: time.Now().UnixNano() has
// ~15.6ms granularity on Windows (measured: back-to-back calls return the
// SAME value), so nano alone collides for two sessions born in one tick —
// both conversations would share one row set and silently merge.
func NewSession(title string) *SessionMeta {
	now := time.Now()
	return &SessionMeta{
		ID:        newSessionID(now),
		Title:     title,
		CreatedAt: now,
		UpdatedAt: now,
	}
}

// newSessionID = "sess_<wall-ns>-<8hex>" — collision-safe within and across
// processes (2^-32 random suffix on top of the timestamp).
func newSessionID(now time.Time) string {
	var b [4]byte
	_, _ = rand.Read(b[:])
	return fmt.Sprintf("sess_%d-%s", now.UnixNano(), hex.EncodeToString(b[:]))
}

// Create persists a session header. Callers normally create lazily — on the
// first message, not on a "new task" click.
func (s *Store) Create(meta *SessionMeta) error {
	_, err := s.db.Exec(`INSERT INTO sessions(id, title, created_at, updated_at) VALUES(?,?,?,?)
		ON CONFLICT(id) DO UPDATE SET title=excluded.title, updated_at=excluded.updated_at`,
		meta.ID, meta.Title, meta.CreatedAt.Format(time.RFC3339), meta.UpdatedAt.Format(time.RFC3339))
	if err != nil {
		return fmt.Errorf("transcript: create session: %w", err)
	}
	return nil
}

// LoadMeta returns one session header without its messages.
func (s *Store) LoadMeta(id string) (*SessionMeta, error) {
	var m SessionMeta
	var created, updated string
	err := s.db.QueryRow(`SELECT id, title, created_at, updated_at FROM sessions WHERE id = ?`, id).
		Scan(&m.ID, &m.Title, &created, &updated)
	if err == sql.ErrNoRows {
		return nil, fmt.Errorf("transcript: session %s not found", id)
	}
	if err != nil {
		return nil, fmt.Errorf("transcript: load meta: %w", err)
	}
	m.CreatedAt, _ = time.Parse(time.RFC3339, created)
	m.UpdatedAt, _ = time.Parse(time.RFC3339, updated)
	return &m, nil
}

// List returns sessions that own at least one message — empty shells (a
// "new task" that was never sent to, an aborted first send) are not history.
func (s *Store) List() ([]SessionMeta, error) {
	rows, err := s.db.Query(`SELECT id, title, created_at, updated_at FROM sessions s
		WHERE EXISTS (SELECT 1 FROM messages m WHERE m.session_id = s.id)
		ORDER BY updated_at DESC`)
	if err != nil {
		return nil, fmt.Errorf("transcript: list: %w", err)
	}
	defer rows.Close()
	var out []SessionMeta
	for rows.Next() {
		var m SessionMeta
		var created, updated string
		if err := rows.Scan(&m.ID, &m.Title, &created, &updated); err != nil {
			continue
		}
		m.CreatedAt, _ = time.Parse(time.RFC3339, created)
		m.UpdatedAt, _ = time.Parse(time.RFC3339, updated)
		out = append(out, m)
	}
	return out, rows.Err()
}

// Rename updates a session title.
func (s *Store) Rename(id, title string) error {
	if _, err := s.db.Exec(`UPDATE sessions SET title = ?, updated_at = ? WHERE id = ?`,
		title, time.Now().Format(time.RFC3339), id); err != nil {
		return fmt.Errorf("transcript: rename: %w", err)
	}
	return nil
}

// Delete removes a session and its messages (FK cascade + explicit, in that
// order of belt-and-braces).
func (s *Store) Delete(id string) error {
	if _, err := s.db.Exec(`DELETE FROM messages WHERE session_id = ?`, id); err != nil {
		return fmt.Errorf("transcript: delete messages: %w", err)
	}
	if _, err := s.db.Exec(`DELETE FROM sessions WHERE id = ?`, id); err != nil {
		return fmt.Errorf("transcript: delete session: %w", err)
	}
	if _, err := s.db.Exec(`DELETE FROM compactions WHERE session_id = ?`, id); err != nil {
		return fmt.Errorf("transcript: delete compaction: %w", err)
	}
	return nil
}
