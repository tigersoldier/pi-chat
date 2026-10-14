package bot

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tigersoldier/pi-chat/internal/config"
	"github.com/tigersoldier/pi-gateway/gwclient"
)

// discardLogger keeps test output quiet.
func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// textDelta builds the message_update pi sends for streamed assistant text.
func textDelta(t *testing.T, s string) gwclient.Event {
	t.Helper()
	delta, err := json.Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	raw := `{"type":"message_update","assistantMessageEvent":{"type":"text_delta","contentIndex":0,"delta":` + string(delta) + `}}`
	return gwclient.Event{Type: "message_update", Raw: []byte(raw)}
}

func toolEvent(t *testing.T, typ, callID, name string, isError bool) gwclient.Event {
	t.Helper()
	raw, err := json.Marshal(struct {
		Type       string `json:"type"`
		ToolCallID string `json:"toolCallId"`
		ToolName   string `json:"toolName"`
		IsError    bool   `json:"isError,omitempty"`
	}{Type: typ, ToolCallID: callID, ToolName: name, IsError: isError})
	if err != nil {
		t.Fatal(err)
	}
	return gwclient.Event{Type: typ, Raw: raw}
}

func TestTurnStateAccumulatesTextDeltas(t *testing.T) {
	st := newTurnState()
	log := discardLogger()

	st.feed(textDelta(t, "Hel"), log)
	st.feed(textDelta(t, "lo"), log)

	if got := st.takePending(); got != "Hello" {
		t.Fatalf("pending text = %q, want %q", got, "Hello")
	}
	if got := st.takePending(); got != "" {
		t.Fatalf("takePending did not drain: %q", got)
	}
	if got := st.text(); got != "Hello" {
		t.Fatalf("full text = %q, want %q", got, "Hello")
	}
	if !st.sawWork() {
		t.Fatal("a text delta should count as the turn having started")
	}
}

func TestTurnStateTracksToolTaskProgressWithoutArgumentsOrOutput(t *testing.T) {
	st := newTurnState()
	log := discardLogger()
	startedAt := time.Unix(1_000, 0)
	st.progressStartedAt = startedAt

	start := toolEvent(t, "tool_execution_start", "call-1", "bash", false)
	start.Raw = []byte(`{"type":"tool_execution_start","toolCallId":"call-1","toolName":"bash","args":{"command":"do-not-share"}}`)
	st.feed(start, log)
	end := toolEvent(t, "tool_execution_end", "call-1", "bash", true)
	end.Raw = []byte(`{"type":"tool_execution_end","toolCallId":"call-1","toolName":"bash","isError":true,"result":{"secret":"do-not-share"}}`)
	st.feed(end, log)

	updates := st.takeTaskUpdates()
	if len(updates) != 1 || updates[0].ID != turnProgressTaskID || updates[0].Title != "Running a command" || updates[0].Status != TaskInProgress {
		t.Fatalf("in-progress task update = %+v", updates)
	}
	if strings.Contains(updates[0].Title, "do-not-share") {
		t.Fatal("tool arguments or output leaked into the progress title")
	}
	if updates := st.takeTaskUpdates(); len(updates) != 0 {
		t.Fatalf("tool completion should not create a second task: %+v", updates)
	}

	st.finishProgressTaskAt(TaskComplete, startedAt.Add(42*time.Second))
	updates = st.takeTaskUpdates()
	if len(updates) != 1 || updates[0].ID != turnProgressTaskID || updates[0].Title != "Completed · 1 action · 42s" || updates[0].Status != TaskComplete {
		t.Fatalf("turn completion update = %+v", updates)
	}
}

func TestPublicProgressMarkerBecomesTaskTitleAndIsRemovedFromReply(t *testing.T) {
	st := newTurnState()
	log := discardLogger()
	startedAt := time.Unix(2_000, 0)
	st.progressStartedAt = startedAt

	st.feed(textDelta(t, "Checking the service. [[slack-prog"), log)
	st.feed(textDelta(t, "ress: Checking current service\nlogs]]"), log)
	if got := st.takePending(); got != "Checking the service. " {
		t.Fatalf("reply text = %q, want marker removed", got)
	}

	st.feed(toolEvent(t, "tool_execution_start", "call-1", "bash", false), log)
	updates := st.takeTaskUpdates()
	if len(updates) != 1 || updates[0].Title != "Checking current service logs" || updates[0].Details != "Progress so far:\n• Checking current service logs" {
		t.Fatalf("start updates = %+v, want concise title and safe expanded details", updates)
	}
	if got := st.text(); strings.Contains(got, progressMarkerOpen) {
		t.Fatalf("progress marker leaked into reply text %q", got)
	}
	if got := st.authoritativeText("Finished. [[slack-progress: hidden status]]"); strings.Contains(got, "hidden status") {
		t.Fatalf("authoritative answer leaked a progress marker: %q", got)
	}

	st.feed(toolEvent(t, "tool_execution_end", "call-1", "bash", false), log)
	if updates := st.takeTaskUpdates(); len(updates) != 0 {
		t.Fatalf("tool completion should not close the turn progress task: %+v", updates)
	}
	st.finishProgressTaskAt(TaskComplete, startedAt.Add(75*time.Second))
	updates = st.takeTaskUpdates()
	if len(updates) != 1 || updates[0].ID != turnProgressTaskID || updates[0].Title != "Completed · 1 action · 1m15s" || updates[0].Details != "Completed actions:\n• Checking current service logs" || updates[0].Status != TaskComplete {
		t.Fatalf("turn completion update = %+v", updates)
	}
}

func TestProgressClosingDelimiterCanSplitAcrossDeltas(t *testing.T) {
	st := newTurnState()
	log := discardLogger()

	st.feed(textDelta(t, "[[slack-progress: Checking service logs]"), log)
	st.feed(textDelta(t, "]"), log)
	st.feed(toolEvent(t, "tool_execution_start", "call-1", "bash", false), log)

	updates := st.takeTaskUpdates()
	if len(updates) != 1 || updates[0].Title != "Checking service logs" {
		t.Fatalf("task updates = %+v, want parsed title across split delimiter", updates)
	}
	if got := st.text(); strings.Contains(got, progressMarkerOpen) {
		t.Fatalf("status marker leaked into reply: %q", got)
	}
}

