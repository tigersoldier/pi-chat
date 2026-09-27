package slack

import (
	"testing"

	"github.com/tigersoldier/pi-chat/internal/bot"
)

const botUser = "U0BOT"

// eventsAPI wraps an event_callback the way Socket Mode delivers it.
func eventsAPI(event string) Envelope {
	return Envelope{
		EnvelopeID: "e1",
		Type:       "events_api",
		Payload:    []byte(`{"type":"event_callback","team_id":"T1","event_id":"Ev1","event":` + event + `}`),
	}
}

func TestParseMessageRootMentionStartsTheThread(t *testing.T) {
	env := eventsAPI(`{"type":"app_mention","user":"U1","text":"<@U0BOT> hello there",` +
		`"ts":"1721609600.000100","channel":"C1","channel_type":"channel"}`)

	message, ok := parseMessage(env, botUser)
	if !ok {
		t.Fatal("a root mention should be answered")
	}
	want := bot.Thread{Workspace: "T1", Channel: "C1", ThreadTS: "1721609600.000100"}
	if message.Thread != want {
		t.Fatalf("thread = %+v, want %+v", message.Thread, want)
	}
	if message.Text != "hello there" {
		t.Fatalf("text = %q, want %q", message.Text, "hello there")
	}
	if message.UserID != "U1" || message.Workspace != "T1" || message.EventID != "Ev1" {
		t.Fatalf("unexpected message: %+v", message)
	}
	if !message.Mentioned || message.Direct {
		t.Errorf("mentioned/direct = %v/%v, want true/false", message.Mentioned, message.Direct)
	}
}

func TestParseMessageMentionInsideAThreadJoinsIt(t *testing.T) {
	env := eventsAPI(`{"type":"app_mention","user":"U1","text":"<@U0BOT> again",` +
		`"ts":"1721609700.000200","thread_ts":"1721609600.000100","channel":"C1"}`)

	message, ok := parseMessage(env, botUser)
	if !ok {
		t.Fatal("a mention in a thread should be answered")
	}
	if message.Thread.ThreadTS != "1721609600.000100" {
		t.Fatalf("thread_ts = %q, want the thread root, not the message", message.Thread.ThreadTS)
	}
}

func TestParseMessagePlainTextInAThread(t *testing.T) {
	// Plain text is a prompt in a thread the bot may already own; the core
	// decides that, because only it knows which threads have sessions. A
	// mention of somebody else is in the same category: it is not addressed to
	// the bot.
	env := eventsAPI(`{"type":"message","user":"U1","text":"and another thing",` +
		`"ts":"1721609800.000300","thread_ts":"1721609600.000100","channel":"C1","channel_type":"channel"}`)

	message, ok := parseMessage(env, botUser)
	if !ok {
		t.Fatal("plain text in a channel thread should reach the core")
	}
	if message.Mentioned || message.Direct {
		t.Errorf("mentioned/direct = %v/%v, want false/false", message.Mentioned, message.Direct)
	}
	if message.Text != "and another thing" {
		t.Errorf("text = %q", message.Text)
	}

	// Somebody else's mention does not address the bot.
	other := eventsAPI(`{"type":"message","user":"U1","text":"<@UOTHER> hi",` +
		`"ts":"1721609800.000400","thread_ts":"1721609600.000100","channel":"C1"}`)
	message, ok = parseMessage(other, botUser)
	if !ok {
		t.Fatal("a message in a thread should still reach the core")
	}
	if message.Mentioned {
		t.Error("a mention of somebody else counted as addressing the bot")
	}
	if message.Text != "<@UOTHER> hi" {
		t.Errorf("text = %q, want it untouched", message.Text)
	}
}

