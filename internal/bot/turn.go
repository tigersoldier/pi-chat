package bot

import (
	"context"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/tigersoldier/pi-gateway/gwclient"
	"github.com/tigersoldier/pi-gateway/protocol"
)

// startGrace bounds how long a turn waits for evidence that the agent actually
// started. pi publishes agent_start before doing any work, so this is only
// reached when a prompt is accepted and then produces nothing.
const startGrace = 15 * time.Second

// turnState accumulates what the gateway reports about one turn.
//
// It is written by the connection's OnEvent callback, which runs on the read
// goroutine and therefore must not block or call back into the gateway
// (DESIGN.md §4). Everything it does is a buffer append or a channel close; the
// rendering side reads the buffers from other goroutines.
type turnState struct {
	mu      sync.Mutex
	pending strings.Builder // text not yet rendered
	all     strings.Builder // everything produced so far
	work    bool

	workOnce sync.Once
	doneOnce sync.Once
	workCh   chan struct{} // closed on the first sign of a running turn
	doneCh   chan struct{} // closed on a terminal event
}

func newTurnState() *turnState {
	return &turnState{workCh: make(chan struct{}), doneCh: make(chan struct{})}
}

// feed records one gateway event. Unknown event types are ignored on purpose:
// the gateway forwards everything pi emits, and phase 0 only renders text.
func (s *turnState) feed(ev gwclient.Event, log *slog.Logger) {
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
			s.append(update.AssistantMessageEvent.Delta)
		}
		s.markWork()

	case "message_start", "turn_start", "agent_start", "tool_execution_start", "tool_execution_update":
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
			s.markDone()
		}

	case "extension_ui_request":
		// Phase 0 has approvals on auto and no modal support, so an
		// interactive request has nobody to answer it. Say so loudly rather
		// than stalling silently; M5 routes these to Block Kit.
		log.Warn("pi asked for UI input, which phase 0 cannot answer",
			"event", string(ev.Raw))

	case "extension_error":
		log.Warn("pi reported an extension error", "event", string(ev.Raw))
	}
}

func (s *turnState) append(delta string) {
	if delta == "" {
		return
	}
	s.mu.Lock()
	s.pending.WriteString(delta)
	s.all.WriteString(delta)
	s.mu.Unlock()
}

func (s *turnState) markWork() {
	s.mu.Lock()
	s.work = true
	s.mu.Unlock()
	s.workOnce.Do(func() { close(s.workCh) })
}

func (s *turnState) markDone() {
	s.doneOnce.Do(func() { close(s.doneCh) })
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
	return s.all.String()
}

// takePending returns and clears the text produced since the last call.
func (s *turnState) takePending() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := s.pending.String()
	s.pending.Reset()
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

// onEvent routes one gateway event to the turn it belongs to. It runs on the
// connection's read goroutine.
func (th *thread) onEvent(ev gwclient.Event) {
	cur := th.cur.Load()
	if cur == nil {
		// Nothing is being rendered: between turns, or a turn that already
		// finished.
		th.log.Debug("gateway event outside a turn", "type", ev.Type)
		return
	}
	th.log.Debug("gateway event", "type", ev.Type)
	cur.feed(ev, th.log)
}

// flush renders accumulated text on a ticker until the turn ends. The returned
// function stops it and is safe to call more than once.
func (th *thread) flush(ctx context.Context, st *turnState, r Renderer) func() {
	loopCtx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})

	go func() {
		defer close(done)

		// render sends whatever text has accumulated. It is called on the way
		// out too: a short answer can arrive entirely between two ticks, and
		// dropping it would mean showing nothing until the turn ends.
		render := func(exiting bool) {
			delta := st.takePending()
			if delta == "" {
				return
			}
			renderCtx := ctx
			if exiting {
				// loopCtx is already canceled here, and on shutdown so is ctx.
				// The last render gets its own budget instead of being
				// abandoned by the cancellation that triggered it.
				var cancelRender context.CancelFunc
				renderCtx, cancelRender = context.WithTimeout(context.WithoutCancel(ctx), 15*time.Second)
				defer cancelRender()
			}
			if err := r.Delta(renderCtx, delta); err != nil {
				th.log.Warn("cannot update the reply", "error", err)
			}
		}

		ticker := time.NewTicker(time.Duration(th.b.cfg.Render.FlushMS) * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-loopCtx.Done():
				render(true)
				return
			case <-st.doneCh:
				render(true)
				return
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
