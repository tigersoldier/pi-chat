package slack

import (
	"context"
	"testing"

	"github.com/tigersoldier/pi-chat/internal/bot"
)

// recordingCore records what the router handed to the core.
type recordingCore struct {
	messages []bot.Message
	commands []bot.Command
	actions  []bot.Action
}

func (c *recordingCore) HandleMessage(_ context.Context, m bot.Message) {
	c.messages = append(c.messages, m)
}
func (c *recordingCore) HandleCommand(_ context.Context, command bot.Command) {
	c.commands = append(c.commands, command)
}
func (c *recordingCore) HandleAction(_ context.Context, a bot.Action) {
	c.actions = append(c.actions, a)
}

func TestRouterDispatchesEveryKindItAnswers(t *testing.T) {
	core := &recordingCore{}
	router := NewRouter(core, botUser, discardLogger())
	ctx := context.Background()

	router.Handle(ctx, eventsAPI(`{"type":"app_mention","user":"U1","text":"<@U0BOT> hi",`+
		`"ts":"1","channel":"C1"}`))
	router.Handle(ctx, Envelope{
		EnvelopeID: "e2",
		Type:       "slash_commands",
		Payload:    []byte(`{"team_id":"T1","channel_id":"C1","user_id":"U1","text":"status"}`),
	})
	router.Handle(ctx, Envelope{
		EnvelopeID: "e3",
		Type:       "interactive",
		Payload: []byte(`{"type":"block_actions","team":{"id":"T1"},"user":{"id":"U1"},` +
			`"channel":{"id":"C1"},"message":{"ts":"2"},"actions":[{"action_id":"resume","value":"/sessions/a.jsonl"}]}`),
	})

	if len(core.messages) != 1 {
		t.Errorf("messages = %d, want 1", len(core.messages))
	}
	if len(core.commands) != 1 {
		t.Errorf("commands = %d, want 1", len(core.commands))
	}
	if len(core.actions) != 1 {
		t.Errorf("actions = %d, want 1", len(core.actions))
	}
	if got := core.commands[0].Text; got != "/status" {
		t.Errorf("command text = %q, want the normalized form", got)
	}
	if core.actions[0].Value != "/sessions/a.jsonl" {
		t.Errorf("action value = %q", core.actions[0].Value)
	}
}

func TestRouterDropsWhatItDoesNotAnswer(t *testing.T) {
	core := &recordingCore{}
	router := NewRouter(core, botUser, discardLogger())
	ctx := context.Background()

	// Socket control frames, an unaddressed channel message, and a malformed
	// payload: none of them is a core call, and none of them may panic.
	for _, env := range []Envelope{
		{EnvelopeID: "e1", Type: "hello"},
		{EnvelopeID: "e2", Type: "disconnect", Reason: "refresh_requested"},
		eventsAPI(`{"type":"message","user":"U1","text":"hi","ts":"1","channel":"C1"}`),
		{EnvelopeID: "e4", Type: "events_api", Payload: []byte(`not json`)},
		{EnvelopeID: "e5", Type: "unknown_kind", Payload: []byte(`{}`)},
	} {
		router.Handle(ctx, env)
	}

	if len(core.messages)+len(core.commands)+len(core.actions) != 0 {
		t.Fatalf("the router called the core for an envelope it does not answer: %+v", core)
	}
}
