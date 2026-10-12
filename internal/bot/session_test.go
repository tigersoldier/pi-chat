package bot

import (
	"context"
	"errors"
	"testing"

	"github.com/tigersoldier/pi-gateway/gwclient"

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

// TestEventsBeforeThePromptAreDropped covers setup events that cannot be
// assigned to a reply: until agent_start or the prompt establishes a turn,
// text deltas alone must not be rendered as this answer.
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
	st.renderable.Store(true)
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

	// Text alone is not enough to classify a new turn; a stray frame must not
	// create an unsolicited Slack reply.
	th.onEvent(textDelta(t, "nobody is listening"))
	if th.busy() {
		t.Fatal("text outside a turn should not reserve one")
	}
}

func TestAgentStartDuringPromptPreparationIsNotDropped(t *testing.T) {
	b := &Bot{cfg: config.Defaults(), log: discardLogger()}
	th := &thread{key: "test", b: b, log: b.log}
	prompt := newPromptTurnState()
	th.cur.Store(prompt)
	th.turnMu.Lock() // keep the observed renderer queued while inspecting routing

	th.onEvent(gwclient.Event{Type: "agent_start"})
	observed := th.cur.Load()
	th.turnMu.Unlock()
	if observed == nil || observed == prompt {
		t.Fatal("a pre-prompt agent_start should get a separate observed-turn reservation")
	}
	if !observed.renderable.Load() || !observed.sawWork() {
		t.Fatal("the asynchronous agent_start was not captured by its reservation")
	}
	waitFor(t, "the observed turn to release its reservation", func() bool {
		select {
		case <-observed.finishedCh:
			return true
		default:
			return false
		}
	})
}

func TestObservedReconnectDoesNotCreateAReplacementSession(t *testing.T) {
	th := &thread{}
	_, err := th.reconnectObservedTurn(context.Background(), newTurnState(), [32]byte{}, false)
	if !errors.Is(err, errSessionGone) {
		t.Fatalf("reconnect with a cleared session path = %v, want errSessionGone", err)
	}
}

func TestAgentStartAfterSettlementGetsSeparateReservation(t *testing.T) {
	b := &Bot{cfg: config.Defaults(), log: discardLogger()}
	th := &thread{key: "test", b: b, log: b.log}
	old := newTurnState()
	old.renderable.Store(true)
	old.markDone()
	th.cur.Store(old)
	th.turnMu.Lock()

	th.onEvent(gwclient.Event{Type: "agent_start"})
	following := th.cur.Load()
	if following == nil || following == old {
		th.turnMu.Unlock()
		t.Fatal("a later agent_start was merged into the settled turn")
	}
	th.finishTurn(old)
	waitFor(t, "the following observed turn to finish", func() bool {
		select {
		case <-following.finishedCh:
			return true
		default:
			return false
		}
	})
}

func TestSessionChangeRejectsStaleObservedEvents(t *testing.T) {
	b := &Bot{cfg: config.Defaults(), log: discardLogger()}
	th := &thread{key: "test", b: b, log: b.log}
	oldEpoch := th.sessionEpoch.Load()

	th.sessionChanging.Store(true)
	th.onEvent(gwclient.Event{Type: "agent_start"})
	th.sessionChanging.Store(false)
	if th.busy() {
		t.Fatal("an event during session replacement reserved a turn")
	}

	if nextEpoch := th.sessionEpoch.Add(1); nextEpoch == oldEpoch {
		t.Fatal("session epoch did not advance")
	}
	th.onEventWithClient(gwclient.Event{Type: "agent_start"}, nil, oldEpoch)
	if th.busy() {
		t.Fatal("an event from a retired session reserved a turn")
	}
}

