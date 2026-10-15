package store

import (
	"context"
	"testing"
	"time"
)

func TestPendingUIDeduplicatesAndCanBeConsumedOnce(t *testing.T) {
	ctx := context.Background()
	s, _ := newStore(t)
	now := time.Now().UTC().Truncate(time.Second)
	want := PendingUI{
		RouteID: "route-1", RequestID: "dialog-1", ThreadKey: "T1:C1:1.1", SessionPath: "/sessions/a.jsonl",
		Method: "confirm", CreatedAt: now, ExpiresAt: now.Add(time.Minute),
	}
	inserted, err := s.PutPendingUI(ctx, want)
	if err != nil || !inserted {
		t.Fatalf("PutPendingUI = inserted %v, err %v; want a new row", inserted, err)
	}
	inserted, err = s.PutPendingUI(ctx, want)
	if err != nil || inserted {
		t.Fatalf("duplicate PutPendingUI = inserted %v, err %v; want an idempotent no-op", inserted, err)
	}
	if _, err := s.PutPendingUI(ctx, PendingUI{
		RouteID: want.RouteID, RequestID: want.RequestID, ThreadKey: want.ThreadKey,
		SessionPath: "/sessions/other.jsonl", Method: want.Method,
	}); err == nil {
		t.Fatal("a reused route id from another session was accepted")
	}
	if err := s.SetPendingUIMessage(ctx, want.RouteID, "2.2"); err != nil {
		t.Fatalf("SetPendingUIMessage: %v", err)
	}
	got, found, err := s.PendingUI(ctx, want.RouteID)
	if err != nil || !found {
		t.Fatalf("PendingUI = found %v, err %v", found, err)
	}
	if got.RouteID != want.RouteID || got.RequestID != want.RequestID || got.ThreadKey != want.ThreadKey || got.SessionPath != want.SessionPath ||
		got.Method != want.Method || got.MessageTS != "2.2" || !got.ExpiresAt.Equal(want.ExpiresAt) {
		t.Errorf("PendingUI = %+v, want the persisted route %+v", got, want)
	}

	if _, err := s.db.ExecContext(ctx, `UPDATE seen SET received_at = ? WHERE event_id = ?`, now.Add(-48*time.Hour).Unix(), pendingUISeenPrefix+want.RouteID); err != nil {
		t.Fatalf("age pending replay marker: %v", err)
	}
	consumed, found, err := s.TakePendingUI(ctx, want.RouteID)
	if err != nil || !found {
		t.Fatalf("TakePendingUI = found %v, err %v", found, err)
	}
	if consumed.MessageTS != "2.2" || consumed.SessionPath != want.SessionPath || consumed.RequestID != want.RequestID {
		t.Errorf("consumed row = %+v", consumed)
	}
	var consumedAt int64
	if err := s.db.QueryRowContext(ctx, `SELECT received_at FROM seen WHERE event_id = ?`, pendingUISeenPrefix+want.RouteID).Scan(&consumedAt); err != nil {
		t.Fatalf("read refreshed replay marker: %v", err)
	}
	if consumedAt < time.Now().Add(-time.Second).Unix() {
		t.Errorf("consumed route marker was not refreshed: %d", consumedAt)
	}
	if _, found, err := s.TakePendingUI(ctx, want.RouteID); err != nil || found {
		t.Errorf("second TakePendingUI = found %v, err %v; want absent", found, err)
	}
	if inserted, err := s.PutPendingUI(ctx, want); err != nil || inserted {
		t.Errorf("replayed completed route = inserted %v, err %v; want a deduplicated no-op", inserted, err)
	}
	if _, found, err := s.PendingUI(ctx, want.RouteID); err != nil || found {
		t.Errorf("completed route was resurrected: found=%v err=%v", found, err)
	}
}

func TestPendingUIAllowsSessionLocalRequestIDsToRepeat(t *testing.T) {
	ctx := context.Background()
	s, _ := newStore(t)
	for _, row := range []PendingUI{
		{RouteID: "route-a", RequestID: "ui-1", ThreadKey: "T1:C1:1.1", SessionPath: "/s/a", Method: "confirm"},
		{RouteID: "route-b", RequestID: "ui-1", ThreadKey: "T1:C2:2.2", SessionPath: "/s/b", Method: "confirm"},
	} {
		if inserted, err := s.PutPendingUI(ctx, row); err != nil || !inserted {
			t.Fatalf("PutPendingUI(%s) = inserted %v, err %v", row.RouteID, inserted, err)
		}
	}
}