func TestIncompleteProgressMarkerDoesNotSuppressFollowingReply(t *testing.T) {
	st := newTurnState()
	log := discardLogger()
	st.feed(textDelta(t, "Status [[slack-progress: incomplete"), log)

	// A new assistant message is a boundary: an unclosed marker must become
	// ordinary reply text rather than capturing the next message indefinitely.
	st.startMessage()
	st.feed(textDelta(t, "Final answer"), log)
	if got := st.text(); !strings.Contains(got, "incomplete") || !strings.Contains(got, "Final answer") {
		t.Fatalf("reply lost text around an incomplete marker: %q", got)
	}
	if got := st.authoritativeText("Answer [[slack-progress: dangling"); !strings.Contains(got, "dangling") {
		t.Fatalf("authoritative answer was truncated at incomplete marker: %q", got)
	}
}

func TestReplaceClearsPartialProgressMarkerState(t *testing.T) {
	st := newTurnState()
	st.feed(textDelta(t, "Partial [[slack-prog"), discardLogger())
	st.replace("Recovered answer")
	st.markDone()

	if got := st.text(); got != "Recovered answer" {
		t.Fatalf("recovered text = %q, want no stale marker suffix", got)
	}
	if got := st.takePending(); got != "Recovered answer" {
		t.Fatalf("recovered pending text = %q", got)
	}
}

func TestParallelProgressMarkersStayInToolStartOrder(t *testing.T) {
	st := newTurnState()
	log := discardLogger()
	startedAt := time.Unix(3_000, 0)
	st.progressStartedAt = startedAt
	titles := []string{"Check first task", "Check second task", "Check third task", "Check fourth task"}
	for _, title := range titles {
		st.feed(textDelta(t, "[[slack-progress: "+title+"]]"), log)
	}
	for i, title := range titles {
		id := "call-" + string(rune('1'+i))
		st.feed(toolEvent(t, "tool_execution_start", id, "bash", false), log)
		updates := st.takeTaskUpdates()
		if len(updates) != 1 || updates[0].ID != turnProgressTaskID || updates[0].Title != title || updates[0].Status != TaskInProgress {
			t.Fatalf("tool %d progress = %+v, want title %q on one stable task", i+1, updates, title)
		}
		st.feed(toolEvent(t, "tool_execution_end", id, "bash", false), log)
	}
	if len(st.activeTasks) != 1 {
		t.Fatalf("active tasks = %d, want one task for the entire turn", len(st.activeTasks))
	}

	st.finishProgressTaskAt(TaskComplete, startedAt.Add(2*time.Minute))
	updates := st.takeTaskUpdates()
	wantSummary := "Completed · 4 actions · 2m0s"
	wantDetails := "Completed actions:\n• Check first task\n• Check second task\n• Check third task\n• Check fourth task"
	if len(updates) != 1 || updates[0].ID != turnProgressTaskID || updates[0].Title != wantSummary || updates[0].Details != wantDetails || updates[0].Status != TaskComplete {
		t.Fatalf("turn completion = %+v", updates)
	}
}

func TestProgressTitlesAreTrimmedAndRejectCommandLikeText(t *testing.T) {
	recovered := newTurnState()
	recovered.replace("Answer [[slack-progress: hidden status]] done")
	if got := recovered.takePending(); strings.Contains(got, "hidden status") || strings.Contains(got, progressMarkerOpen) {
		t.Fatalf("recovered text leaked a status marker: %q", got)
	}

	long := strings.Repeat("Check ", maxProgressTitleRunes)
	if got := trimProgressTitle(long); len([]rune(got)) != maxProgressTitleRunes {
		t.Fatalf("trimmed title rune length = %d, want %d", len([]rune(got)), maxProgressTitleRunes)
	}
	if got := trimProgressTitle("Checking logs --unit private.service"); got != "" {
		t.Fatalf("command-like progress title = %q, want rejected", got)
	}
	for _, risky := range []string{
		"Checking AKIAIOSFODNN7EXAMPLE",
		"Using xoxb-123456789012-123456789012-123456789012",
		"Checking 4111111111111111",
		"Checking 123456789012",
		"Reading password=example",
		"Submit PIN 123456",
		"Enter OTP 654321",
		"Use passcode 1482",
		"Enter one-time code 172839",
		"Enter verification code 314159",
		"Enter code 123456",
		"Enter code\n123456",
	} {
		if got := trimProgressTitle(risky); got != "" {
			t.Errorf("credential-like progress title %q was accepted as %q", risky, got)
		}
	}
	for _, safe := range []string{"Spinning up a service", "Pinpointing the issue", "Reviewing the 2026 report", "Review the code changes"} {
		if got := trimProgressTitle(safe); got != safe {
			t.Errorf("ordinary title %q was rejected as %q", safe, got)
		}
	}
	if got := safeToolTitle("read token abc123456789"); got != "Using a tool" {
		t.Fatalf("unsafe tool-name fallback = %q, want generic title", got)
	}

	for _, marker := range []string{"Submit PIN 123456", "Enter code 123456"} {
		st := newTurnState()
		st.feed(textDelta(t, "[[slack-progress: "+marker+"]]"), discardLogger())
		st.feed(toolEvent(t, "tool_execution_start", "call-1", "bash", false), discardLogger())
		if got := st.takeTaskUpdates(); len(got) != 1 || got[0].Title != "Running a command" || got[0].Details != "Progress so far:\n• Running a command" {
			t.Fatalf("short-code marker %q was not safely replaced in title/details: %+v", marker, got)
		}
	}
}

func TestFallbackProgressTitleIsBounded(t *testing.T) {
	title := taskTitle(strings.Repeat("n", maxToolNameBytes))
	if got := len([]rune(title)); got > maxProgressTitleRunes {
		t.Fatalf("fallback title rune length = %d, want at most %d", got, maxProgressTitleRunes)
	}
}

func TestMarkDoneFinalizesActiveProgressTask(t *testing.T) {
	st := newTurnState()
	st.feed(toolEvent(t, "tool_execution_start", "call-1", "bash", false), discardLogger())
	st.markDone()
	updates := st.takeTaskUpdates()
	if len(updates) != 2 || updates[0].Status != TaskInProgress || updates[1].Status != TaskComplete {
		t.Fatalf("markDone updates = %+v, want in-progress then completion", updates)
	}
}