func TestObservedTurnRendersOnlyAssistantText(t *testing.T) {
	b, platform, _ := newTestBot(t)
	t.Cleanup(b.Close)
	thread := Thread{Workspace: "T1", Channel: "C1", ThreadTS: "1700000000.000100"}
	th, err := b.threadFor(thread)
	if err != nil {
		t.Fatalf("track the thread: %v", err)
	}

	th.onEvent(gwclient.Event{Type: "agent_start"})
	st := th.cur.Load()
	if st == nil {
		t.Fatal("agent_start should reserve an observed turn")
	}
	th.onEvent(textDelta(t, "the completed subagent findings"))
	th.onEvent(gwclient.Event{Type: "agent_settled"})

	waitFor(t, "the observed turn to finish", func() bool {
		select {
		case <-st.finishedCh:
			return true
		default:
			return false
		}
	})
	turns := platform.postedTurns()
	if len(turns) != 1 {
		t.Fatalf("started %d Slack replies, want 1", len(turns))
	}
	if turns[0].Thread != thread || turns[0].Workspace != thread.Workspace {
		t.Errorf("reply target = %+v (workspace %q), want the originating thread %+v", turns[0].Thread, turns[0].Workspace, thread)
	}
	if turns[0].Text != "" {
		t.Errorf("the synthetic wake prompt was used as Slack message text: %q", turns[0].Text)
	}
	platform.mu.Lock()
	renderer := platform.renderer
	platform.mu.Unlock()
	if len(renderer.finals) != 1 || renderer.finals[0] != "the completed subagent findings" {
		t.Errorf("rendered finals = %q, want only the assistant's subagent findings", renderer.finals)
	}
	if th.busy() {
		t.Error("the observed turn reservation was not released")
	}
	statuses := platform.reportedStatuses()
	if len(statuses) != 2 || statuses[0] != StatusBusy || statuses[1] != StatusIdle {
		t.Errorf("status lifecycle = %v, want busy then idle", statuses)
	}
}

func TestObservedTurnWithoutAssistantTextDoesNotPostWake(t *testing.T) {
	b, platform, _ := newTestBot(t)
	t.Cleanup(b.Close)
	thread := Thread{Workspace: "T1", Channel: "C1", ThreadTS: "1700000000.000100"}
	th, err := b.threadFor(thread)
	if err != nil {
		t.Fatalf("track the thread: %v", err)
	}

	th.onEvent(gwclient.Event{Type: "agent_start"})
	st := th.cur.Load()
	if st == nil {
		t.Fatal("agent_start should reserve an observed turn")
	}
	th.onEvent(gwclient.Event{Type: "agent_settled"})
	waitFor(t, "the empty observed turn to finish", func() bool {
		select {
		case <-st.finishedCh:
			return true
		default:
			return false
		}
	})
	if got := platform.startedTurns(); got != 0 {
		t.Fatalf("posted %d Slack replies without assistant output", got)
	}
}

func TestObservedTurnForgetsDeletedSession(t *testing.T) {
	b, platform, st := newTestBot(t)
	t.Cleanup(b.Close)
	thread := Thread{Workspace: "T1", Channel: "C1", ThreadTS: "1700000000.000100"}
	th, err := b.threadFor(thread)
	if err != nil {
		t.Fatalf("track the thread: %v", err)
	}
	th.update(func(row *store.ThreadRow) {
		row.SessionName, row.SessionPath, row.SessionID = "session", "/sessions/deleted.jsonl", "sid"
		row.LastSeq, row.LeafID = 9, "leaf-9"
		row.State = store.StateWarm
	})
	th.rememberAssistant("previous answer")

	th.onEvent(gwclient.Event{Type: "agent_start"})
	turn := th.cur.Load()
	th.onEvent(gwclient.Event{
		Type: "gw_session_state",
		Raw:  []byte(`{"type":"gw_session_state","state":"deleted"}`),
	})
	waitFor(t, "the deleted observed turn to finish", func() bool {
		select {
		case <-turn.finishedCh:
			return true
		default:
			return false
		}
	})

	row, found, err := st.Thread(context.Background(), thread.Key())
	if err != nil || !found {
		t.Fatalf("read the thread row: found=%v err=%v", found, err)
	}
	if row.State != store.StateDeleted || row.SessionPath != "" || row.LastSeq != 0 {
		t.Errorf("deleted session survived observed-turn cleanup: %+v", row)
	}
	if got := platform.startedTurns(); got != 0 {
		t.Errorf("posted %d Slack replies for a deleted session with no text", got)
	}
	if _, known := th.assistantBaseline(); known {
		t.Error("deleted session's assistant baseline survived cleanup")
	}
}