func TestParseMessageDirectMessages(t *testing.T) {
	t.Run("plain text in a DM thread", func(t *testing.T) {
		env := eventsAPI(`{"type":"message","user":"U1","text":"carry on",` +
			`"ts":"1721609900.000400","thread_ts":"1721609600.000100","channel":"D1","channel_type":"im"}`)

		message, ok := parseMessage(env, botUser)
		if !ok {
			t.Fatal("plain text in a DM thread should reach the core without a mention")
		}
		if !message.Direct || message.Mentioned {
			t.Errorf("mentioned/direct = %v/%v, want false/true", message.Mentioned, message.Direct)
		}
	})

	t.Run("a mention at the DM root opens a thread", func(t *testing.T) {
		// DESIGN.md §4: the first `@pi <text>` in a DM root opens a DM thread,
		// rooted at the message that asked for it.
		env := eventsAPI(`{"type":"message","user":"U1","text":"<@U0BOT> hello",` +
			`"ts":"1721609950.000500","channel":"D1","channel_type":"im"}`)

		message, ok := parseMessage(env, botUser)
		if !ok {
			t.Fatal("a mention in a DM root should start a session")
		}
		if message.Thread.ThreadTS != "1721609950.000500" {
			t.Errorf("thread_ts = %q, want the message itself", message.Thread.ThreadTS)
		}
		if !message.Mentioned || !message.Direct {
			t.Errorf("mentioned/direct = %v/%v, want true/true", message.Mentioned, message.Direct)
		}
		if message.Text != "hello" {
			t.Errorf("text = %q, want the mention stripped", message.Text)
		}
	})
}

func TestParseMessageRejectsWhatIsNotAddressedToTheBot(t *testing.T) {
	tests := []struct {
		name string
		env  Envelope
	}{
		{"plain text in a channel root", eventsAPI(`{"type":"message","user":"U1","text":"hi","ts":"1","channel":"C1"}`)},
		{"plain text in a DM root", eventsAPI(`{"type":"message","user":"U1","text":"hi","ts":"1","channel":"D1","channel_type":"im"}`)},
		{"the bot's own post", eventsAPI(`{"type":"app_mention","bot_id":"B1","text":"<@U0BOT> hi","ts":"1","channel":"C1"}`)},
		{"an edited message", eventsAPI(`{"type":"app_mention","subtype":"message_changed","user":"U1","text":"<@U0BOT> hi","ts":"1","channel":"C1"}`)},
		{"no channel", eventsAPI(`{"type":"app_mention","user":"U1","text":"<@U0BOT> hi","ts":"1"}`)},
		{"a slash command", Envelope{EnvelopeID: "e2", Type: "slash_commands", Payload: []byte(`{"command":"/pi"}`)}},
		{"a socket control frame", Envelope{EnvelopeID: "e3", Type: "hello"}},
		{"mangled payload", Envelope{EnvelopeID: "e4", Type: "events_api", Payload: []byte(`not json`)}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if message, ok := parseMessage(test.env, botUser); ok {
				t.Fatalf("should not have been answered: %+v", message)
			}
		})
	}
}

func TestParseMessageCountsFiles(t *testing.T) {
	env := eventsAPI(`{"type":"app_mention","user":"U1","text":"<@U0BOT> look",` +
		`"ts":"1","channel":"C1","files":[{"id":"F1"},{"id":"F2"}]}`)

	message, ok := parseMessage(env, botUser)
	if !ok {
		t.Fatal("a mention with files should still parse")
	}
	if message.Files != 2 {
		t.Fatalf("files = %d, want 2", message.Files)
	}
}

func TestParseCommandEnvelope(t *testing.T) {
	env := Envelope{
		EnvelopeID: "e9",
		Type:       "slash_commands",
		Payload: []byte(`{"type":"slash_command","team_id":"T1","channel_id":"C1",` +
			`"user_id":"U1","command":"/pi","text":" status ","response_url":"https://hooks.example/1"}`),
	}

	command, ok := parseCommand(env)
	if !ok {
		t.Fatal("a slash command should parse")
	}
	// Slack's payload omits the slash the user typed; the core's grammar always
	// has it (DESIGN.md §5).
	if command.Text != "/status" {
		t.Errorf("text = %q, want %q", command.Text, "/status")
	}
	// The payload has no thread timestamp, which is exactly why a root command
	// is session-less.
	if command.Thread != nil {
		t.Errorf("a root command got a thread: %+v", command.Thread)
	}
	if command.Channel != "C1" || command.UserID != "U1" || command.Workspace != "T1" {
		t.Errorf("unexpected command: %+v", command)
	}
	if command.ReplyTo != "https://hooks.example/1" {
		t.Errorf("reply handle = %q", command.ReplyTo)
	}
	// A slash-command payload carries no event id, so the envelope is the only
	// thing left to dedupe on.
	if command.EventID != "e9" {
		t.Errorf("event id = %q, want the envelope id", command.EventID)
	}
}

