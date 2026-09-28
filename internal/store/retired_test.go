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
	// Another thread's count is its own: the count is what names the next
	// session of *this* thread.
	if n, err := s.RetiredCount(ctx, "T1:C2:9.9"); err != nil || n != 0 {
		t.Errorf("RetiredCount of another thread = %d (err %v), want 0", n, err)
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

func TestChannelHasSessionLooksPastTheAskingThread(t *testing.T) {
	ctx := context.Background()
	s, _ := newStore(t)

	withSession := sampleThread()
	withSession.ThreadKey = "T1:D1:1.1"
	withSession.ChannelID = "D1"
	withSession.ThreadTS = "1.1"
	if err := s.PutThread(ctx, withSession); err != nil {
		t.Fatalf("PutThread: %v", err)
	}

	// The question is "did this DM have a session *before* this message", so the
	// asking thread must not answer it for itself.
	if has, err := s.ChannelHasSession(ctx, "T1", "D1", "T1:D1:1.1"); err != nil || has {
		t.Errorf("ChannelHasSession(asking thread) = %v (err %v), want false", has, err)
	}
	if has, err := s.ChannelHasSession(ctx, "T1", "D1", "T1:D1:2.2"); err != nil || !has {
		t.Errorf("ChannelHasSession(other thread) = %v (err %v), want true", has, err)
	}
	// A different channel is a different conversation.
	if has, err := s.ChannelHasSession(ctx, "T1", "D2", "T1:D2:2.2"); err != nil || has {
		t.Errorf("ChannelHasSession(other channel) = %v (err %v), want false", has, err)
	}
}

func TestChannelHasSessionIgnoresDeletedAndEmptyThreads(t *testing.T) {
	ctx := context.Background()
	s, _ := newStore(t)

	empty := sampleThread()
	empty.ThreadKey, empty.ChannelID, empty.ThreadTS = "T1:D1:1.1", "D1", "1.1"
	empty.SessionPath, empty.SessionName, empty.SessionID = "", "", ""
	if err := s.PutThread(ctx, empty); err != nil {
		t.Fatalf("PutThread: %v", err)
	}
	// A row without a session is a thread the bot was mentioned in but never
	// prompted in: there is nothing for a new session to replace.
	if has, err := s.ChannelHasSession(ctx, "T1", "D1", "T1:D1:9.9"); err != nil || has {
		t.Errorf("ChannelHasSession = %v (err %v), want false for a row with no session", has, err)
	}

	deleted := sampleThread()
	deleted.ThreadKey, deleted.ChannelID, deleted.ThreadTS = "T1:D1:2.2", "D1", "2.2"
	deleted.State = StateDeleted
	if err := s.PutThread(ctx, deleted); err != nil {
		t.Fatalf("PutThread: %v", err)
	}
	if has, err := s.ChannelHasSession(ctx, "T1", "D1", "T1:D1:9.9"); err != nil || has {
		t.Errorf("ChannelHasSession = %v (err %v), want false for a deleted session", has, err)
	}
}
