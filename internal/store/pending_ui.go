package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

const pendingUISeenPrefix = "ui-route:"

// PendingUI is the durable routing record for a blocking pi dialog. RouteID is
// unique across session event streams; RequestID is the original, session-local
// gateway id needed by RespondUI. Dialog text and choices stay out of SQLite.
type PendingUI struct {
	RouteID         string
	RequestID       string
	ThreadKey       string
	RequesterUserID string
	SessionPath     string
	Method          string
	MessageTS       string
	CreatedAt       time.Time
	// ExpiresAt is zero when Pi supplied no timeout; local cleanup must not
	// expire the route before Pi stops waiting.
	ExpiresAt time.Time
}

type pendingUIQuery interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

// PutPendingUI records a dialog before it is shown. The seen marker is refreshed
// on consumption or expiry so a replayed gateway event cannot resurrect it.
func (s *Store) PutPendingUI(ctx context.Context, row PendingUI) (inserted bool, err error) {
	if row.RouteID == "" || row.RequestID == "" || row.ThreadKey == "" || row.RequesterUserID == "" || row.SessionPath == "" || row.Method == "" {
		return false, errors.New("store: a pending dialog needs a route, request, thread, requester, session path and method")
	}
	if row.CreatedAt.IsZero() {
		row.CreatedAt = time.Now()
	}
	expiresAt := int64(0)
	if !row.ExpiresAt.IsZero() {
		expiresAt = ceilUnixSecond(row.ExpiresAt)
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, fmt.Errorf("store: begin pending route %s: %w", row.RouteID, err)
	}
	defer tx.Rollback()

	previous, found, err := readPendingUI(ctx, tx, row.RouteID)
	if err != nil {
		return false, err
	}
	if found {
		if previous.RequestID != row.RequestID || previous.ThreadKey != row.ThreadKey ||
			previous.RequesterUserID != row.RequesterUserID || previous.SessionPath != row.SessionPath || previous.Method != row.Method {
			return false, fmt.Errorf("store: pending route %s belongs to a different dialog", row.RouteID)
		}
		if err := tx.Commit(); err != nil {
			return false, fmt.Errorf("store: finish duplicate pending route %s: %w", row.RouteID, err)
		}
		return false, nil
	}

	seenID := pendingUISeenPrefix + row.RouteID
	var seen int
	err = tx.QueryRowContext(ctx, `SELECT 1 FROM seen WHERE event_id = ?`, seenID).Scan(&seen)
	if err == nil {
		if err := tx.Commit(); err != nil {
			return false, fmt.Errorf("store: finish seen pending route %s: %w", row.RouteID, err)
		}
		return false, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return false, fmt.Errorf("store: check pending route replay %s: %w", row.RouteID, err)
	}

	_, err = tx.ExecContext(ctx, `
        INSERT INTO pending_ui
            (route_id, request_id, thread_key, requester_user_id, session_path, method, message_ts, created_at, expires_at)
        VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		row.RouteID, row.RequestID, row.ThreadKey, row.RequesterUserID, row.SessionPath, row.Method, row.MessageTS,
		row.CreatedAt.Unix(), expiresAt)
	if err != nil {
		return false, fmt.Errorf("store: save pending route %s: %w", row.RouteID, err)
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT OR IGNORE INTO seen (event_id, received_at) VALUES (?, ?)`, seenID, time.Now().Unix()); err != nil {
		return false, fmt.Errorf("store: record pending route replay key %s: %w", row.RouteID, err)
	}
	if err := tx.Commit(); err != nil {
		return false, fmt.Errorf("store: commit pending route %s: %w", row.RouteID, err)
	}
	return true, nil
}

// PendingUI returns one dialog's route. The second result is false if it has
// already been consumed or is absent; callers check expiry separately.
func (s *Store) PendingUI(ctx context.Context, routeID string) (PendingUI, bool, error) {
	return readPendingUI(ctx, s.db, routeID)
}

func readPendingUI(ctx context.Context, query pendingUIQuery, routeID string) (PendingUI, bool, error) {
	var row PendingUI
	var createdAt, expiresAt int64
	err := query.QueryRowContext(ctx, `
        SELECT route_id, request_id, thread_key, requester_user_id, session_path, method, message_ts, created_at, expires_at
        FROM pending_ui WHERE route_id = ?`, routeID).Scan(
		&row.RouteID, &row.RequestID, &row.ThreadKey, &row.RequesterUserID, &row.SessionPath, &row.Method, &row.MessageTS, &createdAt, &expiresAt)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return PendingUI{}, false, nil
	case err != nil:
		return PendingUI{}, false, fmt.Errorf("store: read pending route %s: %w", routeID, err)
	}
	row.CreatedAt = time.Unix(createdAt, 0).UTC()
	if expiresAt != 0 {
		row.ExpiresAt = time.Unix(expiresAt, 0).UTC()
	}
	return row, true, nil
}

// SetPendingUIMessage records the Slack message that contains the dialog's
// buttons, so the adapter can remove them after an answer.
func (s *Store) SetPendingUIMessage(ctx context.Context, routeID, messageTS string) error {
	result, err := s.db.ExecContext(ctx,
		`UPDATE pending_ui SET message_ts = ? WHERE route_id = ?`, messageTS, routeID)
	if err != nil {
		return fmt.Errorf("store: save message for pending route %s: %w", routeID, err)
	}
	if count, err := result.RowsAffected(); err == nil && count == 0 {
		return ErrNoPendingUI
	}
	return nil
}