func TestParseCommandRejectsOtherEnvelopes(t *testing.T) {
	tests := []struct {
		name string
		env  Envelope
	}{
		{"an event", eventsAPI(`{"type":"message","user":"U1","text":"hi","ts":"1","channel":"C1"}`)},
		{"no channel", Envelope{Type: "slash_commands", Payload: []byte(`{"user_id":"U1","text":"status"}`)}},
		{"no user", Envelope{Type: "slash_commands", Payload: []byte(`{"channel_id":"C1","text":"status"}`)}},
		{"mangled payload", Envelope{Type: "slash_commands", Payload: []byte(`{`)}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if command, ok := parseCommand(test.env); ok {
				t.Fatalf("should not have parsed: %+v", command)
			}
		})
	}
}

func TestParseActionEnvelope(t *testing.T) {
	t.Run("a button in a thread", func(t *testing.T) {
		env := Envelope{
			EnvelopeID: "e10",
			Type:       "interactive",
			Payload: []byte(`{"type":"block_actions","team":{"id":"T1"},"user":{"id":"U1"},` +
				`"channel":{"id":"C1"},"response_url":"https://hooks.example/2",` +
				`"message":{"ts":"1721609600.000200","thread_ts":"1721609600.000100"},` +
				`"actions":[{"action_id":"resume","value":"/sessions/a.jsonl"}]}`),
		}

		action, ok := parseAction(env)
		if !ok {
			t.Fatal("a block_actions payload should parse")
		}
		if action.ActionID != "resume" || action.Value != "/sessions/a.jsonl" {
			t.Errorf("unexpected action: %+v", action)
		}
		if action.Thread == nil || action.Thread.ThreadTS != "1721609600.000100" {
			t.Errorf("the thread of the message was lost: %+v", action.Thread)
		}
		if action.MessageTS != "1721609600.000200" {
			t.Errorf("message ts = %q", action.MessageTS)
		}
		if action.Channel != "C1" || action.UserID != "U1" || action.Workspace != "T1" {
			t.Errorf("unexpected action: %+v", action)
		}
	})

	t.Run("a button on a channel-root message has no thread", func(t *testing.T) {
		// The /pi resume picker is posted at the root: picking from it opens a
		// thread rather than joining one.
		env := Envelope{
			EnvelopeID: "e11",
			Type:       "interactive",
			Payload: []byte(`{"type":"block_actions","team":{"id":"T1"},"user":{"id":"U1"},` +
				`"channel":{"id":"C1"},"message":{"ts":"1721609600.000200"},` +
				`"actions":[{"action_id":"resume","value":"/sessions/a.jsonl"}]}`),
		}

		action, ok := parseAction(env)
		if !ok {
			t.Fatal("a root button should parse")
		}
		if action.Thread != nil {
			t.Errorf("a root message got a thread: %+v", action.Thread)
		}
	})
}

func TestParseActionRejectsOtherPayloads(t *testing.T) {
	tests := []struct {
		name string
		env  Envelope
	}{
		{"an event", eventsAPI(`{"type":"message","user":"U1","text":"hi","ts":"1","channel":"C1"}`)},
		{"no action", Envelope{Type: "interactive", Payload: []byte(`{"type":"block_actions","user":{"id":"U1"},"channel":{"id":"C1"},"actions":[]}`)}},
		{"another interaction type", Envelope{Type: "interactive", Payload: []byte(`{"type":"view_submission","user":{"id":"U1"},"channel":{"id":"C1"},"actions":[{"action_id":"x"}]}`)}},
		{"no user", Envelope{Type: "interactive", Payload: []byte(`{"type":"block_actions","channel":{"id":"C1"},"actions":[{"action_id":"x"}]}`)}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if action, ok := parseAction(test.env); ok {
				t.Fatalf("should not have parsed: %+v", action)
			}
		})
	}
}

func TestStripMention(t *testing.T) {
	tests := []struct {
		name string
		text string
		want string
	}{
		{"plain mention", "<@U0BOT> hello", " hello"},
		{"labelled mention", "<@U0BOT|pi> hello", " hello"},
		{"mention mid-sentence", "hey <@U0BOT> please", "hey  please"},
		{"someone else's mention", "<@UOTHER> hello", "<@UOTHER> hello"},
		{"no mention", "hello", "hello"},
		{"unterminated mention", "<@U0BOT hello", "<@U0BOT hello"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := stripMention(test.text, botUser); got != test.want {
				t.Fatalf("stripMention(%q) = %q, want %q", test.text, got, test.want)
			}
		})
	}

	// Without the bot's own ID there is nothing safe to remove.
	if got := stripMention("<@U0BOT> hi", ""); got != "<@U0BOT> hi" {
		t.Fatalf("stripMention with no bot id = %q", got)
	}
}
