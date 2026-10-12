package store

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// ShutdownCheckpoint records the session cursor and unique notice marker so the
// next process can relay only the answer to that shutdown notice.
type ShutdownCheckpoint struct {
	ThreadKey     string
	SessionPath   string
	LeafID        string
	NoticeID      string
	NoticeEntryID string
	Posted        bool
	CreatedAt     time.Time
}

// PutShutdownCheckpoint records one session that needs a response check.
func (s *Store) PutShutdownCheckpoint(ctx context.Context, row ShutdownCheckpoint) error {
	if row.ThreadKey == "" {
		return errors.New("store: a shutdown checkpoint needs a thread key")
	}
	if row.SessionPath == "" {
		return errors.New("store: a shutdown checkpoint needs a session path")
	}
	if row.NoticeID == "" {
		return errors.New("store: a shutdown checkpoint needs a notice id")
	}
	if row.CreatedAt.IsZero() {
		row.CreatedAt = time.Now()
	}
	_, err := s.db.ExecContext(ctx, `
        INSERT INTO shutdown_pending
            (thread_key, session_path, leaf_id, notice_id, notice_entry_id, posted, created_at)
        VALUES (?, ?, ?, ?, ?, 0, ?)
        ON CONFLICT(thread_key) DO UPDATE SET
            session_path    = excluded.session_path,
            leaf_id         = excluded.leaf_id,
            notice_id       = excluded.notice_id,
            notice_entry_id = '',
            posted          = 0,
            created_at      = excluded.created_at`,
		row.ThreadKey, row.SessionPath, row.LeafID, row.NoticeID, row.NoticeEntryID, row.CreatedAt.Unix())
	if err != nil {
		return fmt.Errorf("store: save shutdown checkpoint for %s: %w", row.ThreadKey, err)
	}
	return nil
}

// ShutdownCheckpoints returns pending checks, including successful posts whose
// checkpoint cleanup still needs retrying.
func (s *Store) ShutdownCheckpoints(ctx context.Context) ([]ShutdownCheckpoint, error) {
	rows, err := s.db.QueryContext(ctx, `
        SELECT thread_key, session_path, leaf_id, notice_id, notice_entry_id, posted, created_at
        FROM shutdown_pending ORDER BY created_at`)
	if err != nil {
		return nil, fmt.Errorf("store: list shutdown checkpoints: %w", err)
	}
	defer rows.Close()

	var out []ShutdownCheckpoint
	for rows.Next() {
		var row ShutdownCheckpoint
		var createdAt int64
		var posted int
		if err := rows.Scan(&row.ThreadKey, &row.SessionPath, &row.LeafID, &row.NoticeID, &row.NoticeEntryID, &posted, &createdAt); err != nil {
			return nil, fmt.Errorf("store: read shutdown checkpoint: %w", err)
		}
		row.Posted = posted != 0
		row.CreatedAt = time.Unix(createdAt, 0).UTC()
		out = append(out, row)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: list shutdown checkpoints: %w", err)
	}
	return out, nil
}

// AdvanceShutdownCheckpoint moves the response cursor forward without
// extending the checkpoint's expiry window.
func (s *Store) AdvanceShutdownCheckpoint(ctx context.Context, threadKey, sessionPath, noticeID, leafID, noticeEntryID string) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE shutdown_pending SET leaf_id = ?, notice_entry_id = ? WHERE thread_key = ? AND session_path = ? AND notice_id = ?`,
		leafID, noticeEntryID, threadKey, sessionPath, noticeID)
	if err != nil {
		return fmt.Errorf("store: advance shutdown checkpoint for %s: %w", threadKey, err)
	}
	return nil
}

// MarkShutdownCheckpointPosted records a successful Slack post so a transient
// failure to delete the checkpoint cannot cause the response to be posted again.
func (s *Store) MarkShutdownCheckpointPosted(ctx context.Context, threadKey, sessionPath, noticeID string) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE shutdown_pending SET posted = 1 WHERE thread_key = ? AND session_path = ? AND notice_id = ?`,
		threadKey, sessionPath, noticeID)
	if err != nil {
		return fmt.Errorf("store: mark shutdown response posted for %s: %w", threadKey, err)
	}
	return nil
}

// DeleteShutdownCheckpoint forgets a checkpoint only if its session and notice
// marker still match, so a stale check cannot delete a newer shutdown cycle.
func (s *Store) DeleteShutdownCheckpoint(ctx context.Context, threadKey, sessionPath, noticeID string) error {
	_, err := s.db.ExecContext(ctx,
		`DELETE FROM shutdown_pending WHERE thread_key = ? AND session_path = ? AND notice_id = ?`, threadKey, sessionPath, noticeID)
	if err != nil {
		return fmt.Errorf("store: delete shutdown checkpoint for %s: %w", threadKey, err)
	}
	return nil
}
