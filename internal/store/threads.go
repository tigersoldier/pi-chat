package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// Thread states (DESIGN.md §4). `deleted` is a state and not a delete, so the
// thread key stays reserved and a later message starts a new session
// deliberately rather than by accident.
const (
	StateWarm    = "warm"
	StateCold    = "cold"
	StateDeleted = "deleted"
)

// ThreadRow is one chat thread and the pi session it owns.
//
// It is the row as stored; the core keeps the live connection beside it. The
// zero ThreadRow is not valid: ThreadKey and the three platform identifiers
// are what make it a thread.
type ThreadRow struct {
	ThreadKey   string // workspace:channel:thread_ts
	WorkspaceID string
	ChannelID   string
	ThreadTS    string

	SessionName string // deterministic for bot-created sessions
	SessionPath string // the gateway's identity for the session
	SessionID   string
	Cwd         string // where pi runs
	ProjectDir  string // the directory pi-chat owns and may delete, "" if adopted

	State string // StateWarm | StateCold | StateDeleted

	// Replay cursor: what the connection had consumed when it last went cold.
	LastSeq uint64
	LeafID  string

	ProgressTS string // the reply being streamed or patched, for restart recovery

	// ObservedTS is the observation watermark (DESIGN.md §4): the newest message
	// of the thread's conversation that a prompt has already carried. The next
	// turn fetches what came after it, so nothing is shown twice and nothing said
	// while the daemon was down is lost. Empty means the conversation has not
	// been read yet.
	ObservedTS string

	CreatedAt  time.Time
	LastActive time.Time
}

// Thread returns one thread by key. The second result is false when the thread
// is unknown, which is a normal answer rather than an error.
func (s *Store) Thread(ctx context.Context, key string) (ThreadRow, bool, error) {
	row, err := scanThread(s.db.QueryRowContext(ctx, selectThread+` WHERE thread_key = ?`, key))
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return ThreadRow{}, false, nil
	case err != nil:
		return ThreadRow{}, false, fmt.Errorf("store: read thread %s: %w", key, err)
	}
	return row, true, nil
}

// Threads returns every thread, newest activity first. The set is small (one
// row per thread the bot has ever been used in), so callers filter in memory.
func (s *Store) Threads(ctx context.Context) ([]ThreadRow, error) {
	rows, err := s.db.QueryContext(ctx, selectThread+` ORDER BY last_active DESC`)
	if err != nil {
		return nil, fmt.Errorf("store: list threads: %w", err)
	}
	defer rows.Close()

	var out []ThreadRow
	for rows.Next() {
		row, err := scanThread(rows)
		if err != nil {
			return nil, fmt.Errorf("store: read a thread row: %w", err)
		}
		out = append(out, row)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: list threads: %w", err)
	}
	return out, nil
}

// PutThread inserts or updates a thread. It is the one write path for the
// table, so a caller cannot forget a column: read, change, put.
func (s *Store) PutThread(ctx context.Context, row ThreadRow) error {
	if row.ThreadKey == "" {
		return errors.New("store: a thread row needs a key")
	}
	if row.State == "" {
		row.State = StateCold
	}
	if row.CreatedAt.IsZero() {
		row.CreatedAt = time.Now()
	}
	if row.LastActive.IsZero() {
		row.LastActive = row.CreatedAt
	}
	_, err := s.db.ExecContext(ctx, `
        INSERT INTO threads (thread_key, workspace_id, channel_id, thread_ts,
                             session_name, session_path, session_id, cwd, project_dir,
                             state, last_seq, leaf_id, progress_ts, observed_ts,
                             created_at, last_active)
        VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
        ON CONFLICT(thread_key) DO UPDATE SET
            workspace_id = excluded.workspace_id,
            channel_id   = excluded.channel_id,
            thread_ts    = excluded.thread_ts,
            session_name = excluded.session_name,
            session_path = excluded.session_path,
            session_id   = excluded.session_id,
            cwd          = excluded.cwd,
            project_dir  = excluded.project_dir,
            state        = excluded.state,
            last_seq     = excluded.last_seq,
            leaf_id      = excluded.leaf_id,
            progress_ts  = excluded.progress_ts,
            observed_ts  = excluded.observed_ts,
            last_active  = excluded.last_active`,
		row.ThreadKey, row.WorkspaceID, row.ChannelID, row.ThreadTS,
		row.SessionName, row.SessionPath, row.SessionID, row.Cwd, row.ProjectDir,
		row.State, row.LastSeq, row.LeafID, row.ProgressTS, row.ObservedTS,
		row.CreatedAt.Unix(), row.LastActive.Unix())
	if err != nil {
		return fmt.Errorf("store: write thread %s: %w", row.ThreadKey, err)
	}
	return nil
}

