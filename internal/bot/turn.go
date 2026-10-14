package bot

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode"

	"github.com/tigersoldier/pi-gateway/gwclient"
	"github.com/tigersoldier/pi-gateway/protocol"
)

// startGrace bounds how long a turn waits for evidence that the agent actually
// started. pi publishes agent_start before doing any work, so this is only
// reached when a prompt is accepted and then produces nothing.
const (
	startGrace                = 15 * time.Second
	assistantMessageSeparator = "\n\n"
	progressMarkerOpen        = "[[slack-progress:"
	progressMarkerClose       = "]]"
	maxProgressTitleRunes     = 80
	maxPendingProgressTitles  = 64
	maxSeenToolCallIDs        = 64
	maxToolCallIDBytes        = 256
	maxToolNameBytes          = 256
	maxProgressSummaryTitles  = 2
	maxProgressDetailTitles   = 6
	maxProgressDetailsRunes   = 600
	turnProgressTaskID        = "turn-progress"
)

// runTurn prompts the thread's session and renders the answer.
func (th *thread) runTurn(ctx context.Context, m Message, r Renderer) error {
	turnCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	ctx = turnCtx

	var st *turnState
	held := false
	waited := false
	for {
		if !th.turnMu.TryLock() {
			waited = true
			th.turnMu.Lock()
		}
		held = true
		active := th.cur.Load()
		if active == nil {
			st = newPromptTurnState()
			st.cancel = cancel
			if th.cur.CompareAndSwap(nil, st) {
				break
			}
			active = th.cur.Load()
		}
		th.turnMu.Unlock()
		held = false

		if active == nil {
			continue
		}
		waited = true
		select {
		case <-active.finishedCh:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	defer func() {
		th.cur.CompareAndSwap(st, nil)
		st.finish()
		if held {
			th.turnMu.Unlock()
		}
	}()
	if waited {
		// startTurn raised the indicator before waiting for this lock. An
		// observed turn may have lowered it while this request was queued.
		th.b.setStatus(ctx, th, StatusBusy)
	}

	client, err := th.ensure(ctx, m.Text)
	if err != nil {
		return err
	}
	sessionPath := th.snapshot().SessionPath

	// GetLastAssistantText returns the session's last answer, which is not
	// necessarily an answer to *this* prompt, so remember it and refuse to
	// present a stale answer as new.
	before, err := client.GetLastAssistantText(ctx)
	if err != nil {
		th.log.Debug("cannot read the previous answer", "error", err)
	} else {
		th.rememberAssistant(before)
	}

	stopFlush := th.flush(ctx, st, r)
	defer func() {
		_ = st.waitAbortResolution(context.Background())
		st.finishOpenTasks()
		stopFlush()
	}()

	// What the thread has been saying since the last turn rides in front of this
	// request (DESIGN.md §4). The fetch happens here — after the session exists
	// and before the prompt — because the prompt is the only place a transcript
	// can go. The reply remains lazy until the agent produces visible text.
	obs := th.observe(ctx, m)
	prepared, err := th.preparePrompt(ctx, m, obs)
	if err != nil {
		return err
	}
	if len(prepared.warnings) > 0 {
		th.b.reply(ctx, Notice{Thread: &m.Thread, UserID: m.UserID,
			Text: "Some attachments could not be read:\n" + strings.Join(prepared.warnings, "\n")})
	}

	// The instruction that explains the transcript travels once per session:
	// the one we create carries it in its system prompt, and one we adopted gets
	// it in front of the first prompt of the conversation (DESIGN.md §4).
	instruction := th.neededInstruction(ctx)

	// A live agent_start during preparation temporarily replaces this prompt
	// reservation. Yield the lock so its observed reply can render first, then
	// restore this reservation before sending the prompt.
	for {
		st.renderable.Store(true)
		active := th.cur.Load()
		if active == nil {
			if th.cur.CompareAndSwap(nil, st) {
				break
			}
			continue
		}
		if active == st {
			break
		}
		st.renderable.Store(false)
		waited = true
		held = false
		th.turnMu.Unlock()
		select {
		case <-active.finishedCh:
		case <-ctx.Done():
			return ctx.Err()
		}
		th.turnMu.Lock()
		held = true
		if err := ctx.Err(); err != nil {
			return err
		}
		th.b.setStatus(ctx, th, StatusBusy)
		client, err = th.ensure(ctx, m.Text)
		if err != nil {
			return err
		}
		if latest, readErr := client.GetLastAssistantText(ctx); readErr == nil {
			before = latest
			th.rememberAssistant(before)
		}
	}
	prompt := prepared.text
	if instruction != "" {
		prompt = instruction + "\n\n" + prompt
	}
	if _, err := client.Prompt(ctx, trimPrompt(prompt), prepared.images...); err != nil {
		if st.sessionDeleted() {
			th.forgetSessionForPath(ctx, sessionPath)
			return fmt.Errorf("%w: it was deleted outside pi-chat", errSessionGone)
		}
		return fmt.Errorf("prompt: %w", err)
	}
	// The prompt is where those messages went, so this is the first moment the
	// watermark may move past them — and the instruction is owed until a prompt
	// has actually carried it.
	th.rememberObserved(ctx, obs)
	if instruction != "" {
		th.instructionSent()
	}
	if err := st.waitStarted(ctx, startGrace); err != nil {
		return err
	}
	if err := client.AwaitSettled(ctx); err != nil {
		if st.sessionDeleted() {
			th.forgetSessionForPath(ctx, sessionPath)
			return fmt.Errorf("%w: it was deleted outside pi-chat", errSessionGone)
		}
		return fmt.Errorf("wait for the turn: %w", err)
	}
	// A concurrent `/abort` can race with settlement. Wait for its gateway
	// outcome before finalizing the Slack task or stopping the renderer.
	if err := st.waitAbortResolution(context.Background()); err != nil {
		return fmt.Errorf("wait for an in-flight abort: %w", err)
	}

	// The turn is over, wherever it ended: remember how far this connection
	// got, so reopening the thread resumes here instead of replaying it. A
	// deleted session has nothing left to resume, so it is handled first.
	if st.sessionDeleted() {
		th.forgetSessionForPath(ctx, sessionPath)
		return fmt.Errorf("%w: it was deleted outside pi-chat, so the next message starts a new one", errSessionGone)
	}
	th.saveCursorAfterActivity(ctx, client)

	final, err := client.GetLastAssistantText(ctx)
	if err != nil {
		th.log.Warn("cannot read the answer", "error", err)
	}
	// GetLastAssistantText returns just the last assistant message. Preserve
	// earlier visible messages and fall back to streamed text if reading back
	// failed, while letting the authoritative final text correct the last one.
	final = st.authoritativeText(final)
	if final != "" {
		th.rememberAssistant(final)
	}
	if final == "" || (final == before && !st.sawWork()) {
		return errors.New("the agent produced no answer")
	}

	st.finishOpenTasks()
	stopFlush()
	if err := r.Finish(ctx, final); err != nil {
		return fmt.Errorf("finish the reply: %w", err)
	}
	th.noteProgress(ctx, r)
	th.touch()
	th.b.completeRequest(ctx, m)
	return nil
}

// renderObservedTurn renders a turn the session started without a Slack
// prompt. It waits for visible text or task progress before creating a Slack
// reply, so synthetic wakes never become empty Slack replies.
func (th *thread) renderObservedTurn(st *turnState, client *gwclient.Client) {
	th.turnMu.Lock()
	defer th.finishTurn(st)
	sessionPath := th.snapshot().SessionPath
	defer func() {
		if st.sessionDeleted() {
			th.forgetSessionForPath(context.Background(), sessionPath)
		}
	}()

	ctx := th.b.lifetime
	if ctx == nil || ctx.Err() != nil {
		return
	}
	th.touch()
	th.b.setStatus(ctx, th, StatusBusy)
	defer th.b.setStatus(context.WithoutCancel(ctx), th, StatusIdle)

	if client == nil {
		client = th.live()
	}
	before, haveBefore := th.assistantBaseline()
	if client != nil && !haveBefore {
		text, err := client.GetLastAssistantText(ctx)
		haveBefore = err == nil
		if err != nil {
			th.log.Debug("cannot read the previous answer before an asynchronous turn", "error", err)
		} else {
			before = sha256.Sum256([]byte(text))
			th.rememberAssistant(text)
		}
	}
	var disconnected <-chan struct{}
	setClient := func(c *gwclient.Client) {
		client = c
		if c == nil {
			disconnected = nil
			return
		}
		disconnected = c.Done()
	}
	setClient(client)
	reconnect := func() error {
		c, err := th.reconnectObservedTurn(ctx, st, before, haveBefore)
		if err == nil {
			setClient(c)
		}
		return err
	}
	reconcile := func() {
		if client == nil || st.sessionDeleted() || th.cur.Load() != st {
			return
		}
		final, err := client.GetLastAssistantText(ctx)
		if err != nil {
			th.log.Debug("cannot reconcile the completed asynchronous answer", "error", err)
			return
		}
		if reconcileObservedText(st, final, before, haveBefore) {
			th.rememberAssistant(final)
		}
	}
	isDone := func() bool {
		select {
		case <-st.doneCh:
			return true
		default:
			return false
		}
	}

	// A native completion can wake the parent without it producing a reply.
	// Don't create an empty placeholder unless task progress is available.
	for st.text() == "" && !st.hasPendingTaskUpdates() {
		select {
		case <-st.textCh:
		case <-st.taskCh:
		case <-st.doneCh:
			if st.text() == "" && !st.hasPendingTaskUpdates() {
				reconcile()
				if st.text() == "" && !st.hasPendingTaskUpdates() {
					if client != nil && !st.sessionDeleted() {
						th.saveCursorAfterActivity(ctx, client)
					}
					return
				}
			}
		case <-disconnected:
			if err := reconnect(); err != nil {
				if isDone() {
					reconcile()
					if st.text() != "" || st.hasPendingTaskUpdates() {
						continue
					}
				}
				th.log.Warn("cannot reconnect before an asynchronous reply", "error", err)
				return
			}
		case <-ctx.Done():
			return
		}
	}

	m := Message{Thread: th.t, Workspace: th.t.Workspace}
	r, err := th.b.plat.StartTurn(ctx, m)
	if err != nil {
		th.log.Error("cannot start the asynchronous reply", "error", err)
		return
	}
	stopFlush := th.flush(ctx, st, r)
	defer func() {
		_ = st.waitAbortResolution(context.Background())
		st.finishOpenTasks()
		stopFlush()
	}()
	if st.hasPendingTaskUpdates() {
		st.signalTaskUpdate()
	}

	var waitErr error
waitForCompletion:
	for !isDone() {
		select {
		case <-st.doneCh:
		case <-disconnected:
			if err := reconnect(); err != nil {
				if !isDone() {
					waitErr = err
				}
				break waitForCompletion
			}
		case <-ctx.Done():
			if !isDone() {
				waitErr = ctx.Err()
			}
			break waitForCompletion
		}
	}
	if st.sessionDeleted() && waitErr == nil {
		waitErr = fmt.Errorf("%w: it was deleted outside pi-chat, so the next message starts a new one", errSessionGone)
	}
	_ = st.waitAbortResolution(context.Background())
	st.finishOpenTasks()
	stopFlush()
	if waitErr == nil {
		reconcile()
	}

	terminalCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 15*time.Second)
	defer cancel()
	if waitErr != nil {
		if err := r.Fail(terminalCtx, waitErr); err != nil {
			th.log.Error("cannot report the asynchronous reply failure", "error", err)
		}
	} else {
		final := st.text()
		if final != "" {
			th.rememberAssistant(final)
		}
		if err := r.Finish(terminalCtx, final); err != nil {
			th.log.Error("cannot finish the asynchronous reply", "error", err)
		}
	}
	if waitErr == nil && client != nil && !st.sessionDeleted() {
		th.saveCursorAfterActivity(ctx, client)
	}
	th.touch()
}

// reconnectObservedTurn reattaches after a connection drop while an observed
// turn is still reserved. GetState restores the gateway's running/settled bit;
// if the turn finished in the gap, the last assistant text recovers its answer.
func (th *thread) reconnectObservedTurn(ctx context.Context, st *turnState, before [32]byte, haveBefore bool) (*gwclient.Client, error) {
	backoff := 250 * time.Millisecond
	firstAttempt := true
	for {
		row := th.snapshot()
		if row.SessionPath == "" || st.sessionDeleted() {
			return nil, fmt.Errorf("%w: the thread no longer owns the observed session", errSessionGone)
		}
		client, err := th.ensure(ctx, "")
		if err == nil {
			_, err = client.GetState(ctx)
		}
		if err == nil && th.cur.Load() != st {
			return client, nil
		}
		if err == nil && !client.TurnRunning() {
			final, readErr := client.GetLastAssistantText(ctx)
			if readErr != nil {
				err = fmt.Errorf("read the asynchronous answer after reconnect: %w", readErr)
			} else {
				if th.cur.Load() != st {
					return client, nil
				}
				reconciled := reconcileObservedText(st, final, before, haveBefore)
				if !reconciled {
					page, entriesErr := client.GetEntries(ctx, row.LeafID)
					if entriesErr != nil {
						th.log.Debug("cannot inspect assistant entries after reconnect", "error", entriesErr)
					} else if row.LeafID != "" {
						reconciled = reconcileObservedEntries(
							st, final, row.LeafID, page.LeafID, page.Entries)
					} else if recovered, ok := latestAssistantTextAfter(
						page.Entries, row.LastActive.Add(time.Second)); ok {
						if final == "" {
							final = recovered
						}
						st.replace(final)
						reconciled = true
					}
				}
				if reconciled {
					if final == "" {
						final = st.text()
					}
					th.rememberAssistant(final)
				}
				st.markDone()
				return client, nil
			}
		}
		if err == nil {
			return client, nil
		}
		if errors.Is(err, errSessionGone) || ctx.Err() != nil {
			return nil, err
		}
		if firstAttempt {
			th.log.Warn("gateway disconnected during an asynchronous turn; reconnecting", "error", err)
			firstAttempt = false
		} else {
			th.log.Debug("asynchronous turn reconnect is still failing", "error", err)
		}
		timer := time.NewTimer(backoff)
		select {
		case <-timer.C:
		case <-ctx.Done():
			timer.Stop()
			return nil, ctx.Err()
		}
		backoff = min(backoff*2, 5*time.Second)
	}
}

// reconcileObservedText replaces missing or partial streamed deltas with the
// authoritative final answer, while leaving textless wakes empty.
func reconcileObservedText(st *turnState, final string, before [32]byte, haveBefore bool) bool {
	if final == "" {
		return false
	}
	current := st.text()
	if current == "" && (!haveBefore || sha256.Sum256([]byte(final)) == before) {
		return false
	}
	st.replace(final)
	return true
}

// turnState accumulates what the gateway reports about one turn.
//
// It is written by the connection's OnEvent callback, which runs on the read
// goroutine and therefore must not block or call back into the gateway
// (DESIGN.md §4). It performs in-memory parsing and bookkeeping, buffer appends,
// and non-blocking channel signals; the rendering side reads from other goroutines.
type turnState struct {
	cancel                context.CancelFunc
	mu                    sync.Mutex
	pending               strings.Builder // text not yet rendered
	pendingTasks          []TaskProgress  // task updates not yet rendered
	activeTasks           map[string]TaskProgress
	seenToolCalls         map[string]struct{}
	seenToolCallIDs       []string
	lastEventSeq          uint64
	progressStartedSent   bool
	terminal              bool
	eventDone             bool // terminal event observed, independent of renderer release
	settled               bool
	progressFinalized     bool
	pendingTerminalStatus TaskProgressStatus
	abortPending          int
	donePending           bool
	aborted               bool

	progressActionTitles []string
	progressActionCount  int

	progressRemainder       string
	progressCapture         strings.Builder
	capturingProgress       bool
	discardingProgressClose bool
	pendingProgress         []string

	messages       []string        // completed assistant messages with visible text
	currentMessage strings.Builder // visible text in the current assistant message
	shown          strings.Builder // text already handed to a renderer
	work           bool
	// promptReservation distinguishes a queued Slack prompt from an unsolicited
	// turn so an early agent_start can be reserved and rendered separately.
	promptReservation bool

	// firstTextCh wakes the renderer as soon as user-visible text arrives, so
	// it can create a reply without showing an empty message during thinking.
	firstTextCh   chan struct{}
	firstTextOnce sync.Once

	// renderable marks the point where this reservation may accept session
	// output. A live agent_start can claim it during prompt preparation; other
	// pre-start frames are not safely attributable to this reply.
	renderable atomic.Bool
	// deleted records that the session was deleted under this turn.
	deleted atomic.Bool

	workOnce        sync.Once
	doneOnce        sync.Once
	textOnce        sync.Once
	finishOnce      sync.Once
	workCh          chan struct{} // closed on the first sign of a running turn
	doneCh          chan struct{} // renderer wait; may close after abort resolution
	textCh          chan struct{} // closed on the first assistant text delta
	finishedCh      chan struct{} // closed after this turn's renderer is finished
	taskCh          chan struct{} // wakes the renderer when a task update arrives
	abortResolvedCh chan struct{} // wakes turn cleanup after abort RPCs resolve
}

func newTurnState() *turnState {
	return &turnState{
		activeTasks:     make(map[string]TaskProgress),
		seenToolCalls:   make(map[string]struct{}),
		firstTextCh:     make(chan struct{}),
		workCh:          make(chan struct{}),
		doneCh:          make(chan struct{}),
		textCh:          make(chan struct{}),
		finishedCh:      make(chan struct{}),
		taskCh:          make(chan struct{}, 1),
		abortResolvedCh: make(chan struct{}, 1),
	}
}

// feed records one gateway event. Unknown event types are ignored on purpose:
// the gateway forwards everything pi emits, and this build renders text.
func (s *turnState) hasSeenEvent(seq uint64) bool {
	if seq == 0 {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return seq <= s.lastEventSeq
}

func (s *turnState) feed(ev gwclient.Event, log *slog.Logger) {
	if ev.Type == "gw_snapshot" {
		if snapshot, err := ev.Snapshot(); err == nil {
			s.mu.Lock()
			s.lastEventSeq = snapshot.HeadSeq
			s.mu.Unlock()
		}
		return
	}
	// Resume replay carries the original sequence; never apply a frame twice.
	if ev.Seq != 0 {
		s.mu.Lock()
		if ev.Seq <= s.lastEventSeq {
			s.mu.Unlock()
			return
		}
		s.lastEventSeq = ev.Seq
		s.mu.Unlock()
	}

	switch ev.Type {
	case "message_update":
		// Streaming updates are delta-only on the wire: the assistant's text
		// arrives as text_delta fragments, and thinking and tool-call
		// arguments use the same shape with a different type (pi docs/json.md).
		var update struct {
			AssistantMessageEvent struct {
				Type  string `json:"type"`
				Delta string `json:"delta"`
			} `json:"assistantMessageEvent"`
		}
		if err := ev.Unmarshal(&update); err == nil && update.AssistantMessageEvent.Type == "text_delta" {
			s.appendAssistantDelta(update.AssistantMessageEvent.Delta)
		}
		s.markWork()

	case "message_start":
		s.startMessage()
		s.markWork()

	case "turn_start", "agent_start", "tool_execution_update":
		s.markWork()

	case "tool_execution_start":
		s.recordToolTask(ev)
		s.markWork()

	case "tool_execution_end":
		s.markWork()

	case "agent_settled":
		// The only reliable end of work. message_end ends one message and
		// turn_end ends one turn of the agent loop: a single prompt can produce
		// several of each (a tool call, then the answer), so treating either as
		// the end would stop rendering while text was still to come. The
		// documented signal is agent_settled, or gw_turn going settled.
		s.markDone()

	case "gw_turn":
		turn, err := ev.Turn()
		if err != nil {
			return
		}
		if turn.State == "running" {
			s.markWork()
		} else {
			s.markDone()
		}

	case "gw_session_state":
		state, err := ev.SessionState()
		if err == nil && state.State == protocol.SessionStateDeleted {
			// The session file is gone: the thread has to stop pointing at it
			// (DESIGN.md §4, invariant 3). The flag is read by the turn path,
			// because the read goroutine must not write to the database.
			s.deleted.Store(true)
			s.finishProgressTask(TaskError)
			s.markDone()
		}

	case "extension_ui_request":
		// Two kinds of frame share this type, and only one of them needs
		// answering (DESIGN.md §6).
		request, ok := ev.UIRequest()
		if !ok {
			log.Warn("pi sent an extension UI request that could not be decoded",
				"event", string(ev.Raw))
			return
		}
		if !request.Blocking {
			// Fire-and-forget: never answered, and not silenced by `--approve`,
			// so pi-lens and friends emit these on every turn. This build has
			// nowhere to show them; phase 3 maps setStatus/setTitle onto Slack.
			if request.Method == gwclient.UIMethodNotify && request.NotifyTypeOrInfo() != "info" {
				log.Warn("pi raised a notification",
					"type", request.NotifyTypeOrInfo(), "text", stripANSI(request.Text))
				return
			}
			log.Debug("extension notification ignored",
				"method", request.Method, "status_key", request.StatusKey)
			return
		}
		// A dialog blocks pi until somebody answers it. With approvals on auto
		// pi resolves what it can itself, so reaching here means the thread
		// stalls until pi's own timeout. Phase 2 answers these with Block Kit.
		log.Warn("pi is waiting for an answer this build cannot give; the turn stalls until pi times out",
			"method", request.Method, "id", request.ID,
			"title", request.Title, "message", stripANSI(request.Message))

	case "extension_error":
		log.Warn("pi reported an extension error", "event", string(ev.Raw))
	}
}

func (s *turnState) recordToolTask(ev gwclient.Event) {
	var tool struct {
		ToolCallID string `json:"toolCallId"`
		ToolName   string `json:"toolName"`
	}
	if err := ev.Unmarshal(&tool); err != nil {
		return
	}
	if len(tool.ToolCallID) > maxToolCallIDBytes || len(tool.ToolName) > maxToolNameBytes {
		return
	}
	callID := strings.TrimSpace(tool.ToolCallID)
	name := strings.TrimSpace(stripANSI(tool.ToolName))
	if callID == "" || name == "" {
		return
	}

	s.mu.Lock()
	if s.terminal {
		s.mu.Unlock()
		return
	}
	if _, duplicate := s.seenToolCalls[callID]; duplicate {
		s.mu.Unlock()
		return
	}
	if len(s.seenToolCallIDs) == maxSeenToolCallIDs {
		delete(s.seenToolCalls, s.seenToolCallIDs[0])
		copy(s.seenToolCallIDs, s.seenToolCallIDs[1:])
		s.seenToolCallIDs = s.seenToolCallIDs[:len(s.seenToolCallIDs)-1]
	}
	s.seenToolCalls[callID] = struct{}{}
	s.seenToolCallIDs = append(s.seenToolCallIDs, callID)
	title := safeToolTitle(name)
	if len(s.pendingProgress) > 0 {
		title = s.pendingProgress[0]
		copy(s.pendingProgress, s.pendingProgress[1:])
		s.pendingProgress[len(s.pendingProgress)-1] = ""
		s.pendingProgress = s.pendingProgress[:len(s.pendingProgress)-1]
		if title == "" {
			title = safeToolTitle(name)
		}
	}
	if containsCredentialLikeText(title) {
		title = "Using a tool"
	}
	s.rememberProgressTitleLocked(title)
	update := TaskProgress{
		ID:      turnProgressTaskID,
		Title:   title,
		Status:  TaskInProgress,
		Details: progressDetails(s.progressActionTitles, s.progressActionCount, false),
	}
	s.activeTasks[turnProgressTaskID] = update
	s.queueProgressUpdateLocked(update)
	s.mu.Unlock()
	s.signalTaskUpdate()
}

// rememberProgressTitleLocked keeps at most six unique safe titles for expanded
// details; the terminal title uses only the first two. Callers hold s.mu.
func (s *turnState) rememberProgressTitleLocked(title string) {
	s.progressActionCount++
	if len(s.progressActionTitles) < maxProgressDetailTitles && !slices.Contains(s.progressActionTitles, title) {
		s.progressActionTitles = append(s.progressActionTitles, title)
	}
}

// queueProgressUpdateLocked preserves the first unsent in-progress update and
// coalesces later title changes, while retaining a terminal update after it.
// Callers hold s.mu; the queue is bounded to two items.
func (s *turnState) queueProgressUpdateLocked(update TaskProgress) {
	if update.Status == TaskInProgress {
		if !s.progressStartedSent && len(s.pendingTasks) > 0 && s.pendingTasks[0].Status == TaskInProgress {
			if len(s.pendingTasks) == 1 {
				if s.pendingTasks[0].Title != update.Title {
					s.pendingTasks = append(s.pendingTasks, update)
				}
			} else {
				s.pendingTasks[1] = update
			}
			return
		}
		s.pendingTasks = []TaskProgress{update}
		return
	}

	if !s.progressStartedSent && len(s.pendingTasks) > 0 && s.pendingTasks[0].Status == TaskInProgress {
		if len(s.pendingTasks) == 1 {
			s.pendingTasks = append(s.pendingTasks, update)
		} else {
			s.pendingTasks[1] = update
		}
		return
	}
	s.pendingTasks = []TaskProgress{update}
}

// finishProgressTask closes the single logical progress task for this reply.
// Tool activity only changes its title; it is complete or failed with the turn.
func (s *turnState) finishProgressTask(status TaskProgressStatus) {
	s.mu.Lock()
	if s.progressFinalized {
		s.mu.Unlock()
		return
	}
	s.terminal = true
	if !s.settled {
		s.settled = true
		s.pendingTerminalStatus = status
	} else if status == TaskError {
		s.pendingTerminalStatus = TaskError
	}
	updated := s.finalizeProgressLocked()
	s.mu.Unlock()
	if updated {
		s.signalTaskUpdate()
	}
}

// finalizeProgressLocked publishes the saved terminal state once outstanding
// abort requests have resolved. Callers hold s.mu.
func (s *turnState) finalizeProgressLocked() bool {
	if !s.settled || s.abortPending != 0 || s.progressFinalized {
		return false
	}
	status := s.pendingTerminalStatus
	if status == "" {
		status = TaskComplete
	}
	if status == TaskComplete && s.aborted {
		status = TaskError
	}
	s.progressFinalized = true
	update, exists := s.activeTasks[turnProgressTaskID]
	if exists {
		update.Status = status
		if status == TaskComplete {
			update.Title = summarizeProgressTitles(s.progressActionTitles, s.progressActionCount)
			update.Details = progressDetails(s.progressActionTitles, s.progressActionCount, true)
		}
		if status == TaskError {
			update.Details = "The turn ended before it completed."
		}
		delete(s.activeTasks, turnProgressTaskID)
		s.queueProgressUpdateLocked(update)
	}
	return exists
}

// beginAbort prevents a concurrent settle event from publishing completion
// before the gateway confirms whether the abort took effect.
func (s *turnState) beginAbort() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.progressFinalized {
		return false
	}
	s.abortPending++
	return true
}

func (s *turnState) resolveAbort(tracked, stopped bool) {
	if !tracked {
		return
	}
	s.mu.Lock()
	if s.abortPending > 0 {
		s.abortPending--
	}
	if stopped {
		s.aborted = true
	}
	updated := s.finalizeProgressLocked()
	closeDone := s.donePending && s.abortPending == 0
	if closeDone {
		s.donePending = false
	}
	s.mu.Unlock()
	if updated {
		s.signalTaskUpdate()
	}
	if closeDone {
		s.closeDone()
	}
	select {
	case s.abortResolvedCh <- struct{}{}:
	default:
	}
}

func (s *turnState) waitAbortResolution(ctx context.Context) error {
	for {
		s.mu.Lock()
		pending := s.abortPending > 0
		s.mu.Unlock()
		if !pending {
			return nil
		}
		select {
		case <-s.abortResolvedCh:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

func safeToolTitle(toolName string) string {
	title := trimProgressTitle(taskTitle(toolName))
	if title == "" {
		return "Using a tool"
	}
	return title
}

func summarizeProgressTitles(titles []string, total int) string {
	if len(titles) == 0 || total <= 0 {
		return "Completed"
	}
	shown := min(len(titles), maxProgressSummaryTitles)
	if total < shown {
		total = shown
	}
	title := "Completed: " + strings.Join(titles[:shown], ", ")
	if total > shown {
		title += fmt.Sprintf(" (+%d more)", total-shown)
	}
	if safe := trimProgressTitle(title); safe != "" {
		return safe
	}
	return "Completed"
}

func progressDetails(titles []string, total int, completed bool) string {
	if len(titles) == 0 || total <= 0 {
		return ""
	}
	heading := "Progress so far:"
	if completed {
		heading = "Completed actions:"
	}
	lines := []string{heading}
	for _, title := range titles {
		lines = append(lines, "• "+title)
	}
	if total > len(titles) {
		lines = append(lines, fmt.Sprintf("• %d additional action(s) not listed", total-len(titles)))
	}
	details := strings.Join(lines, "\n")
	runes := []rune(details)
	if len(runes) > maxProgressDetailsRunes {
		return string(runes[:maxProgressDetailsRunes-1]) + "…"
	}
	return details
}

func taskTitle(toolName string) string {
	switch strings.ToLower(toolName) {
	case "read":
		return "Reading files"
	case "write", "edit":
		return "Updating files"
	case "bash":
		return "Running a command"
	case "grep", "find":
		return "Searching files"
	case "ls":
		return "Listing files"
	default:
		name := strings.Join(strings.Fields(toolName), " ")
		if name == "" {
			return "Using a tool"
		}
		title := "Using " + name
		if runes := []rune(title); len(runes) > maxProgressTitleRunes {
			title = string(runes[:maxProgressTitleRunes-1]) + "…"
		}
		return title
	}
}

func (s *turnState) appendAssistantDelta(delta string) {
	if visible := s.consumeProgressDelta(delta); visible != "" {
		s.append(visible)
	}
}

// consumeProgressDelta removes the explicit public-status marker from reply
// text and queues only a bounded short title for the next tool start. Thinking
// deltas never reach this parser.
func (s *turnState) consumeProgressDelta(delta string) string {
	s.mu.Lock()
	defer s.mu.Unlock()

	input := s.progressRemainder + delta
	s.progressRemainder = ""
	var visible strings.Builder
	for input != "" {
		if s.discardingProgressClose {
			end := strings.Index(input, progressMarkerClose)
			if end < 0 {
				keep := progressMarkerPrefixSuffixLen(input, progressMarkerClose)
				if keep > 0 {
					s.progressRemainder = input[len(input)-keep:]
				}
				break
			}
			input = input[end+len(progressMarkerClose):]
			s.discardingProgressClose = false
			continue
		}

		if s.capturingProgress {
			end := strings.Index(input, progressMarkerClose)
			if end < 0 {
				keep := progressMarkerPrefixSuffixLen(input, progressMarkerClose)
				payload := input[:len(input)-keep]
				captured := s.progressCapture.String()
				if len([]rune(captured))+len([]rune(payload)) > maxProgressTitleRunes {
					// An overlong status marker is invalid metadata. Drop its content
					// through the closing delimiter without retaining it.
					if keep > 0 {
						s.progressRemainder = input[len(input)-keep:]
					}
					s.progressCapture.Reset()
					s.capturingProgress = false
					s.discardingProgressClose = true
					break
				}
				s.progressCapture.WriteString(payload)
				if keep > 0 {
					s.progressRemainder = input[len(input)-keep:]
				}
				break
			}

			content := s.progressCapture.String() + input[:end]
			if len([]rune(content)) <= maxProgressTitleRunes && len(s.pendingProgress) < maxPendingProgressTitles {
				s.pendingProgress = append(s.pendingProgress, trimProgressTitle(content))
			}
			s.progressCapture.Reset()
			s.capturingProgress = false
			input = input[end+len(progressMarkerClose):]
			continue
		}

		start := strings.Index(input, progressMarkerOpen)
		if start >= 0 {
			visible.WriteString(input[:start])
			input = input[start+len(progressMarkerOpen):]
			s.capturingProgress = true
			continue
		}

		keep := progressMarkerPrefixSuffixLen(input, progressMarkerOpen)
		visible.WriteString(input[:len(input)-keep])
		if keep > 0 {
			s.progressRemainder = input[len(input)-keep:]
		}
		break
	}
	return visible.String()
}

func progressMarkerPrefixSuffixLen(text, marker string) int {
	max := min(len(text), len(marker)-1)
	for n := max; n > 0; n-- {
		if strings.HasSuffix(text, marker[:n]) {
			return n
		}
	}
	return 0
}

func trimProgressTitle(text string) string {
	title := strings.Join(strings.Fields(text), " ")
	if title == "" || containsCredentialLikeText(title) || strings.ContainsAny(title, "`$|;&=/\\{}[]") || strings.Contains(title, "--") {
		return ""
	}
	if runes := []rune(title); len(runes) > maxProgressTitleRunes {
		return string(runes[:maxProgressTitleRunes-1]) + "…"
	}
	return title
}

func containsCredentialLikeText(text string) bool {
	lower := strings.ToLower(text)
	for _, cue := range []string{
		"password", "passwd", "secret", "token", "credential", "authorization", "bearer",
		"api key", "api_key", "api-key", "access key", "access_key", "access-key",
		"private key", "private_key",
	} {
		if strings.Contains(lower, cue) {
			return true
		}
	}

	for field := range strings.FieldsSeq(text) {
		field = strings.Trim(field, `"'(),[]{}<>;`)
		runes := []rune(field)
		digits := false
		for _, r := range runes {
			digits = digits || unicode.IsDigit(r)
		}
		// Long opaque tokens are much more likely to be keys or IDs than useful
		// action titles. Fall back instead of trying to redact them heuristically.
		if len(runes) >= 16 || (len(runes) >= 12 && digits) {
			return true
		}
	}
	return false
}

func (s *turnState) flushProgressMarker() {
	s.mu.Lock()
	tail := s.progressRemainder
	if s.capturingProgress {
		content := s.progressCapture.String() + tail
		if len([]rune(content)) <= maxProgressTitleRunes && !containsCredentialLikeText(content) {
			tail = progressMarkerOpen + content
		} else {
			tail = ""
		}
	} else if s.discardingProgressClose {
		tail = ""
	}
	s.progressRemainder = ""
	s.progressCapture.Reset()
	s.capturingProgress = false
	s.discardingProgressClose = false
	s.mu.Unlock()
	if tail != "" {
		s.append(tail)
	}
}

func stripProgressMarkers(text string) string {
	var visible strings.Builder
	for text != "" {
		start := strings.Index(text, progressMarkerOpen)
		if start < 0 {
			visible.WriteString(text)
			break
		}
		visible.WriteString(text[:start])
		contentStart := start + len(progressMarkerOpen)
		end := strings.Index(text[contentStart:], progressMarkerClose)
		if end < 0 {
			content := text[contentStart:]
			if len([]rune(content)) <= maxProgressTitleRunes && !containsCredentialLikeText(content) {
				visible.WriteString(text[start:])
			}
			break
		}
		suffixStart := contentStart + end + len(progressMarkerClose)
		text = text[suffixStart:]
	}
	return visible.String()
}

func (s *turnState) finishOpenTasks() {
	s.mu.Lock()
	if s.settled || s.progressFinalized {
		s.mu.Unlock()
		return
	}
	s.terminal = true
	s.settled = true
	s.pendingTerminalStatus = TaskError
	updated := s.finalizeProgressLocked()
	s.mu.Unlock()
	if updated {
		s.signalTaskUpdate()
	}
}

func (s *turnState) signalTaskUpdate() {
	select {
	case s.taskCh <- struct{}{}:
	default:
	}
}

func (s *turnState) takeTaskUpdates() []TaskProgress {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.pendingTasks) == 0 {
		return nil
	}
	updates := append([]TaskProgress(nil), s.pendingTasks...)
	for _, update := range updates {
		if update.Status == TaskInProgress {
			s.progressStartedSent = true
		}
	}
	s.pendingTasks = nil
	return updates
}

func (s *turnState) startMessage() {
	s.flushProgressMarker()
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.currentMessage.Len() > 0 {
		s.messages = append(s.messages, s.currentMessage.String())
		s.currentMessage.Reset()
	}
}

func (s *turnState) append(delta string) {
	if delta == "" {
		return
	}
	s.mu.Lock()
	if s.currentMessage.Len() == 0 && len(s.messages) > 0 {
		s.pending.WriteString(assistantMessageSeparator)
	}
	s.pending.WriteString(delta)
	s.currentMessage.WriteString(delta)
	s.mu.Unlock()
	s.firstTextOnce.Do(func() { close(s.firstTextCh) })
	s.textOnce.Do(func() { close(s.textCh) })
}

func (s *turnState) replace(text string) {
	text = stripProgressMarkers(text)
	s.mu.Lock()
	s.progressRemainder = ""
	s.progressCapture.Reset()
	s.capturingProgress = false
	s.discardingProgressClose = false
	s.pendingProgress = nil
	if text == "" {
		s.mu.Unlock()
		return
	}
	combined := text
	if len(s.messages) > 0 {
		combined = strings.Join(s.messages, assistantMessageSeparator) + assistantMessageSeparator + text
	}
	s.pending.Reset()
	// Delta is append-only: send only an unseen suffix. A divergent final is
	// corrected by Renderer.Finish instead of duplicating already-shown text.
	shown := s.shown.String()
	if strings.HasPrefix(combined, shown) {
		s.pending.WriteString(combined[len(shown):])
	}
	s.currentMessage.Reset()
	s.currentMessage.WriteString(text)
	s.mu.Unlock()
	s.firstTextOnce.Do(func() { close(s.firstTextCh) })
	s.textOnce.Do(func() { close(s.textCh) })
}

func (s *turnState) markWork() {
	s.mu.Lock()
	s.work = true
	s.mu.Unlock()
	s.workOnce.Do(func() { close(s.workCh) })
}

func (s *turnState) markDone() {
	s.mu.Lock()
	s.eventDone = true
	s.mu.Unlock()
	s.finishProgressTask(TaskComplete)
	s.flushProgressMarker()
	s.mu.Lock()
	s.pendingProgress = nil
	clear(s.seenToolCalls)
	s.seenToolCallIDs = nil
	s.terminal = true
	closeDone := s.abortPending == 0
	if !closeDone {
		s.donePending = true
	}
	s.mu.Unlock()
	if closeDone {
		s.closeDone()
	}
}

func (s *turnState) closeDone() {
	s.doneOnce.Do(func() { close(s.doneCh) })
}

func (s *turnState) finish() {
	s.finishOnce.Do(func() {
		s.mu.Lock()
		s.terminal = true
		clear(s.seenToolCalls)
		s.seenToolCallIDs = nil
		s.pendingProgress = nil
		s.progressActionTitles = nil
		s.mu.Unlock()
		close(s.finishedCh)
	})
}

// sessionDeleted reports that the session was deleted while this turn ran.
func (s *turnState) sessionDeleted() bool { return s.deleted.Load() }

func newPromptTurnState() *turnState {
	st := newTurnState()
	st.promptReservation = true
	return st
}

func (s *turnState) done() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.eventDone
}

func (s *turnState) sawWork() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.work
}

// text is everything the turn produced, as streamed.
func (s *turnState) text() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.streamedText()
}

