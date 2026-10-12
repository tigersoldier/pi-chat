package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// RetiredSession is a session pi-chat replaced but did not delete.
//
// `/pi new` means a clean slate, not "throw my work away" — that is what
// `/pi delete` is for — so the session file and its working directory stay, and
// the session stays adoptable through `/pi resume`. The record exists for one
// reason: the startup sweep removes project directories that no row accounts
// for, and without this the directory of a session the user may still resume
// would look exactly like the remains of a crash (DESIGN.md §4, §9).
type RetiredSession struct {
	SessionPath string // the gateway's identity for the session
	ThreadKey   string
	SessionName string
	ProjectDir  string // the directory pi-chat owns, "" when there is none
	ObservedTS  string // the thread's observation watermark for this session
	LastSeq     uint64 // gateway event cursor for this session
	LeafID      string
	RetiredAt   time.Time
}

// RetireSession records that a session has been replaced by a new one.
func (s *Store) RetireSession(ctx context.Context, r RetiredSession) error {
	if r.SessionPath == "" {
		return errors.New("store: a retired session needs its path")
	}
	if r.ThreadKey == "" {
		return errors.New("store: a retired session needs the thread it belonged to")
	}
	if r.RetiredAt.IsZero() {
		r.RetiredAt = time.Now()
	}
	_, err := s.db.ExecContext(ctx, `
        INSERT INTO retired (session_path, thread_key, session_name, project_dir,
                             observed_ts, last_seq, leaf_id, retired_at)
        VALUES (?, ?, ?, ?, ?, ?, ?, ?)
        ON CONFLICT(session_path) DO UPDATE SET
            thread_key   = excluded.thread_key,
            session_name = excluded.session_name,
            project_dir  = excluded.project_dir,
            observed_ts  = excluded.observed_ts,
            last_seq     = excluded.last_seq,
            leaf_id      = excluded.leaf_id`,
		r.SessionPath, r.ThreadKey, r.SessionName, r.ProjectDir, r.ObservedTS, r.LastSeq, r.LeafID, r.RetiredAt.Unix())
	if err != nil {
		return fmt.Errorf("store: retire session %s: %w", r.SessionPath, err)
	}
	return nil
}

// RetiredSessions lists resumable sessions. An empty threadKey lists every
// retired session; root-level adoption uses that to keep them out of other
// threads' pickers.
func (s *Store) RetiredSessions(ctx context.Context, threadKey string) ([]RetiredSession, error) {
	query := `SELECT session_path, thread_key, session_name, project_dir,
                     observed_ts, last_seq, leaf_id, retired_at FROM retired`
	var args []any
	if threadKey != "" {
		query += ` WHERE thread_key = ?`
		args = append(args, threadKey)
	}
	query += ` ORDER BY retired_at DESC`
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("store: list retired sessions: %w", err)
	}
	defer rows.Close()
	var out []RetiredSession
	for rows.Next() {
		var retired RetiredSession
		var retiredAt int64
		if err := rows.Scan(&retired.SessionPath, &retired.ThreadKey, &retired.SessionName,
			&retired.ProjectDir, &retired.ObservedTS, &retired.LastSeq, &retired.LeafID, &retiredAt); err != nil {
			return nil, fmt.Errorf("store: read a retired session: %w", err)
		}
		retired.RetiredAt = time.Unix(retiredAt, 0)
		out = append(out, retired)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: list retired sessions: %w", err)
	}
	return out, nil
}