func TestProgressTerminalUpdateKeepsInitialStatusAndRejectsLateStarts(t *testing.T) {
	st := newTurnState()
	log := discardLogger()
	st.feed(toolEvent(t, "tool_execution_start", "call-1", "bash", false), log)
	st.finishOpenTasks()
	st.feed(toolEvent(t, "tool_execution_start", "call-2", "read", false), log)

	updates := st.takeTaskUpdates()
	if len(updates) != 2 || updates[0].Status != TaskInProgress || updates[1].Status != TaskError || updates[0].ID != turnProgressTaskID || updates[1].ID != turnProgressTaskID {
		t.Fatalf("terminal updates = %+v, want in-progress then error on the same task", updates)
	}
	if len(st.activeTasks) != 0 || len(st.takeTaskUpdates()) != 0 {
		t.Fatal("a late start reopened a terminal progress task")
	}
}

func TestAbortResponseWinsSettlementRace(t *testing.T) {
	st := newTurnState()
	log := discardLogger()
	st.feed(toolEvent(t, "tool_execution_start", "call-1", "bash", false), log)
	if got := st.takeTaskUpdates(); len(got) != 1 || got[0].Status != TaskInProgress {
		t.Fatalf("initial progress = %+v", got)
	}

	tracked := st.beginAbort()
	st.feed(gwclient.Event{Type: "agent_settled"}, log)
	if !st.done() {
		t.Fatal("settlement did not mark the turn boundary")
	}
	select {
	case <-st.doneCh:
		t.Fatal("renderer wait closed before the abort result arrived")
	default:
	}
	if len(st.takeTaskUpdates()) != 0 {
		t.Fatal("settlement published a terminal task before the abort result arrived")
	}

	st.resolveAbort(tracked, true)
	if !st.done() {
		t.Fatal("settled turn did not finish after abort resolution")
	}
	if got := st.takeTaskUpdates(); len(got) != 1 || got[0].Status != TaskError {
		t.Fatalf("aborted turn update = %+v, want error after confirmed abort", got)
	}
}

func TestAgentStartDuringAbortWaitCreatesNextTurn(t *testing.T) {
	log := discardLogger()
	lifetime, cancel := context.WithCancel(context.Background())
	cancel()
	th := &thread{b: &Bot{lifetime: lifetime, log: log}, log: log}
	previous := newTurnState()
	th.cur.Store(previous)
	previous.feed(gwclient.Event{Type: "agent_start", Seq: 10}, log)
	tracked := previous.beginAbort()
	previous.feed(gwclient.Event{Type: "agent_settled", Seq: 11}, log)

	// Hold the renderer lock so its goroutine cannot finish and release the new
	// turn reservation before the callback's routing decision is inspected.
	th.turnMu.Lock()
	th.onSessionEventWithClient("", gwclient.Event{Type: "agent_start", Seq: 12}, nil, th.sessionEpoch.Load())
	next := th.cur.Load()
	if next == nil || next == previous {
		th.turnMu.Unlock()
		t.Fatal("agent_start during abort wait was not assigned a new turn")
	}
	th.turnMu.Unlock()
	select {
	case <-next.finishedCh:
	case <-time.After(time.Second):
		t.Fatal("test renderer did not release the next turn")
	}
	previous.resolveAbort(tracked, true)
}

func TestReplayedAgentStartDoesNotReplaceDoneTurn(t *testing.T) {
	st := newTurnState()
	st.feed(gwclient.Event{Type: "agent_start", Seq: 10}, discardLogger())
	st.markDone()
	th := &thread{log: discardLogger()}
	th.cur.Store(st)

	th.onSessionEventWithClient("", gwclient.Event{Type: "agent_start", Seq: 10}, nil, th.sessionEpoch.Load())
	if th.cur.Load() != st {
		t.Fatal("replayed agent_start replaced the completed turn before sequence deduplication")
	}
}

func TestObservedCleanupWaitsForAbortBeforeFinalizingProgress(t *testing.T) {
	st := newTurnState()
	st.feed(toolEvent(t, "tool_execution_start", "call-1", "bash", false), discardLogger())
	st.takeTaskUpdates()
	tracked := st.beginAbort()
	st.markDone()

	cleanupStarted := make(chan struct{})
	cleanupDone := make(chan struct{})
	go func() {
		close(cleanupStarted)
		_ = st.waitAbortResolution(context.Background())
		st.finishOpenTasks()
		close(cleanupDone)
	}()
	<-cleanupStarted
	select {
	case <-cleanupDone:
		t.Fatal("observed cleanup passed an unresolved abort")
	default:
	}

	st.resolveAbort(tracked, true)
	select {
	case <-cleanupDone:
	case <-time.After(time.Second):
		t.Fatal("observed cleanup did not resume after abort resolution")
	}
	if got := st.takeTaskUpdates(); len(got) != 1 || got[0].Status != TaskError {
		t.Fatalf("final progress update = %+v, want an error before cleanup ends", got)
	}
}

func TestFailedAbortLeavesSettledProgressComplete(t *testing.T) {
	st := newTurnState()
	log := discardLogger()
	st.feed(toolEvent(t, "tool_execution_start", "call-1", "bash", false), log)
	st.takeTaskUpdates()

	tracked := st.beginAbort()
	st.feed(gwclient.Event{Type: "agent_settled"}, log)
	st.resolveAbort(tracked, false)
	if !st.done() {
		t.Fatal("settled turn did not finish after failed abort")
	}
	if got := st.takeTaskUpdates(); len(got) != 1 || got[0].Status != TaskComplete {
		t.Fatalf("non-aborted turn update = %+v, want complete", got)
	}
}

func TestReplayedEventsDoNotDuplicateProgressMarkers(t *testing.T) {
	st := newTurnState()
	log := discardLogger()
	marker := textDelta(t, "[[slack-progress: First action]]")
	marker.Seq = 10
	start := toolEvent(t, "tool_execution_start", "call-1", "bash", false)
	start.Seq = 11
	st.feed(marker, log)
	st.feed(start, log)
	if got := st.takeTaskUpdates(); len(got) != 1 || got[0].Title != "First action" {
		t.Fatalf("first action = %+v", got)
	}

	marker.Seq = 10
	start.Seq = 11
	st.feed(marker, log)
	st.feed(start, log)
	if got := st.takeTaskUpdates(); len(got) != 0 || len(st.pendingProgress) != 0 {
		t.Fatalf("replayed events produced updates or stale titles: updates=%+v queued=%v", got, st.pendingProgress)
	}

	nextMarker := textDelta(t, "[[slack-progress: Second action]]")
	nextMarker.Seq = 12
	nextStart := toolEvent(t, "tool_execution_start", "call-2", "bash", false)
	nextStart.Seq = 13
	st.feed(nextMarker, log)
	st.feed(nextStart, log)
	if got := st.takeTaskUpdates(); len(got) != 1 || got[0].Title != "Second action" {
		t.Fatalf("second action = %+v, want no stale replayed title", got)
	}
}