// authoritativeText combines earlier streamed assistant messages with the
// gateway's authoritative text for the last one. An empty final falls back to
// the full stream, which is all that is available when reading it back failed.
func (s *turnState) authoritativeText(final string) string {
	s.mu.Lock()
	defer s.mu.Unlock()

	final = stripProgressMarkers(final)
	if final == "" {
		return s.streamedText()
	}
	if len(s.messages) == 0 {
		return final
	}
	return strings.Join(s.messages, assistantMessageSeparator) + assistantMessageSeparator + final
}

// streamedText joins all streamed message text while the mutex is held.
func (s *turnState) streamedText() string {
	prior := strings.Join(s.messages, assistantMessageSeparator)
	current := s.currentMessage.String()
	switch {
	case prior == "":
		return current
	case current == "":
		return prior
	default:
		return prior + assistantMessageSeparator + current
	}
}

// hasPending reports whether there is text waiting for the renderer.
func (s *turnState) hasPending() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.pending.Len() > 0 || len(s.pendingTasks) > 0
}

func (s *turnState) hasPendingTaskUpdates() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.pendingTasks) > 0
}

// takePending returns and clears the text produced since the last call.
func (s *turnState) takePending() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := s.pending.String()
	s.pending.Reset()
	s.shown.WriteString(out)
	return out
}

