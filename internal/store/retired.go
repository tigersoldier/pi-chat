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
        INSERT INTO retired (session_path, thread_key, session_name, project_dir, retired_at)
        VALUES (?, ?, ?, ?, ?)
        ON CONFLICT(session_path) DO UPDATE SET
            thread_key   = excluded.thread_key,
            session_name = excluded.session_name,
            project_dir  = excluded.project_dir`,
		r.SessionPath, r.ThreadKey, r.SessionName, r.ProjectDir, r.RetiredAt.Unix())
	if err != nil {
		return fmt.Errorf("store: retire session %s: %w", r.SessionPath, err)
	}
	return nil
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