func TestGatewaySnapshotResetsSequenceWatermark(t *testing.T) {
	st := newTurnState()
	log := discardLogger()
	marker := textDelta(t, "[[slack-progress: Before restart]]")
	marker.Seq = 100
	start := toolEvent(t, "tool_execution_start", "old-call", "bash", false)
	start.Seq = 101
	st.feed(marker, log)
	st.feed(start, log)
	st.takeTaskUpdates()

	st.feed(gwclient.Event{Type: "gw_snapshot", Raw: []byte(`{"type":"gw_snapshot","headSeq":5}`)}, log)
	nextMarker := textDelta(t, "[[slack-progress: After restart]]")
	nextMarker.Seq = 6
	nextStart := toolEvent(t, "tool_execution_start", "new-call", "bash", false)
	nextStart.Seq = 7
	st.feed(nextMarker, log)
	st.feed(nextStart, log)
	if got := st.takeTaskUpdates(); len(got) != 1 || got[0].Title != "After restart" {
		t.Fatalf("post-snapshot task update = %+v", got)
	}
	settled := gwclient.Event{Type: "agent_settled", Seq: 8}
	st.feed(settled, log)
	if !st.done() {
		t.Fatal("turn did not settle after sequence reset")
	}
}

func TestOversizedToolIdentifiersAreNotRetained(t *testing.T) {
	st := newTurnState()
	log := discardLogger()
	longID := strings.Repeat("x", maxToolCallIDBytes+1)
	st.feed(toolEvent(t, "tool_execution_start", longID, "bash", false), log)
	if len(st.activeTasks) != 0 || len(st.seenToolCalls) != 0 || len(st.takeTaskUpdates()) != 0 {
		t.Fatal("oversized tool call ID was retained as progress state")
	}
	longName := strings.Repeat("x", maxToolNameBytes+1)
	st.feed(toolEvent(t, "tool_execution_start", "call-1", longName, false), log)
	if len(st.activeTasks) != 0 || len(st.seenToolCalls) != 0 {
		t.Fatal("oversized tool name was retained as progress state")
	}
}

func TestOversizedProgressMarkerIsRemovedFromReply(t *testing.T) {
	log := discardLogger()
	content := strings.Repeat("x", maxProgressTitleRunes+1)
	marker := progressMarkerOpen + content + " tail" + progressMarkerClose
	want := "Before  after"

	streamed := newTurnState()
	streamed.feed(textDelta(t, "Before "+progressMarkerOpen+content), log)
	streamed.feed(textDelta(t, " tail"+progressMarkerClose+" after"), log)
	if got := streamed.text(); got != want {
		t.Fatalf("streamed oversized marker = %q, want %q", got, want)
	}
	if got := streamed.authoritativeText("Before " + marker + " after"); got != want {
		t.Fatalf("authoritative oversized marker = %q, want %q", got, want)
	}

	recovered := newTurnState()
	recovered.replace("Before " + marker + " after")
	if got := recovered.text(); got != want {
		t.Fatalf("recovered oversized marker = %q, want %q", got, want)
	}
}

func TestProgressTaskUsesOneStableIDForTheTurn(t *testing.T) {
	st := newTurnState()
	log := discardLogger()
	startedAt := time.Unix(4_000, 0)
	st.progressStartedAt = startedAt
	for i := range 9 {
		title := "Check step " + strings.Repeat("x", i+1)
		st.feed(textDelta(t, "[[slack-progress: "+title+"]]"), log)
	}
	for i := range 9 {
		id := "call-" + strings.Repeat("x", i+1)
		st.feed(toolEvent(t, "tool_execution_start", id, "bash", false), log)
		st.feed(toolEvent(t, "tool_execution_end", id, "bash", false), log)
	}

	updates := st.takeTaskUpdates()
	if len(updates) != 2 || updates[0].ID != turnProgressTaskID || updates[0].Title != "Check step x" || updates[0].Status != TaskInProgress || updates[1].ID != turnProgressTaskID || updates[1].Title != "Check step "+strings.Repeat("x", 9) || updates[1].Status != TaskInProgress {
		t.Fatalf("coalesced task updates = %+v, want first and latest in-progress titles", updates)
	}
	if len(st.activeTasks) != 1 || len(st.seenToolCalls) != 9 {
		t.Fatalf("active tasks=%d seen calls=%d, want one task and nine calls", len(st.activeTasks), len(st.seenToolCalls))
	}
	if len(st.progressActionTitles) != maxProgressDetailTitles {
		t.Fatalf("retained action titles = %d, want bounded cap %d", len(st.progressActionTitles), maxProgressDetailTitles)
	}

	st.finishProgressTaskAt(TaskComplete, startedAt.Add(45*time.Second))
	updates = st.takeTaskUpdates()
	wantDetails := "Completed actions:\n• Check step x\n• Check step xx\n• Check step xxx\n• Check step xxxx\n• Check step xxxxx\n• Check step xxxxxx\n• 3 additional action(s) not listed"
	if len(updates) != 1 || updates[0].ID != turnProgressTaskID || updates[0].Title != "Completed · 9 actions · 45s" || updates[0].Details != wantDetails || updates[0].Status != TaskComplete {
		t.Fatalf("completed task update = %+v", updates)
	}

	st.feed(gwclient.Event{Type: "agent_settled"}, log)
	st.feed(toolEvent(t, "tool_execution_start", "late-call", "bash", false), log)
	if len(st.activeTasks) != 0 || len(st.takeTaskUpdates()) != 0 {
		t.Fatal("a late tool event reopened progress after the turn settled")
	}
}

func TestProgressSummaryFormatsActionCountAndDuration(t *testing.T) {
	tests := []struct {
		name        string
		status      TaskProgressStatus
		actions     int
		elapsed     time.Duration
		wantSummary string
	}{
		{name: "singular under one second", status: TaskComplete, actions: 1, elapsed: 500 * time.Millisecond, wantSummary: "Completed · 1 action · <1s"},
		{name: "plural rounded duration", status: TaskComplete, actions: 2, elapsed: 65 * time.Second, wantSummary: "Completed · 2 actions · 1m5s"},
		{name: "incomplete turn", status: TaskError, actions: 0, elapsed: time.Second, wantSummary: "Incomplete · 0 actions · 1s"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := summarizeProgressMetrics(tt.status, tt.actions, tt.elapsed); got != tt.wantSummary {
				t.Fatalf("summary = %q, want %q", got, tt.wantSummary)
			}
		})
	}
}

