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

	message, ok := ParseMessage(env, botUser)
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
	if message.UserID != "U1" || message.TeamID != "T1" || message.EventID != "Ev1" {
		t.Fatalf("unexpected message: %+v", message)
	}
}

func TestParseMessageMentionInsideAThreadJoinsIt(t *testing.T) {
	env := eventsAPI(`{"type":"app_mention","user":"U1","text":"<@U0BOT> again",` +
		`"ts":"1721609700.000200","thread_ts":"1721609600.000100","channel":"C1"}`)

	message, ok := ParseMessage(env, botUser)
	if !ok {
		t.Fatal("a mention in a thread should be answered")
	}
	if message.Thread.ThreadTS != "1721609600.000100" {
		t.Fatalf("thread_ts = %q, want the thread root, not the message", message.Thread.ThreadTS)
	}
}

func TestParseMessageRejectsWhatPhase0DoesNotAnswer(t *testing.T) {
	tests := []struct {
		name string
		env  Envelope
	}{
		{"plain message", eventsAPI(`{"type":"message","user":"U1","text":"hi","ts":"1","channel":"C1"}`)},
		{"the bot's own post", eventsAPI(`{"type":"app_mention","bot_id":"B1","text":"<@U0BOT> hi","ts":"1","channel":"C1"}`)},
		{"an edited message", eventsAPI(`{"type":"app_mention","subtype":"message_changed","user":"U1","text":"<@U0BOT> hi","ts":"1","channel":"C1"}`)},
		{"no channel", eventsAPI(`{"type":"app_mention","user":"U1","text":"<@U0BOT> hi","ts":"1"}`)},
		{"a slash command", Envelope{EnvelopeID: "e2", Type: "slash_commands", Payload: []byte(`{"command":"/pi"}`)}},
		{"a socket control frame", Envelope{EnvelopeID: "e3", Type: "hello"}},
		{"mangled payload", Envelope{EnvelopeID: "e4", Type: "events_api", Payload: []byte(`not json`)}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if message, ok := ParseMessage(test.env, botUser); ok {
				t.Fatalf("should not have been answered: %+v", message)
			}
		})
	}
}

func TestParseMessageCountsFiles(t *testing.T) {
	env := eventsAPI(`{"type":"app_mention","user":"U1","text":"<@U0BOT> look",` +
		`"ts":"1","channel":"C1","files":[{"id":"F1"},{"id":"F2"}]}`)

	message, ok := ParseMessage(env, botUser)
	if !ok {
		t.Fatal("a mention with files should still parse")
	}
	if message.Files != 2 {
		t.Fatalf("files = %d, want 2", message.Files)
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
