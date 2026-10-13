package store

import (
	"context"
	"testing"
	"time"
)

func TestShutdownCheckpointRoundTripAndDelete(t *testing.T) {
	ctx := context.Background()
	s, _ := newStore(t)
	want := ShutdownCheckpoint{
		ThreadKey:   "T1:C1:1700000000.000100",
		SessionPath: "/sessions/a.jsonl",
		LeafID:      "leaf-41",
		NoticeID:    "notice-1",
		CreatedAt:   time.Unix(1_700_000_000, 0).UTC(),
	}
	if err := s.PutShutdownCheckpoint(ctx, want); err != nil {
		t.Fatalf("PutShutdownCheckpoint: %v", err)
	}

	rows, err := s.ShutdownCheckpoints(ctx)
	if err != nil {
		t.Fatalf("ShutdownCheckpoints: %v", err)
	}
	if len(rows) != 1 || rows[0] != want {
		t.Fatalf("ShutdownCheckpoints = %+v, want [%+v]", rows, want)
	}

	updated := want
	updated.LeafID = "leaf-55"
	updated.NoticeID = "notice-2"
	if err := s.PutShutdownCheckpoint(ctx, updated); err != nil {
		t.Fatalf("PutShutdownCheckpoint (update): %v", err)
	}
	rows, err = s.ShutdownCheckpoints(ctx)
	if err != nil || len(rows) != 1 || rows[0].LeafID != updated.LeafID {
		t.Fatalf("updated checkpoints = %+v, err=%v", rows, err)
	}
	if err := s.AdvanceShutdownCheckpoint(ctx, want.ThreadKey, want.SessionPath, updated.NoticeID, "leaf-57", "notice-entry-1"); err != nil {
		t.Fatalf("AdvanceShutdownCheckpoint: %v", err)
	}
	rows, err = s.ShutdownCheckpoints(ctx)
	if err != nil || len(rows) != 1 || rows[0].LeafID != "leaf-57" || rows[0].NoticeEntryID != "notice-entry-1" || !rows[0].CreatedAt.Equal(want.CreatedAt) {
		t.Fatalf("advanced checkpoint = %+v, err=%v", rows, err)
	}

	if err := s.MarkShutdownCheckpointPosted(ctx, want.ThreadKey, want.SessionPath, updated.NoticeID); err != nil {
		t.Fatalf("MarkShutdownCheckpointPosted: %v", err)
	}
	rows, err = s.ShutdownCheckpoints(ctx)
	if err != nil || len(rows) != 1 || !rows[0].Posted {
		t.Fatalf("posted checkpoint = %+v, err=%v", rows, err)
	}
	replacement := updated
	replacement.NoticeID = "notice-3"
	if err := s.PutShutdownCheckpoint(ctx, replacement); err != nil {
		t.Fatalf("PutShutdownCheckpoint (replacement): %v", err)
	}
	rows, err = s.ShutdownCheckpoints(ctx)
	if err != nil || len(rows) != 1 || rows[0].Posted || rows[0].NoticeEntryID != "" || rows[0].NoticeID != replacement.NoticeID {
		t.Fatalf("replacement checkpoint = %+v, err=%v", rows, err)
	}

	if err := s.DeleteShutdownCheckpoint(ctx, want.ThreadKey, "/sessions/other.jsonl", replacement.NoticeID); err != nil {
		t.Fatalf("DeleteShutdownCheckpoint with stale path: %v", err)
	}
	if err := s.DeleteShutdownCheckpoint(ctx, want.ThreadKey, want.SessionPath, want.NoticeID); err != nil {
		t.Fatalf("DeleteShutdownCheckpoint with stale notice id: %v", err)
	}
	rows, err = s.ShutdownCheckpoints(ctx)
	if err != nil || len(rows) != 1 {
		t.Fatalf("stale checkpoint delete removed the replacement: rows=%+v err=%v", rows, err)
	}
	if err := s.DeleteShutdownCheckpoint(ctx, want.ThreadKey, want.SessionPath, replacement.NoticeID); err != nil {
		t.Fatalf("DeleteShutdownCheckpoint: %v", err)
	}
	rows, err = s.ShutdownCheckpoints(ctx)
	if err != nil || len(rows) != 0 {
		t.Fatalf("checkpoints after delete = %+v, err=%v; want empty", rows, err)
	}
}

func TestPutShutdownCheckpointRequiresIdentity(t *testing.T) {
	s, _ := newStore(t)
	if err := s.PutShutdownCheckpoint(context.Background(), ShutdownCheckpoint{SessionPath: "/sessions/a.jsonl"}); err == nil {
		t.Fatal("PutShutdownCheckpoint accepted an empty thread key")
	}
	if err := s.PutShutdownCheckpoint(context.Background(), ShutdownCheckpoint{ThreadKey: "T1:C1:1.1"}); err == nil {
		t.Fatal("PutShutdownCheckpoint accepted an empty session path")
	}
	if err := s.PutShutdownCheckpoint(context.Background(), ShutdownCheckpoint{ThreadKey: "T1:C1:1.1", SessionPath: "/sessions/a.jsonl"}); err == nil {
		t.Fatal("PutShutdownCheckpoint accepted an empty notice id")
	}
}
