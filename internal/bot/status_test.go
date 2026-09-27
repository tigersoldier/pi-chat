package bot

import (
	"context"
	"testing"

	"github.com/tigersoldier/pi-chat/internal/store"
)

// seedStopThread seeds a thread and loads it, which is what a stop and a turn
// both need before they can do anything.
func seedStopThread(t *testing.T, b *Bot, st *store.Store) *thread {
	t.Helper()
	thread := Thread{Workspace: "T1", Channel: "C1", ThreadTS: "1700000000.000100"}
	seedThread(t, st, store.ThreadRow{
		ThreadKey: thread.Key(), WorkspaceID: "T1", ChannelID: "C1", ThreadTS: thread.ThreadTS,
	})
	th, ok := b.tracked(thread)
	if !ok {
		t.Fatal("the seeded thread did not load")
	}
	return th
}

// TestStatusFollowsTheTurn covers the loading indicator: it goes up before the
// turn produces anything and comes down however the turn ends. The turn here
// fails — there is no gateway to reach — which is the point: a failure that left
// the indicator spinning would be the visible bug.
func TestStatusFollowsTheTurn(t *testing.T) {
	b, platform, st := newTestBot(t)
	th := seedStopThread(t, b, st)

	b.startTurn(context.Background(), th, newTestMessage("Ev1", "hello"))
	waitFor(t, "the turn to end", func() bool { return len(platform.reportedStatuses()) >= 2 })

	got := platform.reportedStatuses()
	if got[0] != StatusBusy {
		t.Errorf("the first status was %q, want %q so the indicator appears while the agent works",
			got[0], StatusBusy)
	}
	if last := got[len(got)-1]; last != StatusIdle {
		t.Errorf("the last status was %q, want %q so the indicator does not spin forever", last, StatusIdle)
	}
}

// TestStopWithoutARunningTurnClearsTheStatus covers Slack's stop button pressed
// on a thread whose local turn has already ended: there is nothing to abort, and
// the stale processing status is the only thing left to repair.
func TestStopWithoutARunningTurnClearsTheStatus(t *testing.T) {
	b, platform, st := newTestBot(t)
	th := seedStopThread(t, b, st)
	thread := th.t

	b.HandleAction(context.Background(), Action{
		EventID: "Ev2", Channel: thread.Channel, UserID: "U1",
		Thread: &thread, ActionID: ActionStop,
	})

	waitFor(t, "the status to be cleared", func() bool { return len(platform.reportedStatuses()) > 0 })
	got := platform.reportedStatuses()
	if len(got) != 1 || got[0] != StatusIdle {
		t.Errorf("statuses = %v, want a single %q", got, StatusIdle)
	}
}

// TestStopForAnUnknownThreadIsIgnored: a stop for a thread pi-chat has no row
// for is not an error, and must not be answered with a status for a session that
// does not exist.
func TestStopForAnUnknownThreadIsIgnored(t *testing.T) {
	b, platform, _ := newTestBot(t)
	thread := Thread{Workspace: "T1", Channel: "C-NOBODY", ThreadTS: "1700000009.000100"}

	b.HandleAction(context.Background(), Action{
		EventID: "Ev3", Channel: thread.Channel, UserID: "U1",
		Thread: &thread, ActionID: ActionStop,
	})

	if got := platform.reportedStatuses(); len(got) != 0 {
		t.Errorf("statuses = %v, want none for a thread with no session", got)
	}
}

// TestStopFromAStrangerIsRefused: the stop button is a way to interrupt
// someone's work, so it goes through the same allowlist as everything else.
func TestStopFromAStrangerIsRefused(t *testing.T) {
	b, platform, st := newTestBot(t)
	th := seedStopThread(t, b, st)
	thread := th.t

	b.HandleAction(context.Background(), Action{
		EventID: "Ev4", Channel: thread.Channel, UserID: "U2",
		Thread: &thread, ActionID: ActionStop,
	})

	waitFor(t, "the refusal", func() bool { return len(platform.postedNotices()) == 1 })
	if !platform.postedNotices()[0].Ephemeral {
		t.Error("a refusal should be visible only to the sender")
	}
	if got := platform.reportedStatuses(); len(got) != 0 {
		t.Errorf("statuses = %v: a refused stop still touched the session", got)
	}
}