// waitStarted blocks until the turn shows signs of life, ends, or grace
// expires. Timing out is not an error: the caller decides what an apparently
// silent turn means.
func (s *turnState) waitStarted(ctx context.Context, grace time.Duration) error {
	timer := time.NewTimer(grace)
	defer timer.Stop()
	select {
	case <-s.workCh:
	case <-s.doneCh:
	case <-timer.C:
	case <-ctx.Done():
		return ctx.Err()
	}
	return nil
}

// stripANSI removes ANSI escape sequences, which pi extensions put in status
// and notification text: observed verbatim as `\x1b[38;5;241mLSP Inactive\x1b[39m`.
// Slack renders them literally, and they are noise in a journal.
func stripANSI(s string) string {
	if !strings.ContainsRune(s, 0x1b) {
		return s
	}
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); {
		// CSI: ESC [ , parameters, then a final byte in @..~
		if s[i] == 0x1b && i+1 < len(s) && s[i+1] == '[' {
			j := i + 2
			for j < len(s) && (s[j] < 0x40 || s[j] > 0x7e) {
				j++
			}
			i = min(j+1, len(s))
			continue
		}
		b.WriteByte(s[i])
		i++
	}
	return b.String()
}

// onEvent routes one gateway event to the turn it belongs to. It runs on the
// connection's read goroutine.
func (th *thread) onEvent(ev gwclient.Event) {
	th.onEventWithClient(ev, nil, th.sessionEpoch.Load())
}

