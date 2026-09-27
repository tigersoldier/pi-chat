package bot

import (
	"context"
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

// recordingRenderer records what a turn renders.
type recordingRenderer struct {
	mu      sync.Mutex
	deltas  []string
	finals  []string
	failure string
	signal  chan struct{} // if set, receives a value on every Delta
}

func (r *recordingRenderer) Start(context.Context) error { return nil }

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
	r.finals = append(r.finals, final)
	return nil
}

func (r *recordingRenderer) Fail(_ context.Context, cause error) error {
	r.failure = cause.Error()
	return nil
}

func (r *recordingRenderer) rendered() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return strings.Join(r.deltas, "")
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