// SetThreadState records warm, cold or deleted without touching the cursor or
// the session identity.
func (s *Store) SetThreadState(ctx context.Context, key, state string) error {
	if state != StateWarm && state != StateCold && state != StateDeleted {
		return fmt.Errorf("store: unknown thread state %q", state)
	}
	res, err := s.db.ExecContext(ctx,
		`UPDATE threads SET state = ?, last_active = ? WHERE thread_key = ?`,
		state, time.Now().Unix(), key)
	if err != nil {
		return fmt.Errorf("store: set thread %s %s: %w", key, state, err)
	}
	if n, err := res.RowsAffected(); err == nil && n == 0 {
		return fmt.Errorf("store: set thread %s: %w", key, ErrNoThread)
	}
	return nil
}

// SetThreadCursor records how much of the session's event stream has been
// consumed, so the next attach resumes from there instead of at the head.
// A key with no row is not an error: the cursor is advisory, and the next
// lookup falls back to attaching at the head.
func (s *Store) SetThreadCursor(ctx context.Context, key string, seq uint64, leafID string) error {
	// The result is deliberately unused: nothing reads it back, and a no-op
	// update for an unknown key is fine per the comment above.
	_, err := s.db.ExecContext(ctx,
		`UPDATE threads SET last_seq = ?, leaf_id = ? WHERE thread_key = ?`,
		seq, leafID, key)
	if err != nil {
		return fmt.Errorf("store: save the cursor of %s: %w", key, err)
	}
	return nil
}

// SetProgressTS records the reply message a turn is writing into.
func (s *Store) SetProgressTS(ctx context.Context, key, ts string) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE threads SET progress_ts = ? WHERE thread_key = ?`, ts, key)
	if err != nil {
		return fmt.Errorf("store: save the reply handle of %s: %w", key, err)
	}
	return nil
}

// SetThreadObservedTS records how far the thread's conversation has been read
// into a prompt (DESIGN.md §4). Like the cursor, it is written after the fact:
// a prompt that failed must not move the watermark past messages the agent
// never saw.
func (s *Store) SetThreadObservedTS(ctx context.Context, key, ts string) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE threads SET observed_ts = ? WHERE thread_key = ?`, ts, key)
	if err != nil {
		return fmt.Errorf("store: save the observation watermark of %s: %w", key, err)
	}
	return nil
}

// MarkThreadsCold clears the warm marker for every thread and reports how many
// rows it changed.
//
// No thread is warm at startup: a warm thread means this process holds a bound
// connection to it, and a fresh process holds none. A crash would otherwise
// leave rows claiming to be warm forever, which is exactly the state `/pi
// status` and the warm-session cap read.
func (s *Store) MarkThreadsCold(ctx context.Context) (int64, error) {
	res, err := s.db.ExecContext(ctx,
		`UPDATE threads SET state = ? WHERE state = ?`, StateCold, StateWarm)
	if err != nil {
		return 0, fmt.Errorf("store: mark threads cold: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, nil // the update happened; only the count is unavailable
	}
	return n, nil
}

// ErrNoThread reports an operation on a thread key that is not in the
// database.
var ErrNoThread = errors.New("no such thread")

const selectThread = `
    SELECT thread_key, workspace_id, channel_id, thread_ts,
           session_name, session_path, session_id, cwd, project_dir,
           state, last_seq, leaf_id, progress_ts, observed_ts, created_at, last_active
    FROM threads`

// scanner is what *sql.Row and *sql.Rows have in common.
type scanner interface{ Scan(dest ...any) error }

func scanThread(src scanner) (ThreadRow, error) {
	var (
		row                   ThreadRow
		createdAt, lastActive int64
	)
	err := src.Scan(&row.ThreadKey, &row.WorkspaceID, &row.ChannelID, &row.ThreadTS,
		&row.SessionName, &row.SessionPath, &row.SessionID, &row.Cwd, &row.ProjectDir,
		&row.State, &row.LastSeq, &row.LeafID, &row.ProgressTS, &row.ObservedTS,
		&createdAt, &lastActive)
	if err != nil {
		return ThreadRow{}, err
	}
	row.CreatedAt = time.Unix(createdAt, 0).UTC()
	row.LastActive = time.Unix(lastActive, 0).UTC()
	return row, nil
}