// RestoreRetiredSession atomically moves one session from the retired set back
// onto its original thread. It refuses to overwrite a newer session or to move
// a path to a different thread.
func (s *Store) RestoreRetiredSession(ctx context.Context, threadKey, sessionPath, sessionID, cwd string) (ThreadRow, bool, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return ThreadRow{}, false, fmt.Errorf("store: begin restoring a retired session: %w", err)
	}
	defer tx.Rollback()

	var retired RetiredSession
	err = tx.QueryRowContext(ctx, `SELECT thread_key, session_name, project_dir, observed_ts,
                                         last_seq, leaf_id FROM retired WHERE session_path = ?`,
		sessionPath).Scan(&retired.ThreadKey, &retired.SessionName, &retired.ProjectDir,
		&retired.ObservedTS, &retired.LastSeq, &retired.LeafID)
	if errors.Is(err, sql.ErrNoRows) || (err == nil && retired.ThreadKey != threadKey) {
		return ThreadRow{}, false, nil
	}
	if err != nil {
		return ThreadRow{}, false, fmt.Errorf("store: read retired session %s: %w", sessionPath, err)
	}
	row, err := scanThread(tx.QueryRowContext(ctx, selectThread+` WHERE thread_key = ?`, threadKey))
	if errors.Is(err, sql.ErrNoRows) {
		return ThreadRow{}, false, nil
	}
	if err != nil {
		return ThreadRow{}, false, fmt.Errorf("store: read thread %s to restore session: %w", threadKey, err)
	}
	if row.SessionPath != "" {
		return ThreadRow{}, false, nil
	}
	row.SessionName, row.SessionPath, row.SessionID = retired.SessionName, sessionPath, sessionID
	row.Cwd, row.ProjectDir = cwd, retired.ProjectDir
	row.State = StateCold
	row.LastSeq, row.LeafID = retired.LastSeq, retired.LeafID
	row.ProgressTS = ""
	row.ObservedTS = retired.ObservedTS
	if err := putThread(ctx, tx, row); err != nil {
		return ThreadRow{}, false, err
	}
	result, err := tx.ExecContext(ctx, `DELETE FROM retired WHERE session_path = ? AND thread_key = ?`, sessionPath, threadKey)
	if err != nil {
		return ThreadRow{}, false, fmt.Errorf("store: remove restored session from retired list: %w", err)
	}
	if n, err := result.RowsAffected(); err != nil || n != 1 {
		if err != nil {
			return ThreadRow{}, false, fmt.Errorf("store: confirm restored session removal: %w", err)
		}
		return ThreadRow{}, false, nil
	}
	if err := tx.Commit(); err != nil {
		return ThreadRow{}, false, fmt.Errorf("store: commit restored session: %w", err)
	}
	return row, true, nil
}

// RetiredCount reports how many sessions this thread has retired, which is what
// names the next one.
func (s *Store) RetiredCount(ctx context.Context, threadKey string) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx,
		`SELECT count(*) FROM retired WHERE thread_key = ?`, threadKey).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("store: count retired sessions of %s: %w", threadKey, err)
	}
	return n, nil
}

// RetiredProjects returns the working directories of retired sessions, so the
// sweep can leave them alone.
func (s *Store) RetiredProjects(ctx context.Context) ([]string, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT project_dir FROM retired WHERE project_dir <> ''`)
	if err != nil {
		return nil, fmt.Errorf("store: list the retired project directories: %w", err)
	}
	defer rows.Close()

	var out []string
	for rows.Next() {
		var dir string
		if err := rows.Scan(&dir); err != nil {
			return nil, fmt.Errorf("store: read a retired project directory: %w", err)
		}
		out = append(out, dir)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: list the retired project directories: %w", err)
	}
	return out, nil
}

// ChannelHasSession reports whether any thread in this channel other than
// exceptKey owns a session.
//
// It answers one question: a top-level message in a DM starts a new session, and
// whether that is worth telling the user about depends on whether the DM already
// had one (DESIGN.md §5).
func (s *Store) ChannelHasSession(ctx context.Context, workspaceID, channelID, exceptKey string) (bool, error) {
	var one int
	err := s.db.QueryRowContext(ctx, `
        SELECT 1 FROM threads
        WHERE workspace_id = ? AND channel_id = ? AND thread_key <> ?
          AND session_path <> '' AND state <> ?
        LIMIT 1`, workspaceID, channelID, exceptKey, StateDeleted).Scan(&one)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return false, nil
	case err != nil:
		return false, fmt.Errorf("store: look for a session in %s: %w", channelID, err)
	}
	return true, nil
}
