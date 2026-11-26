package bot

import (
	"context"
	"errors"
	"testing"
	"time"

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
func TestStatusKeepaliveRenewsTheCurrentState(t *testing.T) {
	b, platform, st := newTestBot(t)
	th := seedStopThread(t, b, st)
	platform.mu.Lock()
	platform.statusRefreshInterval = 5 * time.Millisecond
	platform.mu.Unlock()

	b.setStatus(context.Background(), th, StatusBusy)
	stopRefresh := b.startStatusKeepalive(context.Background(), th)
	waitFor(t, "a busy status refresh", func() bool { return len(platform.reportedStatuses()) >= 2 })

	beforeWaiting := len(platform.reportedStatuses())
	b.setStatus(context.Background(), th, StatusWaiting)
	waitFor(t, "a waiting status refresh", func() bool { return len(platform.reportedStatuses()) >= beforeWaiting+2 })
	stopRefresh()
	b.setStatus(context.Background(), th, StatusIdle)

	got := platform.reportedStatuses()
	seenWaiting := false
	for _, status := range got {
		if status == StatusWaiting {
			seenWaiting = true
		}
		if seenWaiting && status == StatusBusy {
			t.Fatalf("a stale busy heartbeat followed the waiting transition: %v", got)
		}
	}
	if len(got) == 0 || got[len(got)-1] != StatusIdle {
		t.Fatalf("last status = %v, want idle after stopping the heartbeat", got)
	}
}

func TestStatusKeepaliveRetriesShortlyAfterAFailure(t *testing.T) {
	// A failed renewal must not wait a whole interval: Slack drops a processing
	// session to active an hour after the last successful write.
	b, platform, st := newTestBot(t)
	th := seedStopThread(t, b, st)
	platform.mu.Lock()
	platform.statusRefreshInterval = time.Second
	platform.statusFailures = 1
	platform.mu.Unlock()
	previous := statusRefreshRetry
	statusRefreshRetry = 5 * time.Millisecond
	defer func() { statusRefreshRetry = previous }()

	b.setStatus(context.Background(), th, StatusBusy)
	start := time.Now()
	stopRefresh := b.startStatusKeepalive(context.Background(), th)
	waitFor(t, "a retry after the failed renewal", func() bool {
		return len(platform.reportedStatuses()) >= 2
	})
	elapsed := time.Since(start)
	stopRefresh()
	b.setStatus(context.Background(), th, StatusIdle)

	if elapsed >= 1500*time.Millisecond {
		t.Fatalf("the retry took %v, want it before the next full interval", elapsed)
	}
}

func TestRecoverProgressClearsOnlyAfterReporterSucceeds(t *testing.T) {
	b, platform, st := newTestBot(t)
	thread := Thread{Workspace: "T1", Channel: "C1", ThreadTS: "1700000000.000100"}
	row := store.ThreadRow{
		ThreadKey: thread.Key(), WorkspaceID: thread.Workspace, ChannelID: thread.Channel,
		ThreadTS: thread.ThreadTS, ProgressTS: "1700000000.000200",
	}
	seedThread(t, st, row)

	platform.mu.Lock()
	platform.recoveryError = errors.New("Slack unavailable")
	platform.mu.Unlock()
	b.recoverProgress(context.Background())
	stored, ok, err := st.Thread(context.Background(), row.ThreadKey)
	if err != nil || !ok {
		t.Fatalf("read thread after failed recovery: ok=%v err=%v", ok, err)
	}
	if stored.ProgressTS != row.ProgressTS {
		t.Fatalf("failed recovery cleared handle %q", row.ProgressTS)
	}

	platform.mu.Lock()
	platform.recoveryError = nil
	platform.mu.Unlock()
	b.recoverProgress(context.Background())
	stored, ok, err = st.Thread(context.Background(), row.ThreadKey)
	if err != nil || !ok || stored.ProgressTS != "" {
		t.Fatalf("successful recovery state = %+v ok=%v err=%v", stored, ok, err)
	}
	platform.mu.Lock()
	recoveries := append([]progressRecoveryRecord(nil), platform.recoveries...)
	platform.mu.Unlock()
	if len(recoveries) != 2 || recoveries[0].ts != row.ProgressTS || recoveries[0].thread.Key() != thread.Key() {
		t.Fatalf("recovery calls = %+v, want the persisted handle retried", recoveries)
	}
}

func TestRecoverProgressIncludesTheSelfDMSurface(t *testing.T) {
	// The self-DM has no stream, but it patches one placeholder message and
	// persists its handle; a restart must be able to replace that placeholder.
	b, platform, st := newTestBot(t)
	thread := Thread{Workspace: "T1", Channel: "D1", ThreadTS: "1700000000.000100", Surface: SurfaceSelfDM}
	row := store.ThreadRow{
		ThreadKey: thread.Key(), WorkspaceID: thread.Workspace, ChannelID: thread.Channel,
		ThreadTS: thread.ThreadTS, Surface: SurfaceSelfDM, ProgressTS: "1700000000.000200",
	}
	seedThread(t, st, row)

	b.recoverProgress(context.Background())
	platform.mu.Lock()
	recoveries := append([]progressRecoveryRecord(nil), platform.recoveries...)
	platform.mu.Unlock()
	if len(recoveries) != 1 || recoveries[0].thread.Surface != SurfaceSelfDM || recoveries[0].ts != row.ProgressTS {
		t.Fatalf("self-DM recovery calls = %+v, want the persisted placeholder", recoveries)
	}
}

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
