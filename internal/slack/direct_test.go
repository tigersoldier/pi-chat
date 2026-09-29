package slack

import (
	"testing"

	"github.com/tigersoldier/pi-chat/internal/bot"
)

func TestIsDirectChannel(t *testing.T) {
	// A one-to-one conversation is the one context where the conversation itself is
	// the address, so everything here decides how a notice tells somebody to keep
	// going. Slack names it `directmessage`; a group DM has the generated `mpdm-…`
	// name and behaves like a channel, because with two or more people in it the
	// conversation is no longer only the bot's (DESIGN.md §5).
	for _, tc := range []struct {
		id, name string
		want     bool
	}{
		{"D0C4RK6J1V0", "directmessage", true},
		{"D0C4RK6J1V0", "", true}, // the id is the fallback when no name travels
		{"C0C4TMQNR8R", "general", false},
		{"C0C4TMQNR8R", "", false},
		{"G1", "mpdm-alice--bob-1", false},
		{"G1", "", false},
		{"", "", false},
	} {
		if got := isDirectChannel(tc.id, tc.name); got != tc.want {
			t.Errorf("isDirectChannel(%q, %q) = %v, want %v", tc.id, tc.name, got, tc.want)
		}
	}
}

func TestAPressKnowsWhetherItWasPressedInADM(t *testing.T) {
	for _, tc := range []struct {
		channel string
		name    string
		want    bool
	}{
		{"D1", "directmessage", true},
		{"C1", "general", false},
	} {
		env := Envelope{EnvelopeID: "Ev1", Type: "interactive", Payload: []byte(
			`{"type":"block_actions","team":{"id":"T1"},"user":{"id":"U1"},` +
				`"channel":{"id":"` + tc.channel + `","name":"` + tc.name + `"},` +
				`"message":{"ts":"2","thread_ts":"1.1"},"response_url":"https://hooks.example/2",` +
				`"actions":[{"action_id":"resume:0","value":"/sessions/a.jsonl"}]}`)}

		action, ok := parseAction(env)
		if !ok {
			t.Fatalf("the press in %s was dropped", tc.channel)
		}
		if action.Direct != tc.want {
			t.Errorf("a press in %s: Direct = %v, want %v", tc.channel, action.Direct, tc.want)
		}
	}
}

func TestASlashCommandKnowsWhetherItWasTypedInADM(t *testing.T) {
	// /pi resume is legal in a DM, and the thread it opens there answers plain text
	// rather than mentions — so the command has to carry the context with it.
	for _, tc := range []struct {
		channel string
		name    string
		want    bool
	}{
		{"D1", "directmessage", true},
		{"C1", "general", false},
	} {
		env := Envelope{EnvelopeID: "Ev1", Type: "slash_commands", Payload: []byte(
			`{"team_id":"T1","channel_id":"` + tc.channel + `","channel_name":"` + tc.name + `",` +
				`"user_id":"U1","text":"resume","response_url":"https://hooks.example/1"}`)}

		command, ok := parseCommand(env)
		if !ok {
			t.Fatalf("the command in %s was dropped", tc.channel)
		}
		if command.Direct != tc.want {
			t.Errorf("a command in %s: Direct = %v, want %v", tc.channel, command.Direct, tc.want)
		}
		if command.Text != "/resume" {
			t.Errorf("Text = %q, want the slash form", command.Text)
		}
	}
}

// The seam's own types have to carry the field for any of this to mean anything.
var _ = bot.Command{Direct: true}
var _ = bot.Action{Direct: true}
