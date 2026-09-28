package bot

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tigersoldier/pi-chat/internal/store"
)

// threadWithSession seeds a thread that owns a session, as a previous run would
// have left it, and returns the core's thread for it. The project directory is
// really created under the projects root, because the sweep has to be able to see
// it.
func threadWithSession(t *testing.T, b *Bot, st *store.Store, channel, ts string) *thread {
	t.Helper()
	key := "T1:" + channel + ":" + ts
	projectDir := filepath.Join(b.cfg.Paths.ProjectsRoot, "2026-09-27-fix-tests")
	if err := os.MkdirAll(projectDir, 0o700); err != nil {
		t.Fatalf("create the project directory: %v", err)
	}
	seedThread(t, st, store.ThreadRow{
		ThreadKey: key, WorkspaceID: "T1", ChannelID: channel, ThreadTS: ts,
		SessionName: "slack-t1-" + channel + "-" + ts,
		SessionPath: "/sessions/" + channel + ".jsonl",
		Cwd:         projectDir, ProjectDir: projectDir,
		State: store.StateCold, ObservedTS: "1700000000.000500",
	})
	th, err := b.threadFor(Thread{Workspace: "T1", Channel: channel, ThreadTS: ts})
	if err != nil {
		t.Fatalf("threadFor: %v", err)
	}
	return th
}

// noticeWith returns the first notice whose text contains all of the given
// fragments, so a test does not depend on how many notices a path posts.
func noticeWith(notices []Notice, fragments ...string) (Notice, bool) {
	for _, n := range notices {
		matches := true
		for _, fragment := range fragments {
			if !strings.Contains(n.Text, fragment) {
				matches = false
				break
			}
		}
		if matches {
			return n, true
		}
	}
	return Notice{}, false
}

func TestNewRetiresTheSessionAndKeepsItResumable(t *testing.T) {
	b, plat, st := newTestBot(t)
	const ts = "1700000000.000100"
	th := threadWithSession(t, b, st, "C1", ts)
	dir := th.snapshot().ProjectDir

	b.HandleCommand(context.Background(), Command{
		EventID: "Ev1", Channel: "C1", UserID: "U1", Workspace: "T1",
		Thread: &Thread{Workspace: "T1", Channel: "C1", ThreadTS: ts},
		Text:   "/new",
	})

	waitFor(t, "the answer", func() bool { return len(plat.postedNotices()) >= 1 })
	notice, ok := noticeWith(plat.postedNotices(), "/pi resume")
	if !ok {
		t.Fatalf("no notice mentioned where the old session went: %+v", plat.postedNotices())
	}
	if !notice.Ephemeral {
		t.Error("a command answer belongs to whoever typed it, so it is ephemeral")
	}

	// The thread let go of the session, including the observation watermark: the
	// next session starts with its own conversation.
	row := th.snapshot()
	if row.SessionPath != "" || row.SessionName != "" || row.SessionID != "" {
		t.Errorf("the thread still points at the retired session: %+v", row)
	}
	if row.ProjectDir != "" || row.Cwd != "" {
		t.Errorf("the thread still owns the retired directory: %+v", row)
	}
	if row.ObservedTS != "" {
		t.Errorf("the new session inherits the old watermark: %q", row.ObservedTS)
	}

	// The retired session is on record — that is what keeps its directory and
	// the session file resumable.
	projects, err := st.RetiredProjects(context.Background())
	if err != nil {
		t.Fatalf("RetiredProjects: %v", err)
	}
	if len(projects) != 1 || projects[0] != dir {
		t.Errorf("retired projects = %v, want %q", projects, dir)
	}
	removed, err := b.SweepWorkspaces(context.Background())
	if err != nil {
		t.Fatalf("SweepWorkspaces: %v", err)
	}
	if len(removed) != 0 {
		t.Errorf("the sweep removed %v: a retired session's directory is somebody's work", removed)
	}
	if _, err := os.Stat(dir); err != nil {
		t.Errorf("the retired directory is gone: %v", err)
	}
}

func TestNewWithTextStartsTheFreshSessionOnIt(t *testing.T) {
	b, plat, st := newTestBot(t)
	const ts = "1700000000.000100"
	threadWithSession(t, b, st, "C1", ts)

	b.HandleCommand(context.Background(), Command{
		EventID: "Ev1", Channel: "C1", UserID: "U1", Workspace: "T1",
		Thread: &Thread{Workspace: "T1", Channel: "C1", ThreadTS: ts},
		TS:     "1700000000.000700",
		Text:   "/new summarize the failing test",
	})

	waitFor(t, "the turn", func() bool { return plat.startedTurns() == 1 })
	turns := plat.postedTurns()
	if want := "summarize the failing test"; turns[0].Text != want {
		t.Errorf("the turn's prompt = %q, want %q", turns[0].Text, want)
	}
	if want := "1700000000.000700"; turns[0].TS != want {
		t.Errorf("the turn's trigger timestamp = %q, want the command's message %q", turns[0].TS, want)
	}
}

