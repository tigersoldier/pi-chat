package selfdm

import (
	"context"
	"net/url"
	"strings"
	"testing"

	"github.com/tigersoldier/pi-chat/internal/bot"
	"github.com/tigersoldier/pi-chat/internal/slack"
)

// TestRouterTurnsARootMessageIntoANewSession: a root message in a DM is the
// conversation's own address, so it is the first message of the thread it starts
// — exactly what a DM with the app does.
func TestRouterTurnsARootMessageIntoANewSession(t *testing.T) {
	s, core, _, _ := surfaceFor(t, nil)
	s.router.Handle(context.Background(), slack.Message{
		TS:   "5.0",
		User: "U1",
		Text: "fix the failing test",
	})

	core.mu.Lock()
	defer core.mu.Unlock()
	if len(core.messages) != 1 {
		t.Fatalf("messages = %+v, want one", core.messages)
	}
	got := core.messages[0]
	if got.Thread.ThreadTS != "5.0" || got.TS != "5.0" {
		t.Errorf("message = thread %q, ts %q; a root message is its own thread",
			got.Thread.ThreadTS, got.TS)
	}
	if got.Thread.Channel != "D1" || got.Workspace != "T1" || got.UserID != "U1" {
		t.Errorf("message addressed %+v, want channel D1 in T1 from U1", got.Thread)
	}
	if !got.Mentioned || !got.Direct {
		t.Errorf("Mentioned/Direct = %v/%v, want both true: in a self-DM everything is addressed to the agent",
			got.Mentioned, got.Direct)
	}
	if got.Text != "fix the failing test" {
		t.Errorf("text = %q, want the prompt verbatim", got.Text)
	}
}

// TestRouterContinuesTheThreadItRepliesIn is the other half of the DM model: a
// reply joins the session its thread owns rather than starting a new one.
func TestRouterContinuesTheThreadItRepliesIn(t *testing.T) {
	s, core, _, _ := surfaceFor(t, nil)
	s.router.Handle(context.Background(), slack.Message{
		TS: "6.0", ThreadTS: "5.0", User: "U1", Text: "and the other test too",
	})

	core.mu.Lock()
	defer core.mu.Unlock()
	if len(core.messages) != 1 {
		t.Fatalf("messages = %+v, want one", core.messages)
	}
	got := core.messages[0]
	if got.Thread.ThreadTS != "5.0" || got.TS != "6.0" {
		t.Errorf("reply = ts %q in thread %q, want 6.0 in 5.0", got.TS, got.Thread.ThreadTS)
	}
}

// TestRouterIgnoresWhatIsNotARequest: the poller reads everything; the router is
// what decides that an edit, a join, another app's post or the daemon's own
// answer is not a prompt.
func TestRouterIgnoresWhatIsNotARequest(t *testing.T) {
	cases := []struct {
		name string
		msg  slack.Message
	}{
		{"an edit", slack.Message{TS: "5.0", User: "U1", Subtype: "message_changed", Text: "fixed typo"}},
		{"a join", slack.Message{TS: "5.0", User: "U1", Subtype: "channel_join"}},
		{"another app", slack.Message{TS: "5.0", BotID: "B9", Text: "a bot says something"}},
		{"no author", slack.Message{TS: "5.0", Text: "who said this?"}},
		{"nothing at all", slack.Message{TS: "5.0", User: "U1", Text: "   "}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s, core, _, _ := surfaceFor(t, nil)
			s.router.Handle(context.Background(), tc.msg)
			if n, c, a := core.got(); n+c+a != 0 {
				t.Errorf("the core was called %d/%d/%d times, want none", n, c, a)
			}
		})
	}
}

// TestRouterIgnoresItsOwnPosts: both authors are the same person, so the ledger
// is the only thing that can tell them apart — and without it the agent's own
// answer would come back as the next prompt.
func TestRouterIgnoresItsOwnPosts(t *testing.T) {
	s, core, _, st := surfaceFor(t, nil)
	st.markPosted("7.0")
	s.router.Handle(context.Background(), slack.Message{TS: "7.0", User: "U1", Text: "an answer from pi"})
	if n, c, a := core.got(); n+c+a != 0 {
		t.Errorf("the core was called %d/%d/%d times, want none for a posted message", n, c, a)
	}
}

// TestRouterReadsPiCommands: there are no slash commands in a self-DM, so the
// `/pi …` text form is what has to become one — and the prefix is required, so
// somebody else's `/status` is not answered as ours.
func TestRouterReadsPiCommands(t *testing.T) {
	cases := []struct {
		name       string
		ts         string
		threadTS   string
		text       string
		wantText   string
		wantThread bool
	}{
		{"a root command", "5.0", "", "/pi status", "/status", false},
		{"a command with a slash already", "5.0", "", "/pi /status", "/status", false},
		{"a bare /pi asks for help", "5.0", "", "/pi", "/", false},
		{"a forwarded pi command", "6.0", "5.0", "/pi compact", "/compact", true},
		{"a command in the thread", "6.0", "5.0", "/pi delete", "/delete", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s, core, _, _ := surfaceFor(t, nil)
			s.router.Handle(context.Background(), slack.Message{
				TS: tc.ts, ThreadTS: tc.threadTS, User: "U1", Text: tc.text,
			})
			core.mu.Lock()
			defer core.mu.Unlock()
			if len(core.commands) != 1 || len(core.messages) != 0 {
				t.Fatalf("commands/messages = %d/%d, want 1/0", len(core.commands), len(core.messages))
			}
			cmd := core.commands[0]
			if cmd.Text != tc.wantText {
				t.Errorf("command text = %q, want %q", cmd.Text, tc.wantText)
			}
			if (cmd.Thread != nil) != tc.wantThread {
				t.Errorf("command thread = %v, want a thread: %v", cmd.Thread, tc.wantThread)
			}
			if !cmd.Direct || cmd.Channel != "D1" || cmd.Workspace != "T1" {
				t.Errorf("command addressed %+v, want a direct command in D1/T1", cmd)
			}
			// A polled message has no platform event id; without a synthesized one
			// the core would claim "" once and refuse every command after it.
			if want := "selfdm:cmd:D1:" + tc.ts; cmd.EventID != want {
				t.Errorf("EventID = %q, want %q", cmd.EventID, want)
			}
		})
	}
}