func TestCompletedActionCountUsesUniqueToolEnds(t *testing.T) {
	st := newTurnState()
	log := discardLogger()
	startedAt := time.Unix(5_000, 0)
	st.progressStartedAt = startedAt

	st.feed(toolEvent(t, "tool_execution_start", "completed-1", "bash", false), log)
	st.takeTaskUpdates()
	end := toolEvent(t, "tool_execution_end", "completed-1", "bash", false)
	st.feed(end, log)
	st.feed(end, log)

	st.feed(toolEvent(t, "tool_execution_start", "completed-2", "bash", false), log)
	st.takeTaskUpdates()
	st.feed(toolEvent(t, "tool_execution_end", "completed-2", "bash", true), log)

	st.feed(toolEvent(t, "tool_execution_start", "still-running", "bash", false), log)
	st.takeTaskUpdates()
	st.finishProgressTaskAt(TaskComplete, startedAt.Add(125*time.Second))

	updates := st.takeTaskUpdates()
	if len(updates) != 1 || updates[0].Title != "Completed · 2 actions · 2m5s" {
		t.Fatalf("completion summary = %+v, want two returned calls and elapsed time", updates)
	}
}

func TestProgressDurationStartsAtFirstTurnStart(t *testing.T) {
	st := newTurnState()
	log := discardLogger()
	st.feed(gwclient.Event{Type: "agent_start"}, log)
	startedAt := st.progressStartedAt
	if startedAt.IsZero() {
		t.Fatal("agent_start did not establish the progress start time")
	}

	st.feed(gwclient.Event{Type: "turn_start"}, log)
	if !st.progressStartedAt.Equal(startedAt) {
		t.Fatal("a later turn_start reset the original progress start time")
	}
}

func TestDuplicateToolStartDoesNotConsumeNextProgressTitle(t *testing.T) {
	st := newTurnState()
	log := discardLogger()
	st.feed(textDelta(t, "[[slack-progress: First action]]"), log)
	st.feed(toolEvent(t, "tool_execution_start", "call-1", "bash", false), log)
	if got := st.takeTaskUpdates(); len(got) != 1 || got[0].Title != "First action" {
		t.Fatalf("first action = %+v", got)
	}

	st.feed(textDelta(t, "[[slack-progress: Second action]]"), log)
	st.feed(toolEvent(t, "tool_execution_start", "call-1", "bash", false), log)
	if got := st.takeTaskUpdates(); len(got) != 0 {
		t.Fatalf("duplicate start emitted an update: %+v", got)
	}
	if len(st.pendingProgress) != 1 {
		t.Fatalf("queued titles after duplicate start = %d, want one", len(st.pendingProgress))
	}

	st.feed(toolEvent(t, "tool_execution_start", "call-2", "bash", false), log)
	if got := st.takeTaskUpdates(); len(got) != 1 || got[0].Title != "Second action" {
		t.Fatalf("second action = %+v, want marker preserved for next call", got)
	}
}

func TestProgressCaptureAndTitleQueueAreBounded(t *testing.T) {
	log := discardLogger()
	st := newTurnState()
	overlong := strings.Repeat("x", maxProgressTitleRunes+1)
	st.feed(textDelta(t, progressMarkerOpen+overlong), log)
	if st.capturingProgress || st.progressCapture.Len() != 0 {
		t.Fatalf("oversized marker retained capture state: capturing=%v bytes=%d", st.capturingProgress, st.progressCapture.Len())
	}
	st.startMessage()
	st.feed(textDelta(t, "Final answer"), log)
	if got := st.text(); got != "Final answer" {
		t.Fatalf("text after an oversized marker boundary = %q, want final answer", got)
	}

	st = newTurnState()
	for range maxPendingProgressTitles + 1 {
		st.feed(textDelta(t, "[[slack-progress: Check service]]"), log)
	}
	if len(st.pendingProgress) != maxPendingProgressTitles {
		t.Fatalf("queued titles = %d, want cap %d", len(st.pendingProgress), maxPendingProgressTitles)
	}
	for i := range maxPendingProgressTitles {
		id := "queued-" + strings.Repeat("x", i+1)
		st.feed(toolEvent(t, "tool_execution_start", id, "bash", false), log)
		if got := st.takeTaskUpdates(); len(got) != 1 || got[0].Title != "Check service" {
			t.Fatalf("queued progress %d = %+v", i, got)
		}
	}
	st.feed(toolEvent(t, "tool_execution_start", "overflow", "bash", false), log)
	if got := st.takeTaskUpdates(); len(got) != 1 || got[0].Title != "Running a command" {
		t.Fatalf("overflow should use the generic title: %+v", got)
	}
	if len(st.seenToolCalls) != maxSeenToolCallIDs {
		t.Fatalf("seen tool-call IDs = %d, want bounded cap %d", len(st.seenToolCalls), maxSeenToolCallIDs)
	}
}

func TestTurnStateCanRecoverTheFinalTextAfterReconnect(t *testing.T) {
	st := newTurnState()
	st.append("partial")
	if got := st.takePending(); got != "partial" {
		t.Fatalf("pending text = %q, want partial", got)
	}

	st.replace("partial answer")
	if got := st.text(); got != "partial answer" {
		t.Fatalf("full text = %q, want partial answer", got)
	}
	if got := st.takePending(); got != " answer" {
		t.Fatalf("pending text = %q, want only the suffix not yet rendered", got)
	}

	st = newTurnState()
	st.append("old partial")
	st.takePending()
	st.replace("corrected answer")
	if got := st.takePending(); got != "" {
		t.Fatalf("pending text = %q, want no duplicate for a divergent correction", got)
	}
}

func TestReconcileObservedTextRecoversMissedDeltas(t *testing.T) {
	before := sha256.Sum256([]byte("previous answer"))

	st := newTurnState()
	if !reconcileObservedText(st, "completed while disconnected", before, true) {
		t.Fatal("a changed final answer should be recovered even when no deltas arrived")
	}
	if got := st.text(); got != "completed while disconnected" {
		t.Fatalf("recovered text = %q", got)
	}

	st = newTurnState()
	st.append("partial")
	if !reconcileObservedText(st, "partial plus missed text", before, true) {
		t.Fatal("the authoritative final answer should replace a partial stream")
	}
	if got := st.text(); got != "partial plus missed text" {
		t.Fatalf("reconciled text = %q", got)
	}

	st = newTurnState()
	if reconcileObservedText(st, "previous answer", before, true) {
		t.Fatal("a textless wake should not replay the previous answer")
	}
}

