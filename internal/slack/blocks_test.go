package slack

import (
	"testing"

	"github.com/tigersoldier/pi-chat/internal/bot"
)

func TestEveryButtonInOneBlockGetsItsOwnActionID(t *testing.T) {
	// Slack refuses the whole message when one actions block repeats an
	// action_id, and a picker repeats one by design: every button means the same
	// thing and carries its own choice in its value. /pi resume's picker was
	// rejected exactly that way the first time it had two sessions to offer —
	// and because the error report went through the same response URL, the
	// command was silent at both ends.
	buttons := make([]bot.Button, 0, buttonLimit+1)
	for i := 0; i < buttonLimit+1; i++ {
		buttons = append(buttons, bot.Button{ActionID: bot.ActionResume, Text: "a session", Value: "/sessions/a.jsonl"})
	}

	blocks := blocksFor(buttons)
	if len(blocks) != 1 {
		t.Fatalf("blocks = %#v, want one actions block", blocks)
	}
	elements, ok := blocks[0]["elements"].([]block)
	if !ok {
		t.Fatalf("elements = %#v, want an array", blocks[0]["elements"])
	}
	// The block is capped, so the ids below are the ones that would have been
	// rendered — and the cap is what keeps them within Slack's own limit.
	if len(elements) != buttonLimit {
		t.Fatalf("rendered %d buttons, want the cap of %d", len(elements), buttonLimit)
	}

	seen := map[string]bool{}
	for i, element := range elements {
		id, _ := element["action_id"].(string)
		if id == "" {
			t.Fatalf("button %d has no action id: %#v", i, element)
		}
		if seen[id] {
			t.Errorf("action id %q was rendered twice, which Slack rejects", id)
		}
		seen[id] = true
		if got := coreActionID(id); got != bot.ActionResume {
			t.Errorf("coreActionID(%q) = %q, want the core's own id back", id, got)
		}
	}
}

func TestCoreActionIDOnlyRemovesTheSuffixItAdded(t *testing.T) {
	// The press path hands the id back to the core, so only the adapter's own
	// `:<digits>` suffix may be removed: an action id of the core's that contains
	// a colon of its own has to survive the round trip.
	for _, tc := range []struct {
		wire string
		core string
	}{
		{"resume:0", "resume"},
		{"delete-cancel:12", "delete-cancel"},
		{"model:set:3", "model:set"},
		{"resume", "resume"},
		{"model:set", "model:set"},
		{"x:", "x:"},
		{"x:-1", "x:-1"},
		{"", ""},
	} {
		if got := coreActionID(tc.wire); got != tc.core {
			t.Errorf("coreActionID(%q) = %q, want %q", tc.wire, got, tc.core)
		}
	}
}

func TestAPressArrivesWithTheCoreActionID(t *testing.T) {
	// The two halves of the same rule: what blocksFor renders is what a press
	// carries back, and the core is handed its own name for the button — not the
	// position the adapter added.
	env := Envelope{EnvelopeID: "Ev1", Type: "interactive", Payload: []byte(
		`{"type":"block_actions","team":{"id":"T1"},"user":{"id":"U1"},` +
			`"channel":{"id":"C1"},"message":{"ts":"2","thread_ts":"1.1"},` +
			`"response_url":"https://hooks.example/2",` +
			`"actions":[{"action_id":"resume:1","value":"/sessions/a.jsonl"}]}`)}

	action, ok := parseAction(env)
	if !ok {
		t.Fatal("the press was dropped")
	}
	if action.ActionID != bot.ActionResume {
		t.Errorf("ActionID = %q, want %q", action.ActionID, bot.ActionResume)
	}
	if action.Value != "/sessions/a.jsonl" {
		t.Errorf("Value = %q, want the button's own payload", action.Value)
	}
	if action.Thread == nil || action.Thread.ThreadTS != "1.1" {
		t.Errorf("Thread = %#v, want the thread the button was pressed in", action.Thread)
	}
}
