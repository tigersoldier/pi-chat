package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// Surface state is the small durable half of a chat adapter that has no
// threads of its own to hang state on (DESIGN.md §12). The self-DM surface uses
// it for two things:
//
//   - a cursor: where the poller got to in the conversation, so a restart does
//     not replay a personal DM as commands;
//   - a posted ledger: which messages the daemon itself posted. In the self-DM
//     the daemon writes as the same user it reads, so authorship cannot tell
//     the agent's answer from the human's request — the ledger is what does.
//
// Both are deliberately untyped: the store keeps the surface's own bookkeeping
// out of the schema's chat-shaped tables, and a second surface gets its own
// namespace with no migration.

// SurfaceKV reads one value. The second result is false when the key is
// unknown, which is a normal answer rather than an error.
func (s *Store) SurfaceKV(ctx context.Context, surface, key string) (string, bool, error) {
	var value string
	err := s.db.QueryRowContext(ctx,
		`SELECT value FROM surface_kv WHERE surface = ? AND key = ?`, surface, key).Scan(&value)
	switch {
	case err == nil:
		return value, true, nil
	case errors.Is(err, sql.ErrNoRows):
		return "", false, nil
	default:
		return "", false, fmt.Errorf("store: read %s/%s: %w", surface, key, err)
	}
}

// SetSurfaceKV writes one value, replacing any earlier one.
func (s *Store) SetSurfaceKV(ctx context.Context, surface, key, value string) error {
	_, err := s.db.ExecContext(ctx, `
        INSERT INTO surface_kv (surface, key, value, updated_at)
        VALUES (?, ?, ?, ?)
        ON CONFLICT(surface, key) DO UPDATE SET value = excluded.value, updated_at = excluded.updated_at`,
		surface, key, value, time.Now().Unix())
	if err != nil {
		return fmt.Errorf("store: write %s/%s: %w", surface, key, err)
	}
	return nil
}

// MarkSurfacePosted records a message the daemon posted as itself. The ledger
// is what the surface's reader consults before treating a message as input.
func (s *Store) MarkSurfacePosted(ctx context.Context, surface, ts string) error {
	if ts == "" {
		return nil
	}
	_, err := s.db.ExecContext(ctx, `
        INSERT INTO surface_posted (surface, ts, posted_at) VALUES (?, ?, ?)
        ON CONFLICT(surface, ts) DO NOTHING`,
		surface, ts, time.Now().Unix())
	if err != nil {
		return fmt.Errorf("store: mark %s/%s posted: %w", surface, ts, err)
	}
	return nil
}

// SurfacePosted reports whether the daemon posted the message with this ts.
func (s *Store) SurfacePosted(ctx context.Context, surface, ts string) (bool, error) {
	var found string
	err := s.db.QueryRowContext(ctx,
		`SELECT ts FROM surface_posted WHERE surface = ? AND ts = ?`, surface, ts).Scan(&found)
	switch {
	case err == nil:
		return true, nil
	case errors.Is(err, sql.ErrNoRows):
		return false, nil
	default:
		return false, fmt.Errorf("store: read %s posted %s: %w", surface, ts, err)
	}
}

// PruneSurfacePosted forgets posted-ledger entries older than the cutoff. The
// ledger only has to outlive the window in which a message could be read back
// after a restart — the cursor has moved past it long before.
func (s *Store) PruneSurfacePosted(ctx context.Context, before time.Time) (int64, error) {
	result, err := s.db.ExecContext(ctx,
		`DELETE FROM surface_posted WHERE posted_at < ?`, before.Unix())
	if err != nil {
		return 0, fmt.Errorf("store: prune the posted ledger: %w", err)
	}
	n, err := result.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("store: prune the posted ledger: %w", err)
	}
	return n, nil
}