func TestTurnStateSeparatesAssistantMessages(t *testing.T) {
	st := newTurnState()
	log := discardLogger()

	st.feed(gwclient.Event{Type: "message_start"}, log)
	st.feed(textDelta(t, "First "), log)
	st.feed(textDelta(t, "response."), log)
	if got := st.takePending(); got != "First response." {
		t.Fatalf("first message = %q, want %q", got, "First response.")
	}

	st.feed(gwclient.Event{Type: "message_start"}, log)
	st.feed(textDelta(t, "Second"), log)
	if got := st.takePending(); got != "\n\nSecond" {
		t.Fatalf("next message delta = %q, want a paragraph break before it", got)
	}
	if got := st.text(); got != "First response.\n\nSecond" {
		t.Fatalf("full text = %q, want both messages separated by a paragraph", got)
	}
	if got := st.authoritativeText("Final second response."); got != "First response.\n\nFinal second response." {
		t.Fatalf("authoritative text = %q, want prior message and corrected final response", got)
	}
}

func TestTurnStateSkipsEmptyAssistantMessages(t *testing.T) {
	st := newTurnState()
	log := discardLogger()

	st.feed(gwclient.Event{Type: "message_start"}, log)
	st.feed(gwclient.Event{Type: "message_end"}, log)
	st.feed(gwclient.Event{Type: "message_start"}, log)
	st.feed(textDelta(t, "The answer."), log)

	if got := st.text(); got != "The answer." {
		t.Fatalf("text after an empty message = %q, want no leading separator", got)
	}
}

func TestTurnStateIgnoresThinkingDeltas(t *testing.T) {
	st := newTurnState()

	// Thinking arrives in the same shape with a different type; rendering it as
	// the answer would show the user the model's private reasoning.
	st.feed(gwclient.Event{
		Type: "message_update",
		Raw:  []byte(`{"type":"message_update","assistantMessageEvent":{"type":"thinking_delta","delta":"secret"}}`),
	}, discardLogger())

	if got := st.text(); got != "" {
		t.Fatalf("thinking leaked into the answer: %q", got)
	}
	if !st.sawWork() {
		t.Fatal("a thinking delta still means the turn is running")
	}
}

func TestTurnStateLifecycleEvents(t *testing.T) {
	log := discardLogger()

	t.Run("running and settled", func(t *testing.T) {
		st := newTurnState()
		st.feed(gwclient.Event{Type: "gw_turn", Raw: []byte(`{"type":"gw_turn","state":"running"}`)}, log)
		if !st.sawWork() {
			t.Fatal("gw_turn running should mark work")
		}
		select {
		case <-st.doneCh:
			t.Fatal("settled too early")
		default:
		}
		st.feed(gwclient.Event{Type: "gw_turn", Raw: []byte(`{"type":"gw_turn","state":"settled"}`)}, log)
		select {
		case <-st.doneCh:
		default:
			t.Fatal("gw_turn settled should mark the turn done")
		}
	})

	t.Run("agent settled", func(t *testing.T) {
		st := newTurnState()
		st.feed(gwclient.Event{Type: "agent_settled", Raw: []byte(`{"type":"agent_settled"}`)}, log)
		select {
		case <-st.doneCh:
		default:
			t.Fatal("agent_settled should mark the turn done")
		}
	})

	t.Run("a finished message is not a finished turn", func(t *testing.T) {
		// Observed against the real daemon: one prompt emitted two messages, the
		// first of them empty. Treating that message_end as the end of the turn
		// stopped the flusher before the answer had streamed at all.
		st := newTurnState()
		for _, typ := range []string{"message_start", "message_end", "turn_end", "agent_end"} {
			st.feed(gwclient.Event{Type: typ, Raw: []byte(`{"type":"` + typ + `"}`)}, log)
		}
		if !st.sawWork() {
			t.Fatal("these events should still count as the turn running")
		}
		select {
		case <-st.doneCh:
			t.Fatal("message_end and turn_end must not end the turn")
		default:
		}

		// Text arriving after them is still rendered.
		st.feed(textDelta(t, "late answer"), log)
		if got := st.takePending(); got != "late answer" {
			t.Fatalf("pending text = %q, want the late answer", got)
		}
	})
}

