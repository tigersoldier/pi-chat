package slack

import (
	"context"
	"testing"

	"github.com/tigersoldier/pi-chat/internal/bot"
)

// The status is what Slack renders as a loading indicator and a stop button, so
// the mapping onto its own vocabulary is worth pinning: a typo here is a
// `invalid_status` error at runtime and nothing on screen.
func TestSetStatusMapsTheCoreStates(t *testing.T) {
	cases := []struct {
		core bot.Status
		want string
	}{
		{bot.StatusBusy, "processing"},
		{bot.StatusWaiting, "suspended"},
		{bot.StatusIdle, "active"},
		{bot.StatusClosed, "closed"},
	}
	for _, tc := range cases {
		t.Run(tc.want, func(t *testing.T) {
			stub := &stubSlack{}
			platform := newTestPlatform(t, stub, "stream")
			thread := bot.Thread{Workspace: "T1", Channel: "C1", ThreadTS: "1700000000.000100"}

			if err := platform.SetStatus(context.Background(), thread, tc.core); err != nil {
				t.Fatalf("SetStatus(%q): %v", tc.core, err)
			}

			calls := stub.recorded()
			if len(calls) != 1 || calls[0].method != "agents.sessions.setStatus" {
				t.Fatalf("calls = %+v, want one agents.sessions.setStatus", stub.methods())
			}
			params := calls[0].params
			if params["status"] != tc.want {
				t.Errorf("status = %v, want %q", params["status"], tc.want)
			}
			// The thread is what scopes the status to one conversation, and the
			// channel is required for public channels.
			if params["channel_id"] != "C1" || params["thread_ts"] != "1700000000.000100" {
				t.Errorf("params = %+v, want the thread's channel and thread_ts", params)
			}
		})
	}
}

// TestSetStatusAsksOnceWhenTheInstallCannotShowOne: a workspace without the
// agent feature answers `feature_disabled` forever, an install that is not an
// agent app at all answers `not_agent_app`, and a per-turn warning about a
// cosmetic loss is noise. The refusal is remembered, and it is not an error —
// turns run without an indicator.
func TestSetStatusAsksOnceWhenTheInstallCannotShowOne(t *testing.T) {
	for _, code := range []string{"feature_disabled", "not_agent_app", "missing_scope"} {
		t.Run(code, func(t *testing.T) {
			stub := &stubSlack{failed: map[string]string{"agents.sessions.setStatus": code}}
			platform := newTestPlatform(t, stub, "stream")
			thread := bot.Thread{Workspace: "T1", Channel: "C1", ThreadTS: "1.2"}

			for i := 0; i < 3; i++ {
				if err := platform.SetStatus(context.Background(), thread, bot.StatusBusy); err != nil {
					t.Fatalf("SetStatus %d: %v", i, err)
				}
			}
			if got := stub.methods(); len(got) != 1 {
				t.Errorf("made %d calls, want 1: an install that cannot show one is asked once", len(got))
			}
		})
	}
}

// TestSetStatusReportsATransientFailure: an error worth retrying must reach the
// caller (and must not latch the install off forever).
func TestSetStatusReportsATransientFailure(t *testing.T) {
	stub := &stubSlack{failed: map[string]string{"agents.sessions.setStatus": "internal_error"}}
	platform := newTestPlatform(t, stub, "stream")
	thread := bot.Thread{Workspace: "T1", Channel: "C1", ThreadTS: "1.2"}

	if err := platform.SetStatus(context.Background(), thread, bot.StatusBusy); err == nil {
		t.Fatal("a transient failure was swallowed")
	}
	_ = platform.SetStatus(context.Background(), thread, bot.StatusBusy)
	if got := stub.methods(); len(got) != 2 {
		t.Errorf("made %d calls, want 2: a transient failure must not latch the install off", len(got))
	}
}

// TestParseStopped reads Slack's own stop button.
func TestParseStopped(t *testing.T) {
	env := eventsAPI(`{"type":"agent_session_stopped","user":"U9","channel":"C1",` +
		`"thread_ts":"1700000000.000100","event_ts":"1700000009.000001",` +
		`"streaming_message_ts":["1700000000.000002"]}`)

	action, ok := parseStopped(env)
	if !ok {
		t.Fatal("the stop event was not recognized")
	}
	if action.ActionID != bot.ActionStop {
		t.Errorf("action = %q, want %q", action.ActionID, bot.ActionStop)
	}
	if action.Thread == nil || action.Thread.ThreadTS != "1700000000.000100" || action.Thread.Channel != "C1" {
		t.Errorf("thread = %+v, want the thread the event named", action.Thread)
	}
	if action.UserID != "U9" {
		t.Errorf("user = %q, want the person who pressed stop", action.UserID)
	}
	if action.EventID == "" {
		t.Error("the envelope id is the only thing to dedupe a stop on, and it is empty")
	}
}

// TestParseStoppedIgnoresWhatItCannotRoute: a stop without a thread names no
// session, so there is nothing to stop.
func TestParseStoppedIgnoresWhatItCannotRoute(t *testing.T) {
	cases := map[string]Envelope{
		"a message":      eventsAPI(`{"type":"app_mention","user":"U1","text":"<@U0BOT> hi","ts":"1","channel":"C1"}`),
		"no thread":      eventsAPI(`{"type":"agent_session_stopped","user":"U9","channel":"C1"}`),
		"no channel":     eventsAPI(`{"type":"agent_session_stopped","user":"U9","thread_ts":"1.2"}`),
		"not an event":   {EnvelopeID: "e1", Type: "interactive", Payload: []byte(`{}`)},
		"broken payload": eventsAPI(`not json`),
	}
	for name, env := range cases {
		if _, ok := parseStopped(env); ok {
			t.Errorf("%s was read as a stop", name)
		}
	}
}

// TestRouterRoutesTheStopButton: the stop arrives on the action path, which is
// where the allowlist and the thread lookup live.
func TestRouterRoutesTheStopButton(t *testing.T) {
	core := &recordingCore{}
	router := NewRouter(core, botUser, discardLogger())

	router.Handle(context.Background(), eventsAPI(`{"type":"agent_session_stopped","user":"U9",`+
		`"channel":"C1","thread_ts":"1700000000.000100","event_ts":"1700000009.000001"}`))

	if len(core.actions) != 1 {
		t.Fatalf("actions = %d, want 1", len(core.actions))
	}
	if core.actions[0].ActionID != bot.ActionStop {
		t.Errorf("action = %q, want %q", core.actions[0].ActionID, bot.ActionStop)
	}
	// A stop is not a message: it must not become a prompt.
	if len(core.messages) != 0 {
		t.Errorf("messages = %d, want 0", len(core.messages))
	}
}