// onEventWithClient also hands the connection to an observed-turn renderer so
// it can stop waiting if the gateway connection dies.
func (th *thread) onEventWithClient(ev gwclient.Event, client *gwclient.Client, epoch uint64) {
	th.onSessionEventWithClient("", ev, client, epoch)
}

// onSessionEvent ignores frames from a connection whose session identity is no
// longer the thread's. A late deletion revokes only the old path's binding; it
// must not complete a turn or clear the successor's row.
func (th *thread) onSessionEvent(sessionPath string, ev gwclient.Event) {
	th.onSessionEventWithClient(sessionPath, ev, nil, th.sessionEpoch.Load())
}

// onSessionEventWithClient keeps both the session identity and connection
// generation attached to each callback, while passing the client to any
// observed-turn renderer started from that callback.
func (th *thread) onSessionEventWithClient(sessionPath string, ev gwclient.Event, client *gwclient.Client, epoch uint64) {
	if sessionPath != "" && th.snapshot().SessionPath != sessionPath {
		if ev.Type == "gw_session_state" {
			if state, err := ev.SessionState(); err == nil && state.State == protocol.SessionStateDeleted {
				go th.revokeDeletedSessionBinding(context.Background(), sessionPath)
			}
		}
		th.log.Debug("ignoring an event from a session this thread no longer owns",
			"type", ev.Type, "session", sessionPath)
		return
	}
	if epoch != th.sessionEpoch.Load() || th.sessionChanging.Load() {
		return
	}
	// A compaction is not a turn's output, and its consequence outlives the turn
	// it happened in: it replaces the conversation with a summary, which is where
	// an adopted session's instruction lives. It is applied to the thread rather
	// than to the turn, and it does no I/O — the read goroutine must not block.
	if ev.Type == "compaction_end" && compactionSucceeded(ev) {
		th.instructionDiscarded()
	}
	var cur *turnState
	for {
		cur = th.cur.Load()
		if ev.Type == "agent_start" && cur != nil && cur.hasSeenEvent(ev.Seq) {
			return
		}
		if ev.Type != "agent_start" || (cur != nil && !cur.done() && !(cur.promptReservation && !cur.renderable.Load())) {
			break
		}
		if cur != nil && cur.sessionDeleted() {
			return
		}
		previous := cur
		st := newTurnState()
		st.renderable.Store(true)
		if !th.cur.CompareAndSwap(previous, st) {
			continue
		}
		// The prompt can cross into its submitted phase while this callback is
		// replacing the preparation reservation. If it did, restore that state
		// and let the prompt own this agent_start.
		if previous != nil && !previous.done() && previous.promptReservation && previous.renderable.Load() {
			if th.cur.CompareAndSwap(st, previous) {
				cur = previous
				break
			}
			continue
		}
		if epoch != th.sessionEpoch.Load() || th.sessionChanging.Load() {
			th.cur.CompareAndSwap(st, nil)
			st.finish()
			return
		}
		cur = st
		th.log.Info("the session started a turn without a Slack prompt; capturing its reply")
		go th.renderObservedTurn(st, client)
		break
	}
	if epoch != th.sessionEpoch.Load() || th.sessionChanging.Load() {
		return
	}
	cur = th.cur.Load()
	if ev.Type == "gw_session_state" {
		if state, err := ev.SessionState(); err == nil && state.State == protocol.SessionStateDeleted {
			if cur != nil {
				// Deletion belongs to the thread even before its next prompt. Keep
				// it on the turn for the turn path to persist after the read loop
				// has returned; the gateway callback must not write the store.
				cur.deleted.Store(true)
				cur.finishOpenTasks()
				cur.markDone()
				return
			}
			// An idle connection still owns a durable binding. Revoke it away
			// from the gateway read goroutine, which must never block on I/O.
			go th.forgetSessionForPath(context.Background(), sessionPath)
			return
		}
	}
	if cur == nil {
		// Text and lifecycle events alone do not establish an unsolicited
		// response; agent_start above is the boundary for a live wake.
		th.log.Debug("gateway event outside a turn", "type", ev.Type)
		return
	}
	if !cur.renderable.Load() {
		th.log.Debug("gateway event before the prompt; dropping it", "type", ev.Type)
		return
	}
	th.log.Debug("gateway event", "type", ev.Type)
	cur.feed(ev, th.log)
}