func TestNewWithoutASessionOnlySaysSo(t *testing.T) {
	b, plat, st := newTestBot(t)
	const ts = "1700000000.000100"
	if _, err := b.threadFor(Thread{Workspace: "T1", Channel: "C1", ThreadTS: ts}); err != nil {
		t.Fatalf("threadFor: %v", err)
	}

	b.HandleCommand(context.Background(), Command{
		EventID: "Ev1", Channel: "C1", UserID: "U1", Workspace: "T1",
		Thread: &Thread{Workspace: "T1", Channel: "C1", ThreadTS: ts},
		Text:   "/new",
	})

	waitFor(t, "the answer", func() bool { return len(plat.postedNotices()) >= 1 })
	if _, ok := noticeWith(plat.postedNotices(), "no session here yet"); !ok {
		t.Errorf("notices = %+v, want one saying there is nothing to replace", plat.postedNotices())
	}
	if projects, err := st.RetiredProjects(context.Background()); err != nil || len(projects) != 0 {
		t.Errorf("retired projects = %v (err %v), want none", projects, err)
	}
	if plat.startedTurns() != 0 {
		t.Error("/new without a text started a turn")
	}
}

func TestNewRefusesWhileATurnIsRunning(t *testing.T) {
	b, plat, st := newTestBot(t)
	const ts = "1700000000.000100"
	th := threadWithSession(t, b, st, "C1", ts)

	// Hold the turn lock the way a running turn does: a session must not be
	// retired under the turn that is using it.
	th.turnMu.Lock()
	defer th.turnMu.Unlock()

	b.HandleCommand(context.Background(), Command{
		EventID: "Ev1", Channel: "C1", UserID: "U1", Workspace: "T1",
		Thread: &Thread{Workspace: "T1", Channel: "C1", ThreadTS: ts},
		Text:   "/new",
	})

	waitFor(t, "the refusal", func() bool { return len(plat.postedNotices()) >= 1 })
	if _, ok := noticeWith(plat.postedNotices(), "turn is running"); !ok {
		t.Errorf("notices = %+v, want one saying a turn is running", plat.postedNotices())
	}
	if row := th.snapshot(); row.SessionPath == "" {
		t.Error("the session was retired while a turn was running")
	}
	if projects, err := st.RetiredProjects(context.Background()); err != nil || len(projects) != 0 {
		t.Errorf("retired projects = %v (err %v), want none", projects, err)
	}
}

func TestATopLevelDMMessageAnnouncesANewSession(t *testing.T) {
	b, plat, st := newTestBot(t)
	threadWithSession(t, b, st, "D1", "1700000000.000100")

	b.HandleMessage(context.Background(), Message{
		EventID:   "Ev1",
		Thread:    Thread{Workspace: "T1", Channel: "D1", ThreadTS: "1700000000.000200"},
		UserID:    "U1",
		Workspace: "T1",
		TS:        "1700000000.000200",
		Text:      "start over",
		Direct:    true,
	})

	waitFor(t, "the notice", func() bool { return len(plat.postedNotices()) >= 1 })
	notice, ok := noticeWith(plat.postedNotices(), "New session", "/pi resume")
	if !ok {
		t.Fatalf("notices = %+v, want one announcing the new session", plat.postedNotices())
	}
	// It is about the conversation rather than about one interaction, so it is
	// visible rather than ephemeral.
	if notice.Ephemeral {
		t.Error("the new-session notice should be visible")
	}
}

func TestATopLevelDMMessageIsSilentWhenTheDMWasEmpty(t *testing.T) {
	b, plat, _ := newTestBot(t)

	b.HandleMessage(context.Background(), Message{
		EventID:   "Ev1",
		Thread:    Thread{Workspace: "T1", Channel: "D1", ThreadTS: "1700000000.000200"},
		UserID:    "U1",
		Workspace: "T1",
		TS:        "1700000000.000200",
		Text:      "hello",
		Direct:    true,
	})

	waitFor(t, "the turn", func() bool { return plat.startedTurns() == 1 })
	if _, ok := noticeWith(plat.postedNotices(), "New session"); ok {
		t.Error("a first message in a DM is not a reset, so there is nothing to announce")
	}
}
