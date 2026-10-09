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

func TestParseMessagePlainRoomTextIsHandedOverUnaddressed(t *testing.T) {
	// In a room only a mention turns, and `app_mention` owns that: Slack sends both
	// events for one mention, so the message event carrying it is the same request
	// over again and is dropped here.
	//
	// What is left is the room's own conversation, and it is handed to the core
	// *unaddressed* rather than discarded. The core ignores it without a notice;
	// the next addressed turn reads it back as context (DESIGN.md §4, §5).
	mention := eventsAPI(`{"type":"message","user":"U1","text":"<@U0BOT> again",` +
		`"ts":"1721609800.000400","thread_ts":"1721609600.000100","channel":"C1","channel_type":"channel"}`)
	if _, ok := parseMessage(mention, botUser); ok {
		t.Error("a channel message event carrying a mention was handed over; its app_mention copy owns it")
	}

	plain := eventsAPI(`{"type":"message","user":"U1","text":"and another thing",` +
		`"ts":"1721609800.000300","thread_ts":"1721609600.000100","channel":"C1","channel_type":"channel"}`)
	message, ok := parseMessage(plain, botUser)
	if !ok {
		t.Fatal("a plain message in a channel thread should reach the core, unaddressed")
	}
	if message.Mentioned || message.Direct {
		t.Errorf("mentioned/direct = %v/%v, want false/false", message.Mentioned, message.Direct)
	}

	// A mention of somebody else is the same conversation: it does not address the
	// bot, and the core decides what to do with it.
	other := eventsAPI(`{"type":"message","user":"U1","text":"<@UOTHER> hi",` +
		`"ts":"1721609800.000500","thread_ts":"1721609600.000100","channel":"C1"}`)
	otherMessage, ok := parseMessage(other, botUser)
	if !ok {
		t.Fatal("a mention of somebody else is conversation, not a request for the bot")
	}
	if otherMessage.Mentioned {
		t.Error("a mention of somebody else was taken as addressing the bot")
	}

	// At a channel root there is no thread to belong to: nothing to read back, and
	// nobody to explain anything to, so the core is not troubled with it.
	root := eventsAPI(`{"type":"message","user":"U1","text":"hello everyone",` +
		`"ts":"1721609800.000600","channel":"C1","channel_type":"channel"}`)
	if _, ok := parseMessage(root, botUser); ok {
		t.Error("a plain message at a channel root was handed to the core")
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

	t.Run("plain text at the DM root opens a thread", func(t *testing.T) {
		// A suggested prompt arrives exactly like this, and so does the first
		// thing anyone types in the DM composer: no mention, no thread yet.
		env := eventsAPI(`{"type":"message","user":"U1","text":"fix the failing test",` +
			`"ts":"1721609940.000450","channel":"D1","channel_type":"im"}`)

		message, ok := parseMessage(env, botUser)
		if !ok {
			t.Fatal("plain text in a DM root should start a session")
		}
		if !message.Direct || message.Mentioned {
			t.Errorf("mentioned/direct = %v/%v, want false/true", message.Mentioned, message.Direct)
		}
		if message.Thread.ThreadTS != "1721609940.000450" {
			t.Errorf("thread_ts = %q, want the message's own timestamp", message.Thread.ThreadTS)
		}
	})

	t.Run("a group DM is a room, not an address", func(t *testing.T) {
		// Two or more people are in it and they talk to each other, so it behaves
		// like a channel (DESIGN.md §5): a bare message is not addressed to the bot,
		// and app_mention is the event that answers a mention there.
		root := eventsAPI(`{"type":"message","user":"U1","text":"what changed?",` +
			`"ts":"1721609960.000600","channel":"G1","channel_type":"mpim"}`)
		if _, ok := parseMessage(root, botUser); ok {
			t.Error("a bare top-level message in a group DM is not addressed to the bot")
		}

		// A reply inside one of its threads is handed over unaddressed, for the same
		// reason a channel thread's is: it is conversation, not a request, and the
		// next addressed turn reads it as context.
		reply := eventsAPI(`{"type":"message","user":"U1","text":"I think so too",` +
			`"ts":"1721609960.000700","thread_ts":"1721609960.000600",` +
			`"channel":"G1","channel_type":"mpim"}`)
		message, ok := parseMessage(reply, botUser)
		if !ok {
			t.Fatal("a reply in a group DM thread should reach the core, unaddressed")
		}
		if message.Mentioned || message.Direct {
			t.Errorf("mentioned/direct = %v/%v, want false/false for a group DM",
				message.Mentioned, message.Direct)
		}
	})

	t.Run("a mention in a group DM turns", func(t *testing.T) {
		env := eventsAPI(`{"type":"app_mention","user":"U1","text":"<@U0BOT> what changed?",` +
			`"ts":"1721609960.000600","channel":"G1","channel_type":"mpim"}`)

		message, ok := parseMessage(env, botUser)
		if !ok {
			t.Fatal("a mention in a group DM should reach the core")
		}
		if !message.Mentioned || message.Direct {
			t.Errorf("mentioned/direct = %v/%v, want true/false", message.Mentioned, message.Direct)
		}
	})

	t.Run("an app_mention in a DM is not answered", func(t *testing.T) {
		// Slack does not deliver app_mention for a DM at all — and if it ever did,
		// message.im is the copy that owns the conversation.
		env := eventsAPI(`{"type":"app_mention","user":"U1","text":"<@U0BOT> hello",` +
			`"ts":"1721609955.000480","channel":"D1","channel_type":"im"}`)
		if _, ok := parseMessage(env, botUser); ok {
			t.Error("an app_mention event in a DM was answered; message.im owns a DM")
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
		{"a channel message event carrying a mention, which app_mention owns", eventsAPI(`{"type":"message","user":"U1","text":"<@U0BOT> hi","ts":"1","channel":"C1"}`)},
		{"an app_mention in a DM, which message.im owns", eventsAPI(`{"type":"app_mention","user":"U1","text":"<@U0BOT> hi","ts":"1","channel":"D1","channel_type":"im"}`)},
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

func TestParseMessagePreservesFileIdentities(t *testing.T) {
	env := eventsAPI(`{"type":"app_mention","user":"U1","text":"<@U0BOT> look",` +
		`"ts":"1","channel":"C1","files":[{"id":"F1"},{"id":"F2"}]}`)

	message, ok := parseMessage(env, botUser)
	if !ok {
		t.Fatal("a mention with files should still parse")
	}
	if len(message.Files) != 2 {
		t.Fatalf("files = %d, want 2", len(message.Files))
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