func TestTakeExpiredPendingUIDoesNotConsumeLiveRows(t *testing.T) {
	ctx := context.Background()
	s, _ := newStore(t)
	now := time.Now().UTC().Truncate(time.Second)
	for _, row := range []PendingUI{
		{RouteID: "expired", RequestID: "ui-expired", ThreadKey: "T1:C1:1.1", SessionPath: "/s/1", Method: "confirm", CreatedAt: now.Add(-time.Minute), ExpiresAt: now.Add(-time.Second)},
		{RouteID: "live", RequestID: "ui-live", ThreadKey: "T1:C1:2.2", SessionPath: "/s/2", Method: "input", CreatedAt: now, ExpiresAt: now.Add(time.Minute)},
	} {
		if _, err := s.PutPendingUI(ctx, row); err != nil {
			t.Fatalf("PutPendingUI(%s): %v", row.RouteID, err)
		}
	}
	if _, err := s.db.ExecContext(ctx, `UPDATE seen SET received_at = ? WHERE event_id = ?`, now.Add(-48*time.Hour).Unix(), pendingUISeenPrefix+"expired"); err != nil {
		t.Fatalf("age expired replay marker: %v", err)
	}
	expired, err := s.TakeExpiredPendingUI(ctx, now)
	if err != nil {
		t.Fatalf("TakeExpiredPendingUI: %v", err)
	}
	if len(expired) != 1 || expired[0].RouteID != "expired" {
		t.Fatalf("expired rows = %+v, want only the expired dialog", expired)
	}
	if _, found, err := s.PendingUI(ctx, "live"); err != nil || !found {
		t.Errorf("live dialog found=%v err=%v, want it to remain pending", found, err)
	}
	var expiredAt int64
	if err := s.db.QueryRowContext(ctx, `SELECT received_at FROM seen WHERE event_id = ?`, pendingUISeenPrefix+"expired").Scan(&expiredAt); err != nil {
		t.Fatalf("read refreshed expired replay marker: %v", err)
	}
	if expiredAt < now.Unix() {
		t.Errorf("expired route marker was not refreshed: %d, want at least %d", expiredAt, now.Unix())
	}
	if inserted, err := s.PutPendingUI(ctx, expired[0]); err != nil || inserted {
		t.Errorf("replayed expired route = inserted %v, err %v; want marker-based dedupe", inserted, err)
	}
}

func TestPendingUIExpiryRoundsUpAndDoesNotExpireEarly(t *testing.T) {
	ctx := context.Background()
	s, _ := newStore(t)
	base := time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC)
	pending := PendingUI{
		RouteID: "fractional-expiry", RequestID: "ui-fractional", ThreadKey: "T1:C1:1.1",
		SessionPath: "/s/1", Method: "confirm", CreatedAt: base, ExpiresAt: base.Add(1500 * time.Millisecond),
	}
	if inserted, err := s.PutPendingUI(ctx, pending); err != nil || !inserted {
		t.Fatalf("PutPendingUI = inserted %v, err %v", inserted, err)
	}
	got, found, err := s.PendingUI(ctx, pending.RouteID)
	if err != nil || !found {
		t.Fatalf("PendingUI = found %v, err %v", found, err)
	}
	wantExpiry := base.Add(2 * time.Second)
	if !got.ExpiresAt.Equal(wantExpiry) || got.ExpiresAt.Before(pending.ExpiresAt) {
		t.Fatalf("stored expiry = %s, want no earlier than %s", got.ExpiresAt, pending.ExpiresAt)
	}
	if expired, err := s.TakeExpiredPendingUI(ctx, wantExpiry.Add(-time.Nanosecond)); err != nil || len(expired) != 0 {
		t.Fatalf("route expired before its stored deadline: rows=%+v err=%v", expired, err)
	}
	if expired, err := s.TakeExpiredPendingUI(ctx, wantExpiry); err != nil || len(expired) != 1 {
		t.Fatalf("route at its stored deadline: rows=%+v err=%v", expired, err)
	}
}
