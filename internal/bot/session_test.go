package bot

import (
	"context"
	"testing"

	"github.com/tigersoldier/pi-chat/internal/config"
	"github.com/tigersoldier/pi-chat/internal/store"
)

// TestForgetSessionReservesTheKeyButDropsTheSession covers DESIGN.md §4
// invariant 3: when a session is deleted outside pi-chat, the thread keeps its
// identity and whatever the agent wrote stays on disk, but the thread must stop
// pointing at the tombstoned session so the next message starts a new one
// deliberately.
func TestForgetSessionReservesTheKeyButDropsTheSession(t *testing.T) {
	b, _, st := newTestBot(t)
	key := "T1:C1:1700000000.000100"
	thread := Thread{Workspace: "T1", Channel: "C1", ThreadTS: "1700000000.000100"}
	seedThread(t, st, store.ThreadRow{
		ThreadKey: key, WorkspaceID: "T1", ChannelID: "C1", ThreadTS: thread.ThreadTS,
		SessionName: "slack-t1-c1-1700000000-000100", SessionPath: "/sessions/x.jsonl", SessionID: "sid",
		Cwd: "/work/2026-09-27-fix", ProjectDir: "/work/2026-09-27-fix",
		State: store.StateWarm, LastSeq: 12, LeafID: "leaf-12", ProgressTS: "1700000000.000200",
	})
	th, ok := b.tracked(thread)
	if !ok {
		t.Fatal("the seeded thread did not load")
	}

	th.forgetSession()

	row, found, err := st.Thread(context.Background(), key)
	if err != nil || !found {
		t.Fatalf("the row is gone: found=%v err=%v", found, err)
	}
	if row.State != store.StateDeleted {
		t.Errorf("state = %q, want %q", row.State, store.StateDeleted)
	}
	if row.SessionName != "" || row.SessionPath != "" || row.SessionID != "" {
		t.Errorf("the session identity survived a delete: %+v", row)
	}
	if row.LastSeq != 0 || row.LeafID != "" || row.ProgressTS != "" {
		t.Errorf("the cursor of a deleted session survived: %+v", row)
	}
	// The key stays reserved, and the working directory is left alone: only the
	// session file was deleted, and unreferenced work is not pi-chat's to
	// remove.
	if row.ThreadKey != key || row.WorkspaceID != "T1" {
		t.Errorf("the thread identity was lost: %+v", row)
	}
	if row.ProjectDir != "/work/2026-09-27-fix" {
		t.Errorf("the working directory was forgotten: %+v", row)
	}
}

// TestEventsBeforeThePromptAreDropped covers the replay window: a resumed
// connection replays from its saved cursor, and a long replay can still be
// arriving while a turn is being set up. Those frames belong to a previous
// turn, so they must not be rendered as this turn's answer.
func TestEventsBeforeThePromptAreDropped(t *testing.T) {
	b := &Bot{cfg: config.Defaults(), log: discardLogger()}
	th := &thread{key: "test", b: b, log: b.log}
	st := newTurnState()
	th.cur.Store(st)

	th.onEvent(textDelta(t, "an answer from before the restart"))

	if got := st.text(); got != "" {
		t.Fatalf("a replayed frame leaked into the turn: %q", got)
	}
	if st.sawWork() {
		t.Error("a replayed frame counted as this turn's work")
	}

	// Once the prompt is out, the stream is ours.
	st.prompted.Store(true)
	th.onEvent(textDelta(t, "the real answer"))
	if got := st.takePending(); got != "the real answer" {
		t.Fatalf("pending text = %q, want the answer produced after the prompt", got)
	}
}

// TestEventsOutsideATurnAreDropped is the other half: between turns there is
// nothing to render.
func TestEventsOutsideATurnAreDropped(t *testing.T) {
	b := &Bot{cfg: config.Defaults(), log: discardLogger()}
	th := &thread{key: "test", b: b, log: b.log}

	// No turn, so no state to pollute; this must simply not panic.
	th.onEvent(textDelta(t, "nobody is listening"))
}