// flush renders accumulated text until the turn ends. It creates the reply on
// the first visible text, coalesces subsequent updates on a ticker, and flushes
// on exit. The returned function is safe to call more than once.
func (th *thread) flush(ctx context.Context, st *turnState, r Renderer) func() {
	loopCtx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})

	go func() {
		defer close(done)
		startAttempted := false
		started := false

		// render sends whatever text has accumulated. It is called as soon as
		// the first text arrives, periodically thereafter, and on the way out.
		// Starting here (rather than when the model starts thinking) keeps an
		// empty reply out of the conversation until there is something to show.
		render := func(exiting bool) {
			if !st.hasPending() {
				return
			}
			renderCtx := ctx
			if exiting || ctx.Err() != nil {
				// loopCtx is already canceled here, and on shutdown so is ctx.
				// The last render gets its own budget instead of being
				// abandoned by the cancellation that triggered it.
				var cancelRender context.CancelFunc
				renderCtx, cancelRender = context.WithTimeout(context.WithoutCancel(ctx), 15*time.Second)
				defer cancelRender()
			}
			if tasks := st.takeTaskUpdates(); len(tasks) > 0 {
				if reporter, ok := r.(TaskProgressReporter); ok {
					if err := reporter.UpdateTasks(renderCtx, tasks); err != nil {
						th.log.Warn("cannot update task progress", "thread", th.key, "tasks", len(tasks), "error", err)
					}
					th.noteProgress(renderCtx, r)
				}
			}
			delta := st.takePending()
			if delta == "" {
				return
			}
			if !started && !startAttempted {
				startAttempted = true
				if err := r.Start(renderCtx); err != nil {
					// Keep the text in the renderer even without a reply handle,
					// so a later Finish or Fail can still present it.
					th.log.Warn("cannot start the reply", "error", err)
				} else {
					started = true
					th.noteProgress(renderCtx, r)
				}
			}
			if err := r.Delta(renderCtx, delta); err != nil {
				th.log.Warn("cannot update the reply", "error", err)
			}
		}

		ticker := time.NewTicker(time.Duration(th.b.cfg.Render.FlushMS) * time.Millisecond)
		firstTextCh := st.firstTextCh
		defer ticker.Stop()
		for {
			select {
			case <-loopCtx.Done():
				render(true)
				return
			case <-st.doneCh:
				render(true)
				return
			case <-firstTextCh:
				firstTextCh = nil
				render(false)
			case <-st.taskCh:
				render(false)
			case <-ticker.C:
				render(false)
			}
		}
	}()

	var once sync.Once
	return func() {
		once.Do(func() {
			cancel()
			<-done
		})
	}
}