func TestWaitStarted(t *testing.T) {
	log := discardLogger()

	t.Run("work", func(t *testing.T) {
		st := newTurnState()
		go func() {
			time.Sleep(5 * time.Millisecond)
			st.feed(textDelta(t, "x"), log)
		}()
		if err := st.waitStarted(context.Background(), time.Second); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("done without work", func(t *testing.T) {
		st := newTurnState()
		st.markDone()
		if err := st.waitStarted(context.Background(), time.Second); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("grace expires", func(t *testing.T) {
		st := newTurnState()
		// A silent turn is not an error here: the caller decides what it means.
		if err := st.waitStarted(context.Background(), time.Millisecond); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("context canceled", func(t *testing.T) {
		st := newTurnState()
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		if err := st.waitStarted(ctx, time.Second); !errors.Is(err, context.Canceled) {
			t.Fatalf("err = %v, want context.Canceled", err)
		}
	})
}

// captureLogger returns a logger and the buffer it writes to.
func captureLogger() (*slog.Logger, *strings.Builder) {
	var buf strings.Builder
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	return logger, &buf
}

// uiEvent builds an extension_ui_request frame.
func uiEvent(t *testing.T, body string) gwclient.Event {
	t.Helper()
	raw := `{"type":"extension_ui_request",` + body + `}`
	if !json.Valid([]byte(raw)) {
		t.Fatalf("test built invalid JSON: %s", raw)
	}
	return gwclient.Event{Type: "extension_ui_request", Raw: []byte(raw)}
}

func TestFireAndForgetUINeedsNoAnswer(t *testing.T) {
	// pi-lens emits setStatus on every turn, `--approve` does not silence it,
	// and nothing waits for an answer. Warning about it would train the reader
	// to ignore warnings.
	log, logs := captureLogger()
	st := newTurnState()
	st.feed(uiEvent(t, `"id":"1","method":"setStatus","statusKey":"pi-lens-lsp",`+
		`"statusText":"\u001b[38;5;241mLSP Inactive\u001b[39m"`), log)

	if strings.Contains(logs.String(), "level=WARN") {
		t.Fatalf("a fire-and-forget request should not warn: %s", logs.String())
	}
	if !strings.Contains(logs.String(), "setStatus") {
		t.Fatalf("it should still be visible at debug level: %s", logs.String())
	}
	// It is not a turn boundary either.
	select {
	case <-st.doneCh:
		t.Fatal("a notification must not end the turn")
	default:
	}
}

func TestBlockingUIDialogWarns(t *testing.T) {
	// A dialog stalls the turn until pi times out, which the operator should
	// learn from the journal rather than from a thread that never answers.
	log, logs := captureLogger()
	st := newTurnState()
	st.feed(uiEvent(t, `"id":"q1","method":"confirm","title":"Run this?",`+
		`"message":"\u001b[1mrm -rf build\u001b[0m"`), log)

	got := logs.String()
	if !strings.Contains(got, "level=WARN") || !strings.Contains(got, "confirm") {
		t.Fatalf("a blocking dialog should warn and name its method: %s", got)
	}
	if strings.Contains(got, "\\u001b") {
		t.Fatalf("the message should be stripped of escapes: %s", got)
	}
}

func TestNotifyWarningIsSurfaced(t *testing.T) {
	log, logs := captureLogger()
	st := newTurnState()
	st.feed(uiEvent(t, `"id":"1","method":"notify","notifyType":"warning","text":"disk nearly full"`), log)

	if got := logs.String(); !strings.Contains(got, "level=WARN") || !strings.Contains(got, "disk nearly full") {
		t.Fatalf("a warning notification should reach the journal: %s", got)
	}
}

func TestStripANSI(t *testing.T) {
	tests := []struct{ in, want string }{
		{"plain", "plain"},
		{"\x1b[38;5;241mLSP Inactive\x1b[39m", "LSP Inactive"},
		{"\x1b[1mbold\x1b[0m and \x1b[4munderline\x1b[0m", "bold and underline"},
		{"trailing\x1b", "trailing\x1b"}, // a lone ESC is not a sequence
		{"", ""},
	}
	for _, test := range tests {
		if got := stripANSI(test.in); got != test.want {
			t.Errorf("stripANSI(%q) = %q, want %q", test.in, got, test.want)
		}
	}
}

// recordingRenderer records what a turn renders.
type recordingRenderer struct {
	mu       sync.Mutex
	starts   int
	startErr error
	deltas   []string
	tasks    []TaskProgress
	finals   []string
	failure  string
	signal   chan struct{} // if set, receives a value on every Delta
}

func (r *recordingRenderer) Start(context.Context) error {
	r.mu.Lock()
	r.starts++
	err := r.startErr
	r.mu.Unlock()
	return err
}

func (r *recordingRenderer) UpdateTasks(_ context.Context, tasks []TaskProgress) error {
	r.mu.Lock()
	r.tasks = append(r.tasks, tasks...)
	r.mu.Unlock()
	return nil
}

func (r *recordingRenderer) Delta(_ context.Context, text string) error {
	r.mu.Lock()
	r.deltas = append(r.deltas, text)
	r.mu.Unlock()
	if r.signal != nil {
		select {
		case r.signal <- struct{}{}:
		default:
		}
	}
	return nil
}

func (r *recordingRenderer) Finish(_ context.Context, final string) error {
	r.mu.Lock()
	r.finals = append(r.finals, final)
	r.mu.Unlock()
	return nil
}

func (r *recordingRenderer) Fail(_ context.Context, cause error) error {
	r.mu.Lock()
	r.failure = cause.Error()
	r.mu.Unlock()
	return nil
}

func (r *recordingRenderer) finalAnswers() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.finals...)
}

func (r *recordingRenderer) rendered() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return strings.Join(r.deltas, "")
}

func (r *recordingRenderer) taskProgress() []TaskProgress {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]TaskProgress(nil), r.tasks...)
}

func (r *recordingRenderer) startCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.starts
}

func TestFlushRetainsTextWhenReplyStartFails(t *testing.T) {
	cfg := config.Defaults()
	cfg.Render.FlushMS = 60000
	b := &Bot{cfg: cfg, log: discardLogger()}
	th := &thread{key: "test", b: b, log: b.log}
	r := &recordingRenderer{startErr: errors.New("Slack unavailable")}
	st := newTurnState()

	stop := th.flush(context.Background(), st, r)
	st.feed(textDelta(t, "partial answer"), discardLogger())
	stop()

	if got := r.rendered(); got != "partial answer" {
		t.Fatalf("buffered text = %q, want partial answer available to Fail", got)
	}
}

func TestObservedTurnRendersTaskProgressWithoutAssistantText(t *testing.T) {
	b, platform, st := newTestBot(t)
	b.lifetime = context.Background()
	th := testThread(t, b, st)
	r := &recordingRenderer{}
	platform.renderer = r
	turn := newTurnState()
	turn.feed(toolEvent(t, "tool_execution_start", "call-1", "read", false), discardLogger())
	turn.feed(toolEvent(t, "tool_execution_end", "call-1", "read", false), discardLogger())
	turn.feed(gwclient.Event{Type: "agent_settled"}, discardLogger())

	th.renderObservedTurn(turn, nil)

	if got := platform.startedTurns(); got != 1 {
		t.Fatalf("renderer starts = %d, want a task-only reply", got)
	}
	if got := r.taskProgress(); len(got) != 2 || got[0].ID != turnProgressTaskID || got[0].Status != TaskInProgress || got[1].ID != turnProgressTaskID || got[1].Status != TaskComplete {
		t.Fatalf("task updates = %+v, want in-progress followed by completion", got)
	}
	if got := r.startCount(); got != 0 {
		t.Fatalf("empty text reply started %d times", got)
	}
	if got := r.finalAnswers(); len(got) != 1 || got[0] != "" {
		t.Fatalf("task-only stream finalization = %q, want an empty final answer", got)
	}
}

func TestFlushForwardsTaskProgressBeforeAnyAssistantText(t *testing.T) {
	cfg := config.Defaults()
	cfg.Render.FlushMS = 60000
	b := &Bot{cfg: cfg, log: discardLogger()}
	th := &thread{key: "test", b: b, log: b.log}
	r := &recordingRenderer{}
	st := newTurnState()

	stop := th.flush(context.Background(), st, r)
	st.feed(toolEvent(t, "tool_execution_start", "call-1", "read", false), discardLogger())
	waitFor(t, "task progress", func() bool { return len(r.taskProgress()) == 1 })
	if r.startCount() != 0 {
		t.Fatalf("task progress started an empty text reply %d times", r.startCount())
	}

	st.feed(textDelta(t, "answer"), discardLogger())
	waitFor(t, "assistant text", func() bool { return r.rendered() == "answer" })
	stop()
	if got := r.taskProgress()[0]; got.Title != "Reading files" || got.Status != TaskInProgress {
		t.Fatalf("reported task = %+v", got)
	}
}

