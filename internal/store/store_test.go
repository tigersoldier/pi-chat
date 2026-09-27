package store

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

// newStore opens a store in a temporary directory. The path is returned so a
// test can reopen it.
func newStore(t *testing.T) (*Store, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "state", "pi-chat.db")
	s, err := Open(context.Background(), path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s, path
}

func sampleThread() ThreadRow {
	now := time.Unix(1_700_000_000, 0).UTC()
	return ThreadRow{
		ThreadKey:   "T1:C1:1700000000.000100",
		WorkspaceID: "T1",
		ChannelID:   "C1",
		ThreadTS:    "1700000000.000100",
		SessionName: "slack-t1-c1-1700000000-000100",
		SessionPath: "/sessions/a.jsonl",
		SessionID:   "abc",
		Cwd:         "/home/pi/work/2026-09-27-fix-tests",
		ProjectDir:  "/home/pi/work/2026-09-27-fix-tests",
		State:       StateWarm,
		LastSeq:     41,
		LeafID:      "leaf-41",
		ProgressTS:  "1700000000.000200",
		CreatedAt:   now,
		LastActive:  now,
	}
}

func TestThreadRoundTrip(t *testing.T) {
	ctx := context.Background()
	s, _ := newStore(t)
	want := sampleThread()

	if err := s.PutThread(ctx, want); err != nil {
		t.Fatalf("PutThread: %v", err)
	}
	got, found, err := s.Thread(ctx, want.ThreadKey)
	if err != nil {
		t.Fatalf("Thread: %v", err)
	}
	if !found {
		t.Fatal("Thread did not find the row it just wrote")
	}
	if got != want {
		t.Errorf("round trip changed the row:\n got %+v\nwant %+v", got, want)
	}

	// The upsert is the only write path, so a second Put must replace rather
	// than duplicate.
	want.State = "cold"
	want.LastSeq = 55
	want.LastActive = want.LastActive.Add(time.Hour)
	if err := s.PutThread(ctx, want); err != nil {
		t.Fatalf("PutThread (update): %v", err)
	}
	got, _, err = s.Thread(ctx, want.ThreadKey)
	if err != nil {
		t.Fatalf("Thread: %v", err)
	}
	if got.State != "cold" || got.LastSeq != 55 || !got.LastActive.Equal(want.LastActive) {
		t.Errorf("the update did not take: %+v", got)
	}
	threads, err := s.Threads(ctx)
	if err != nil {
		t.Fatalf("Threads: %v", err)
	}
	if len(threads) != 1 {
		t.Fatalf("Threads returned %d rows, want 1", len(threads))
	}
}

func TestThreadUnknownKey(t *testing.T) {
	s, _ := newStore(t)
	_, found, err := s.Thread(context.Background(), "T1:C9:missing")
	if err != nil {
		t.Fatalf("Thread: %v", err)
	}
	if found {
		t.Fatal("Thread reported a row for a key that was never written")
	}
}

func TestPutThreadNeedsAKey(t *testing.T) {
	s, _ := newStore(t)
	if err := s.PutThread(context.Background(), ThreadRow{}); err == nil {
		t.Fatal("PutThread accepted a row with no key")
	}
}

func TestSetThreadStateAndCursor(t *testing.T) {
	ctx := context.Background()
	s, _ := newStore(t)
	row := sampleThread()
	if err := s.PutThread(ctx, row); err != nil {
		t.Fatalf("PutThread: %v", err)
	}

	if err := s.SetThreadState(ctx, row.ThreadKey, StateCold); err != nil {
		t.Fatalf("SetThreadState: %v", err)
	}
	if err := s.SetThreadCursor(ctx, row.ThreadKey, 99, "leaf-99"); err != nil {
		t.Fatalf("SetThreadCursor: %v", err)
	}
	if err := s.SetProgressTS(ctx, row.ThreadKey, "1700000000.000300"); err != nil {
		t.Fatalf("SetProgressTS: %v", err)
	}

	got, _, err := s.Thread(ctx, row.ThreadKey)
	if err != nil {
		t.Fatalf("Thread: %v", err)
	}
	if got.State != StateCold || got.LastSeq != 99 || got.LeafID != "leaf-99" {
		t.Errorf("state or cursor not stored: %+v", got)
	}
	if got.ProgressTS != "1700000000.000300" {
		t.Errorf("progress ts = %q", got.ProgressTS)
	}
	if !got.LastActive.After(row.LastActive) {
		t.Errorf("a state change should count as activity: %v", got.LastActive)
	}
	// The session identity must survive a state change.
	if got.SessionPath != row.SessionPath || got.ProjectDir != row.ProjectDir {
		t.Errorf("a state change lost the session identity: %+v", got)
	}
}

func TestSetThreadStateRejectsUnknownState(t *testing.T) {
	s, _ := newStore(t)
	row := sampleThread()
	if err := s.PutThread(context.Background(), row); err != nil {
		t.Fatalf("PutThread: %v", err)
	}
	if err := s.SetThreadState(context.Background(), row.ThreadKey, "hibernating"); err == nil {
		t.Fatal("SetThreadState accepted a state that is not warm, cold or deleted")
	}
}

