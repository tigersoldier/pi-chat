package store

import (
	"context"
	"testing"
	"time"
)

func TestRetiredSessionKeepsItsIdentityAndDirectory(t *testing.T) {
	ctx := context.Background()
	s, _ := newStore(t)

	retired := RetiredSession{
		SessionPath: "/sessions/first.jsonl",
		ThreadKey:   "T1:C1:1.1",
		SessionName: "slack-t1-c1-1-1",
		ProjectDir:  "/work/2026-09-27-fix-tests",
		ObservedTS:  "1700000000.000500",
		LastSeq:     12,
		LeafID:      "leaf-12",
		RetiredAt:   time.Unix(1_700_000_000, 0).UTC(),
	}
	if err := s.RetireSession(ctx, retired); err != nil {
		t.Fatalf("RetireSession: %v", err)
	}

	// The directory is what the startup sweep reads: a directory no row accounts
	// for is the remains of a crash, and this one is somebody's work.
	projects, err := s.RetiredProjects(ctx)
	if err != nil {
		t.Fatalf("RetiredProjects: %v", err)
	}
	if len(projects) != 1 || projects[0] != retired.ProjectDir {
		t.Errorf("retired projects = %v, want the one directory", projects)
	}
	if n, err := s.RetiredCount(ctx, retired.ThreadKey); err != nil || n != 1 {
		t.Errorf("RetiredCount = %d (err %v), want 1", n, err)
	}
	got, err := s.RetiredSessions(ctx, retired.ThreadKey)
	if err != nil || len(got) != 1 {
		t.Fatalf("RetiredSessions = %+v, %v", got, err)
	}
	if got[0].ObservedTS != retired.ObservedTS || got[0].LastSeq != retired.LastSeq || got[0].LeafID != retired.LeafID {
		t.Errorf("retired cursors = observed %q, seq %d, leaf %q; want %q, %d, %q",
			got[0].ObservedTS, got[0].LastSeq, got[0].LeafID, retired.ObservedTS, retired.LastSeq, retired.LeafID)
	}
	// Another thread's count is its own: the count is what names the next
	// session of *this* thread.
	if n, err := s.RetiredCount(ctx, "T1:C2:9.9"); err != nil || n != 0 {
		t.Errorf("RetiredCount of another thread = %d (err %v), want 0", n, err)
	}
}

func TestRestoreRetiredSessionReturnsItToItsOriginalThread(t *testing.T) {
	ctx := context.Background()
	s, _ := newStore(t)
	original := sampleThread()
	original.SessionName, original.SessionPath, original.SessionID = "", "", ""
	original.Cwd, original.ProjectDir = "", ""
	original.State = StateCold
	original.ObservedTS = ""
	if err := s.PutThread(ctx, original); err != nil {
		t.Fatalf("PutThread: %v", err)
	}
	retired := RetiredSession{
		SessionPath: "/sessions/old.jsonl", ThreadKey: original.ThreadKey,
		SessionName: "slack-old", ProjectDir: "/work/old", ObservedTS: "1700000000.000500",
		LastSeq: 42, LeafID: "leaf-42",
	}
	if err := s.RetireSession(ctx, retired); err != nil {
		t.Fatalf("RetireSession: %v", err)
	}

	got, restored, err := s.RestoreRetiredSession(ctx, original.ThreadKey, retired.SessionPath, "sid-old", "/work/old/wt")
	if err != nil || !restored {
		t.Fatalf("RestoreRetiredSession = %+v, %v, %v", got, restored, err)
	}
	if got.SessionName != retired.SessionName || got.SessionPath != retired.SessionPath || got.SessionID != "sid-old" ||
		got.Cwd != "/work/old/wt" || got.ProjectDir != retired.ProjectDir || got.State != StateCold ||
		got.ObservedTS != retired.ObservedTS || got.LastSeq != retired.LastSeq || got.LeafID != retired.LeafID {
		t.Errorf("restored row = %+v", got)
	}
	if rows, err := s.RetiredSessions(ctx, original.ThreadKey); err != nil || len(rows) != 0 {
		t.Errorf("retired sessions after restore = %+v, %v", rows, err)
	}
	if _, restored, err := s.RestoreRetiredSession(ctx, original.ThreadKey, retired.SessionPath, "sid-old", "/work/old/wt"); err != nil || restored {
		t.Errorf("the same retired session restored twice: restored=%v err=%v", restored, err)
	}
}

func TestRestoreRetiredSessionCannotMoveItOrReplaceCurrentSession(t *testing.T) {
	ctx := context.Background()
	s, _ := newStore(t)
	row := sampleThread()
	if err := s.PutThread(ctx, row); err != nil {
		t.Fatal(err)
	}
	if err := s.RetireSession(ctx, RetiredSession{SessionPath: "/sessions/old.jsonl", ThreadKey: row.ThreadKey}); err != nil {
		t.Fatal(err)
	}
	if _, restored, err := s.RestoreRetiredSession(ctx, "T1:C2:9.9", "/sessions/old.jsonl", "sid", "/work"); err != nil || restored {
		t.Errorf("restored a session into another thread: restored=%v err=%v", restored, err)
	}
	if got, restored, err := s.RestoreRetiredSession(ctx, row.ThreadKey, "/sessions/old.jsonl", "sid", "/work"); err != nil || restored {
		t.Errorf("overwrote an active thread session: restored=%v err=%v row=%+v", restored, err, got)
	}
}

func TestRetiringTheSameSessionTwiceIsIdempotent(t *testing.T) {
	ctx := context.Background()
	s, _ := newStore(t)
	r := RetiredSession{SessionPath: "/sessions/a.jsonl", ThreadKey: "T1:C1:1.1", ProjectDir: "/work/a"}

	for range 2 {
		if err := s.RetireSession(ctx, r); err != nil {
			t.Fatalf("RetireSession: %v", err)
		}
	}
	if n, err := s.RetiredCount(ctx, r.ThreadKey); err != nil || n != 1 {
		t.Errorf("RetiredCount = %d (err %v), want 1 after two writes of one session", n, err)
	}
}

func TestRetireNeedsAPathAndAThread(t *testing.T) {
	ctx := context.Background()
	s, _ := newStore(t)

	if err := s.RetireSession(ctx, RetiredSession{ThreadKey: "T1:C1:1.1"}); err == nil {
		t.Error("a retired session without a path was accepted")
	}
	if err := s.RetireSession(ctx, RetiredSession{SessionPath: "/sessions/a.jsonl"}); err == nil {
		t.Error("a retired session without a thread was accepted")
	}
}

func TestRetiredSessionsSurviveAReopen(t *testing.T) {
	ctx := context.Background()
	s, path := newStore(t)
	if err := s.RetireSession(ctx, RetiredSession{
		SessionPath: "/sessions/a.jsonl", ThreadKey: "T1:C1:1.1", ProjectDir: "/work/a",
	}); err != nil {
		t.Fatalf("RetireSession: %v", err)
	}
	_ = s.Close()

	reopened, err := Open(ctx, path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer reopened.Close()
	projects, err := reopened.RetiredProjects(ctx)
	if err != nil {
		t.Fatalf("RetiredProjects after a reopen: %v", err)
	}
	if len(projects) != 1 {
		t.Errorf("a reopened database forgot the retired session: %v", projects)
	}
}