// TakePendingUI atomically consumes one dialog, ensuring a retry cannot answer
// the same pi request twice. Its replay marker is refreshed for one day after
// consumption, even when the dialog itself stayed pending longer than a day.
func (s *Store) TakePendingUI(ctx context.Context, routeID string) (PendingUI, bool, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return PendingUI{}, false, fmt.Errorf("store: begin consuming pending route %s: %w", routeID, err)
	}
	defer tx.Rollback()

	var row PendingUI
	var createdAt, expiresAt int64
	err = tx.QueryRowContext(ctx, `
        DELETE FROM pending_ui WHERE route_id = ?
        RETURNING route_id, request_id, thread_key, requester_user_id, session_path, method, message_ts, created_at, expires_at`,
		routeID).Scan(&row.RouteID, &row.RequestID, &row.ThreadKey, &row.RequesterUserID, &row.SessionPath, &row.Method,
		&row.MessageTS, &createdAt, &expiresAt)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return PendingUI{}, false, nil
	case err != nil:
		return PendingUI{}, false, fmt.Errorf("store: consume pending route %s: %w", routeID, err)
	}
	if _, err := tx.ExecContext(ctx, `
        INSERT OR REPLACE INTO seen (event_id, received_at) VALUES (?, ?)`,
		pendingUISeenPrefix+routeID, time.Now().Unix()); err != nil {
		return PendingUI{}, false, fmt.Errorf("store: refresh pending route replay key %s: %w", routeID, err)
	}
	if err := tx.Commit(); err != nil {
		return PendingUI{}, false, fmt.Errorf("store: commit consumed pending route %s: %w", routeID, err)
	}
	row.CreatedAt = time.Unix(createdAt, 0).UTC()
	if expiresAt != 0 {
		row.ExpiresAt = time.Unix(expiresAt, 0).UTC()
	}
	return row, true, nil
}

// UnownedPendingUI lists pre-migration routes whose requester is unknown. They
// must be canceled, never answered, and remain available for retry if the
// gateway cannot be reached during cleanup.
func (s *Store) UnownedPendingUI(ctx context.Context) ([]PendingUI, error) {
	rows, err := s.db.QueryContext(ctx, `
        SELECT route_id, request_id, thread_key, requester_user_id, session_path, method, message_ts, created_at, expires_at
        FROM pending_ui WHERE requester_user_id = '' ORDER BY created_at`)
	if err != nil {
		return nil, fmt.Errorf("store: list unowned pending dialogs: %w", err)
	}
	defer rows.Close()

	var pending []PendingUI
	for rows.Next() {
		var row PendingUI
		var createdAt, expiresAt int64
		if err := rows.Scan(&row.RouteID, &row.RequestID, &row.ThreadKey, &row.RequesterUserID, &row.SessionPath, &row.Method,
			&row.MessageTS, &createdAt, &expiresAt); err != nil {
			return nil, fmt.Errorf("store: read an unowned pending dialog: %w", err)
		}
		row.CreatedAt = time.Unix(createdAt, 0).UTC()
		if expiresAt != 0 {
			row.ExpiresAt = time.Unix(expiresAt, 0).UTC()
		}
		pending = append(pending, row)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: list unowned pending dialogs: %w", err)
	}
	return pending, nil
}

// TakeExpiredPendingUI removes expired routes and refreshes their replay markers.
// The caller marks still-pending Slack messages expired outside the DB call.
func (s *Store) TakeExpiredPendingUI(ctx context.Context, now time.Time) ([]PendingUI, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("store: begin expiring pending dialogs: %w", err)
	}
	defer tx.Rollback()

	if _, err := tx.ExecContext(ctx, `
        INSERT OR REPLACE INTO seen (event_id, received_at)
        SELECT ? || route_id, ? FROM pending_ui WHERE expires_at > 0 AND expires_at <= ?`,
		pendingUISeenPrefix, now.Unix(), now.Unix()); err != nil {
		return nil, fmt.Errorf("store: refresh expired dialog replay keys: %w", err)
	}
	rows, err := tx.QueryContext(ctx, `
        DELETE FROM pending_ui WHERE expires_at > 0 AND expires_at <= ?
        RETURNING route_id, request_id, thread_key, requester_user_id, session_path, method, message_ts, created_at, expires_at`, now.Unix())
	if err != nil {
		return nil, fmt.Errorf("store: expire pending dialogs: %w", err)
	}

	var expired []PendingUI
	for rows.Next() {
		var row PendingUI
		var createdAt, expiresAt int64
		if err := rows.Scan(&row.RouteID, &row.RequestID, &row.ThreadKey, &row.RequesterUserID, &row.SessionPath, &row.Method,
			&row.MessageTS, &createdAt, &expiresAt); err != nil {
			return nil, fmt.Errorf("store: read an expired dialog: %w", err)
		}
		row.CreatedAt = time.Unix(createdAt, 0).UTC()
		row.ExpiresAt = time.Unix(expiresAt, 0).UTC()
		expired = append(expired, row)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: list expired dialogs: %w", err)
	}
	if err := rows.Close(); err != nil {
		return nil, fmt.Errorf("store: close expired dialog rows: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("store: commit expired dialogs: %w", err)
	}
	return expired, nil
}

func ceilUnixSecond(t time.Time) int64 {
	seconds := t.Unix()
	if t.Nanosecond() != 0 {
		seconds++
	}
	return seconds
}

// ErrNoPendingUI reports an update to a dialog that is no longer pending.
var ErrNoPendingUI = errors.New("no such pending dialog")