func TestMarkThreadsCold(t *testing.T) {
	ctx := context.Background()
	s, _ := newStore(t)
	warm := sampleThread()
	cold := sampleThread()
	cold.ThreadKey = "T1:C1:1700000000.000200"
	cold.ThreadTS = "1700000000.000200"
	cold.State = StateCold
	for _, row := range []ThreadRow{warm, cold} {
		if err := s.PutThread(ctx, row); err != nil {
			t.Fatalf("PutThread: %v", err)
		}
	}

	n, err := s.MarkThreadsCold(ctx)
	if err != nil {
		t.Fatalf("MarkThreadsCold: %v", err)
	}
	if n != 1 {
		t.Errorf("MarkThreadsCold changed %d rows, want 1", n)
	}
	if got, _, _ := s.Thread(ctx, warm.ThreadKey); got.State != StateCold {
		t.Errorf("the warm row is still %q after startup", got.State)
	}
	// Running it again is a no-op.
	if n, err := s.MarkThreadsCold(ctx); err != nil || n != 0 {
		t.Errorf("second MarkThreadsCold = (%d, %v), want (0, nil)", n, err)
	}
}

func TestClaimEventOnlyFirstCallWins(t *testing.T) {
	ctx := context.Background()
	s, _ := newStore(t)

	first, err := s.ClaimEvent(ctx, "Ev123")
	if err != nil {
		t.Fatalf("ClaimEvent: %v", err)
	}
	if !first {
		t.Fatal("the first claim should win")
	}
	second, err := s.ClaimEvent(ctx, "Ev123")
	if err != nil {
		t.Fatalf("ClaimEvent (retry): %v", err)
	}
	if second {
		t.Fatal("a redelivered event claimed the turn twice")
	}
	// A different event is unaffected.
	if other, err := s.ClaimEvent(ctx, "Ev124"); err != nil || !other {
		t.Fatalf("another event = (%v, %v), want (true, nil)", other, err)
	}
	// An event with no ID cannot be deduped, and must not block the next one.
	if claim, err := s.ClaimEvent(ctx, ""); err != nil || !claim {
		t.Fatalf("empty id = (%v, %v), want (true, nil)", claim, err)
	}
	if claim, err := s.ClaimEvent(ctx, ""); err != nil || !claim {
		t.Fatalf("empty id twice = (%v, %v), want (true, nil)", claim, err)
	}
}

func TestPruneEvents(t *testing.T) {
	ctx := context.Background()
	s, _ := newStore(t)
	if _, err := s.ClaimEvent(ctx, "old"); err != nil {
		t.Fatalf("ClaimEvent: %v", err)
	}
	// Backdate one row past the cutoff.
	if _, err := s.db.ExecContext(ctx,
		`UPDATE seen SET received_at = ? WHERE event_id = ?`, time.Now().Add(-48*time.Hour).Unix(), "old"); err != nil {
		t.Fatalf("backdate: %v", err)
	}
	if _, err := s.ClaimEvent(ctx, "new"); err != nil {
		t.Fatalf("ClaimEvent: %v", err)
	}

	n, err := s.PruneEvents(ctx, time.Now().Add(-24*time.Hour))
	if err != nil {
		t.Fatalf("PruneEvents: %v", err)
	}
	if n != 1 {
		t.Errorf("pruned %d rows, want 1", n)
	}
	// The fresh event survives, so a late retry is still deduped.
	if claim, err := s.ClaimEvent(ctx, "new"); err != nil || claim {
		t.Errorf("the surviving event = (%v, %v), want (false, nil)", claim, err)
	}
}

func TestDedupeAndCursorSurviveAReopen(t *testing.T) {
	// The claim is the only guard against a platform retry running a prompt
	// twice, and the cursor is what makes a restart resume instead of replaying,
	// so both have to outlive the process that wrote them (DESIGN.md §7).
	ctx := context.Background()
	s, path := newStore(t)
	row := sampleThread()
	if err := s.PutThread(ctx, row); err != nil {
		t.Fatalf("PutThread: %v", err)
	}
	if claimed, err := s.ClaimEvent(ctx, "Ev-reopen"); err != nil || !claimed {
		t.Fatalf("ClaimEvent = (%v, %v), want (true, nil)", claimed, err)
	}
	if err := s.SetThreadCursor(ctx, row.ThreadKey, 99, "leaf-99"); err != nil {
		t.Fatalf("SetThreadCursor: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	again, err := Open(ctx, path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	t.Cleanup(func() { _ = again.Close() })

	if claimed, err := again.ClaimEvent(ctx, "Ev-reopen"); err != nil || claimed {
		t.Fatalf("a claim did not survive the reopen: (%v, %v)", claimed, err)
	}
	got, found, err := again.Thread(ctx, row.ThreadKey)
	if err != nil || !found {
		t.Fatalf("the thread did not survive the reopen: found=%v err=%v", found, err)
	}
	if got.LastSeq != 99 || got.LeafID != "leaf-99" {
		t.Errorf("the cursor did not survive the reopen: seq=%d leaf=%q", got.LastSeq, got.LeafID)
	}
	if got.SessionPath != row.SessionPath || got.ProgressTS != row.ProgressTS {
		t.Errorf("the session identity did not survive the reopen: %+v", got)
	}
}

func TestOpenNeedsAPath(t *testing.T) {
	if _, err := Open(context.Background(), ""); err == nil {
		t.Fatal("Open accepted an empty path")
	}
}

func TestOpenRejectsANewerSchema(t *testing.T) {
	ctx := context.Background()
	s, path := newStore(t)
	if _, err := s.db.ExecContext(ctx, "PRAGMA user_version = 99"); err != nil {
		t.Fatalf("bump the version: %v", err)
	}
	_ = s.Close()

	if _, err := Open(ctx, path); err == nil {
		t.Fatal("Open accepted a database from a newer pi-chat")
	}
}