// TestRouterTreatsNearMissesAsPrompts: "/pizza" is not a command, and a slash
// without the prefix belongs to whoever else defines it.
func TestRouterTreatsNearMissesAsPrompts(t *testing.T) {
	for _, text := range []string{"/pizza", "/status", "pi status", "hello"} {
		t.Run(text, func(t *testing.T) {
			s, core, _, _ := surfaceFor(t, nil)
			s.router.Handle(context.Background(), slack.Message{TS: "5.0", User: "U1", Text: text})
			core.mu.Lock()
			defer core.mu.Unlock()
			if len(core.messages) != 1 || len(core.commands) != 0 {
				t.Fatalf("%q became %d messages and %d commands, want a prompt",
					text, len(core.messages), len(core.commands))
			}
			if got := core.messages[0].Text; got != strings.TrimSpace(text) {
				t.Errorf("prompt = %q, want %q", got, text)
			}
		})
	}
}

// TestRouterNumbersAnswerAnOffer is the button stand-in: the self-DM cannot
// receive a press, so a numbered reply becomes one — and once the offer is
// spent, the same number is just a message.
func TestRouterNumbersAnswerAnOffer(t *testing.T) {
	routes := map[string]func(url.Values) string{
		"chat.postMessage": func(url.Values) string { return `{"ok":true,"ts":"9.0"}` },
	}
	s, core, _, _ := surfaceFor(t, routes)
	ctx := context.Background()
	if err := s.plat.Post(ctx, bot.Notice{
		Channel: "D1",
		Text:    "which session?",
		Buttons: []bot.Button{
			{ActionID: bot.ActionResume, Text: "session A", Value: "/a.jsonl"},
			{ActionID: bot.ActionResume, Text: "session B", Value: "/b.jsonl"},
		},
	}); err != nil {
		t.Fatalf("Post: %v", err)
	}

	s.router.Handle(ctx, slack.Message{TS: "10.0", User: "U1", Text: "2"})
	core.mu.Lock()
	actions := append([]bot.Action(nil), core.actions...)
	messages := append([]bot.Message(nil), core.messages...)
	core.mu.Unlock()
	if len(actions) != 1 || len(messages) != 0 {
		t.Fatalf("actions/messages = %d/%d, want 1/0: a numbered reply is a button press",
			len(actions), len(messages))
	}
	got := actions[0]
	if got.ActionID != bot.ActionResume || got.Value != "/b.jsonl" || got.MessageTS != "9.0" {
		t.Errorf("action = %+v, want the second resume button on message 9.0", got)
	}
	if !got.Direct || got.Channel != "D1" || got.UserID != "U1" || got.Workspace != "T1" {
		t.Errorf("action addressed %+v, want a direct action in D1/T1 from U1", got)
	}
	if got.EventID == "" {
		t.Error("the synthesized press needs its own event id, or the core's dedupe refuses every later press")
	}

	// The offer is spent; the same number typed again is a prompt.
	s.router.Handle(ctx, slack.Message{TS: "11.0", User: "U1", Text: "2"})
	core.mu.Lock()
	defer core.mu.Unlock()
	if len(core.actions) != 1 || len(core.messages) != 1 {
		t.Errorf("actions/messages = %d/%d after the offer was spent, want 1/1",
			len(core.actions), len(core.messages))
	}
}

// TestRouterMatchesAnOfferInItsOwnThread: an offer made in a thread answers to
// a number in that thread, and not to the same number typed at the root.
func TestRouterMatchesAnOfferInItsOwnThread(t *testing.T) {
	routes := map[string]func(url.Values) string{
		"chat.postMessage": func(url.Values) string { return `{"ok":true,"ts":"9.0"}` },
	}
	s, core, _, _ := surfaceFor(t, routes)
	ctx := context.Background()
	thread := &bot.Thread{Workspace: "T1", Channel: "D1", ThreadTS: "5.0"}
	if err := s.plat.Post(ctx, bot.Notice{
		Thread:  thread,
		Text:    "delete this session?",
		Buttons: []bot.Button{{ActionID: bot.ActionDelete, Text: "Delete", Value: "x"}},
	}); err != nil {
		t.Fatalf("Post: %v", err)
	}

	s.router.Handle(ctx, slack.Message{TS: "10.0", User: "U1", Text: "1"})
	core.mu.Lock()
	if len(core.actions) != 0 || len(core.messages) != 1 {
		core.mu.Unlock()
		t.Fatalf("a root number answered a thread's offer: actions/messages = %d/%d",
			len(core.actions), len(core.messages))
	}
	core.mu.Unlock()

	s.router.Handle(ctx, slack.Message{TS: "11.0", ThreadTS: "5.0", User: "U1", Text: "1"})
	core.mu.Lock()
	defer core.mu.Unlock()
	if len(core.actions) != 1 || len(core.messages) != 1 {
		t.Fatalf("actions/messages = %d/%d, want the thread's number to be the press",
			len(core.actions), len(core.messages))
	}
	if core.actions[0].ActionID != bot.ActionDelete {
		t.Errorf("action = %+v, want the delete button", core.actions[0])
	}
}