func TestFlushThinkingOnlyDoesNotStartReply(t *testing.T) {
	cfg := config.Defaults()
	cfg.Render.FlushMS = 60000
	b := &Bot{cfg: cfg, log: discardLogger()}
	th := &thread{key: "test", b: b, log: b.log}
	r := &recordingRenderer{}
	st := newTurnState()

	stop := th.flush(context.Background(), st, r)
	st.feed(gwclient.Event{
		Type: "message_update",
		Raw:  []byte(`{"type":"message_update","assistantMessageEvent":{"type":"thinking_delta","delta":"private reasoning"}}`),
	}, discardLogger())
	stop()

	if got := r.startCount(); got != 0 {
		t.Fatalf("Start called %d times while only thinking was emitted", got)
	}
}

func TestFlushStartsImmediatelyOnFirstText(t *testing.T) {
	cfg := config.Defaults()
	cfg.Render.FlushMS = 60000
	b := &Bot{cfg: cfg, log: discardLogger()}
	th := &thread{key: "test", b: b, log: b.log}
	r := &recordingRenderer{signal: make(chan struct{}, 8)}
	st := newTurnState()

	stop := th.flush(context.Background(), st, r)
	st.feed(textDelta(t, "visible answer"), discardLogger())
	defer stop()

	select {
	case <-r.signal:
	case <-time.After(2 * time.Second):
		t.Fatalf("first text was not rendered immediately; got %q", r.rendered())
	}
	if got := r.startCount(); got != 1 {
		t.Fatalf("Start called %d times for the first text chunk, want 1", got)
	}
}

func TestFlushRendersPendingText(t *testing.T) {
	cfg := config.Defaults()
	cfg.Render.FlushMS = 5
	b := &Bot{cfg: cfg, log: discardLogger()}
	th := &thread{key: "test", b: b, log: b.log}
	r := &recordingRenderer{signal: make(chan struct{}, 8)}
	st := newTurnState()

	stop := th.flush(context.Background(), st, r)
	st.append("hello ")
	st.append("world")

	// Wait for the flusher rather than for the clock.
	timeout := time.After(2 * time.Second)
	for r.rendered() != "hello world" {
		select {
		case <-r.signal:
		case <-timeout:
			t.Fatalf("rendered %q, want %q", r.rendered(), "hello world")
		}
	}
	stop()

	// Stopping twice must be safe: a turn stops the flusher before it finishes
	// and again on the way out.
	stop()
}

func TestCanceledTurnMarksOriginalRequestFailed(t *testing.T) {
	b, platform, st := newTestBot(t)
	th := testThread(t, b, st)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	message := newTestMessage("failed-turn", "hello")

	b.startTurn(ctx, th, message)
	waitFor(t, "failed request reaction", func() bool { return len(platform.failedRequests()) == 1 })
	if got := platform.failedRequests()[0].TS; got != message.TS {
		t.Errorf("failed request timestamp = %q, want %q", got, message.TS)
	}
	if got := len(platform.completedRequests()); got != 0 {
		t.Errorf("completed %d requests after the turn failed", got)
	}
	assertFailureReporterUsesLiveBoundedContext(t, platform)
}

func TestStartTurnErrorMarksOriginalRequestFailed(t *testing.T) {
	b, platform, st := newTestBot(t)
	platform.startErr = errors.New("cannot start renderer")
	th := testThread(t, b, st)
	message := newTestMessage("start-failure", "hello")

	b.startTurn(context.Background(), th, message)
	waitFor(t, "failed request reaction", func() bool { return len(platform.failedRequests()) == 1 })
	if got := platform.startedTurns(); got != 1 {
		t.Errorf("StartTurn calls = %d, want 1", got)
	}
	if got := platform.failedRequests()[0].TS; got != message.TS {
		t.Errorf("failed request timestamp = %q, want %q", got, message.TS)
	}
	if got := len(platform.completedRequests()); got != 0 {
		t.Errorf("completed %d requests after StartTurn failed", got)
	}
	assertFailureReporterUsesLiveBoundedContext(t, platform)
}

func assertFailureReporterUsesLiveBoundedContext(t *testing.T, platform *fakePlatform) {
	t.Helper()
	reports := platform.requestFailureReports()
	if len(reports) != 1 {
		t.Fatalf("failure reports = %d, want 1", len(reports))
	}
	if reports[0].contextErr != nil {
		t.Errorf("failure reporter context error = %v, want a live context", reports[0].contextErr)
	}
	if !reports[0].hasDeadline {
		t.Fatal("failure reporter context has no deadline")
	}
	if remaining := time.Until(reports[0].deadline); remaining <= 0 || remaining > requestReactionTimeout {
		t.Errorf("failure reporter deadline has %v remaining, want >0 and <=%v", remaining, requestReactionTimeout)
	}
}

func TestRunTurnSetupFailureDoesNotCompleteRequest(t *testing.T) {
	b, platform, st := newTestBot(t)
	th := testThread(t, b, st)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	r := &recordingRenderer{}

	err := th.runTurn(ctx, newTestMessage("setup-failure", "hello"), r)
	if err == nil {
		t.Fatal("runTurn with a canceled context unexpectedly succeeded")
	}
	if got := len(platform.completedRequests()); got != 0 {
		t.Errorf("completed %d requests after setup failed before any output", got)
	}
	if got := r.startCount(); got != 0 {
		t.Errorf("Start called %d times despite failure before output", got)
	}
}

func TestFlushRendersBeforeExiting(t *testing.T) {
	// A short answer can arrive entirely between two ticks. Dropping it would
	// mean a thread that shows nothing until the turn ends, so the flusher
	// renders once on the way out.
	cfg := config.Defaults()
	cfg.Render.FlushMS = 60000
	b := &Bot{cfg: cfg, log: discardLogger()}
	th := &thread{key: "test", b: b, log: b.log}
	r := &recordingRenderer{}
	st := newTurnState()

	stop := th.flush(context.Background(), st, r)
	st.append("P")
	st.append("ONG")
	st.markDone() // the turn ends before the first tick is due
	stop()

	if got := r.rendered(); got != "PONG" {
		t.Fatalf("rendered %q, want the pending text to be flushed on exit", got)
	}
}
