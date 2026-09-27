package slack

import (
	"context"
	"testing"

	"github.com/tigersoldier/pi-chat/internal/bot"
)

// TestSetSuggestedPromptsOmitsThreadTS guards a failure that is invisible: Slack
// documents that including thread_ts in an agent app makes this call fail
// *silently*, which would look exactly like a working integration with no
// prompts in it.
func TestSetSuggestedPromptsOmitsThreadTS(t *testing.T) {
	stub := &stubSlack{}
	platform := newTestPlatform(t, stub, "stream")

	err := platform.SetSuggestedPrompts(context.Background(), "D1", "Try one of these",
		[]bot.Suggestion{{Title: "What changed?", Message: "Summarize the uncommitted changes in app."}})
	if err != nil {
		t.Fatalf("SetSuggestedPrompts: %v", err)
	}

	calls := stub.recorded()
	if len(calls) != 1 || calls[0].method != "assistant.threads.setSuggestedPrompts" {
		t.Fatalf("calls = %v, want one assistant.threads.setSuggestedPrompts", stub.methods())
	}
	params := calls[0].params
	if _, present := params["thread_ts"]; present {
		t.Error("thread_ts was sent: in an agent app Slack ignores the whole call when it is present")
	}
	if params["channel_id"] != "D1" {
		t.Errorf("channel_id = %v, want the conversation it was opened in", params["channel_id"])
	}
	if params["title"] != "Try one of these" {
		t.Errorf("title = %v", params["title"])
	}
	prompts, ok := params["prompts"].([]any)
	if !ok || len(prompts) != 1 {
		t.Fatalf("prompts = %#v, want one", params["prompts"])
	}
	first, ok := prompts[0].(map[string]any)
	if !ok || first["title"] != "What changed?" || first["message"] == "" {
		t.Errorf("prompt = %#v, want the core's title and message", prompts[0])
	}
}

// TestSetSuggestedPromptsLatchesOnMissingScope: the default manifest does not
// request assistant:write, so a plain-bot install answers missing_scope forever.
func TestSetSuggestedPromptsLatchesOnMissingScope(t *testing.T) {
	stub := &stubSlack{failed: map[string]string{"assistant.threads.setSuggestedPrompts": "missing_scope"}}
	platform := newTestPlatform(t, stub, "stream")
	suggestions := []bot.Suggestion{{Title: "a", Message: "b"}}

	for i := 0; i < 3; i++ {
		if err := platform.SetSuggestedPrompts(context.Background(), "D1", "", suggestions); err != nil {
			t.Fatalf("call %d: %v", i, err)
		}
	}
	if got := stub.methods(); len(got) != 1 {
		t.Errorf("made %d calls, want 1: an install without the scope is asked once", len(got))
	}
}

// TestSetSuggestedPromptsSkipsAnEmptyList: Slack requires the prompts argument,
// so an empty list is not worth sending, and the core should not have built one.
func TestSetSuggestedPromptsSkipsAnEmptyList(t *testing.T) {
	stub := &stubSlack{}
	platform := newTestPlatform(t, stub, "stream")

	if err := platform.SetSuggestedPrompts(context.Background(), "D1", "", nil); err != nil {
		t.Fatalf("SetSuggestedPrompts: %v", err)
	}
	if got := stub.methods(); len(got) != 0 {
		t.Errorf("calls = %v, want none", got)
	}
}

// TestParseOpened reads the event that says a user is looking at the bot's
// conversation.
func TestParseOpened(t *testing.T) {
	env := eventsAPI(`{"type":"app_home_opened","user":"U9","channel":"D1","tab":"messages",` +
		`"event_ts":"1700000009.000001"}`)

	opened, ok := parseOpened(env)
	if !ok {
		t.Fatal("the messages tab of the app home was not recognized")
	}
	if opened.Channel != "D1" || opened.UserID != "U9" {
		t.Errorf("opened = %+v, want the conversation and the user", opened)
	}
	if opened.EventID == "" {
		t.Error("the event id is what dedupes a redelivery, and it is empty")
	}
}

func TestParseOpenedIgnoresOtherTabsAndEvents(t *testing.T) {
	cases := map[string]Envelope{
		"the home tab": eventsAPI(`{"type":"app_home_opened","user":"U9","channel":"D1","tab":"home"}`),
		"no tab":       eventsAPI(`{"type":"app_home_opened","user":"U9","channel":"D1"}`),
		"a message":    eventsAPI(`{"type":"message","user":"U9","text":"hi","ts":"1","channel":"D1"}`),
		"no channel":   eventsAPI(`{"type":"app_home_opened","user":"U9","tab":"messages"}`),
		"a button": Envelope{EnvelopeID: "e1", Type: "interactive",
			Payload: []byte(`{"type":"block_actions","user":{"id":"U1"},"channel":{"id":"D1"}}`)},
		"broken payload": eventsAPI(`not json`),
	}
	for name, env := range cases {
		if _, ok := parseOpened(env); ok {
			t.Errorf("%s was read as a conversation opening", name)
		}
	}
}

// TestRouterRoutesTheConversationOpening keeps the router's claim honest: the
// event reaches the core that turns it into suggestions.
func TestRouterRoutesTheConversationOpening(t *testing.T) {
	core := &recordingCore{}
	router := NewRouter(core, botUser, discardLogger())

	router.Handle(context.Background(), eventsAPI(`{"type":"app_home_opened","user":"U9",`+
		`"channel":"D1","tab":"messages","event_ts":"1700000009.000001"}`))

	if len(core.opened) != 1 {
		t.Fatalf("opened = %d, want 1", len(core.opened))
	}
	if core.opened[0].Channel != "D1" {
		t.Errorf("channel = %q", core.opened[0].Channel)
	}
	if len(core.messages) != 0 {
		t.Errorf("messages = %d: opening a conversation is not a prompt", len(core.messages))
	}
}
