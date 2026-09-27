package store

import (
	"context"
	"fmt"
	"time"
)

// ClaimEvent records that an inbound event has been handled and reports
// whether this call was the first to see it.
//
// This is mandatory rather than an optimisation (DESIGN.md §7): the gateway's
// prompt path has no idempotency key, so a platform redelivery that got past
// the transport would otherwise run the same prompt twice. The claim is the
// first write of a turn's life and it happens before the session is touched,
// so a duplicate is dropped rather than de-duplicated afterwards.
//
// An empty eventID claims nothing and reports true: a platform that sends no
// event ID gets no dedupe rather than sharing one row with every other
// identifier-less event.
func (s *Store) ClaimEvent(ctx context.Context, eventID string) (bool, error) {
	if eventID == "" {
		return true, nil
	}
	res, err := s.db.ExecContext(ctx,
		`INSERT OR IGNORE INTO seen (event_id, received_at) VALUES (?, ?)`,
		eventID, time.Now().Unix())
	if err != nil {
		return false, fmt.Errorf("store: claim event %s: %w", eventID, err)
	}
	claimed, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("store: claim event %s: %w", eventID, err)
	}
	return claimed > 0, nil
}

// PruneEvents forgets events older than the cutoff and reports how many rows
// went. Platforms retry within minutes, so a day of history is generous; the
// table stays bounded without a bounded-memory guess about the future.
func (s *Store) PruneEvents(ctx context.Context, before time.Time) (int64, error) {
	res, err := s.db.ExecContext(ctx, `DELETE FROM seen WHERE received_at < ?`, before.Unix())
	if err != nil {
		return 0, fmt.Errorf("store: prune events: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, nil // the delete happened; only the count is unavailable
	}
	return n, nil
}
