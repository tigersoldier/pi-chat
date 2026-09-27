package slack

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/tigersoldier/pi-chat/internal/bot"
)

// noticeFor builds the notice a test wants to post.
func noticeFor() bot.Notice {
	return bot.Notice{Channel: "C1", UserID: "U1", Text: "hello"}
}

func TestPostChoosesWhereANoticeGoes(t *testing.T) {
	thread := bot.Thread{Workspace: "T1", Channel: "C1", ThreadTS: "1700000000.000100"}

	tests := []struct {
		name         string
		mutate       func(*bot.Notice)
		wantMethod   string
		wantThreadTS any
		wantReplace  bool
	}{
		{
			name:       "a plain notice is a message in the thread",
			mutate:     func(n *bot.Notice) { n.Thread = &thread },
			wantMethod: "chat.postMessage",
			// A thread notice must stay in its thread, or it would appear at the
			// channel root instead of with the conversation.
			wantThreadTS: thread.ThreadTS,
		},
		{
			name:         "an ephemeral notice is shown to one user",
			mutate:       func(n *bot.Notice) { n.Ephemeral = true },
			wantMethod:   "chat.postEphemeral",
			wantThreadTS: nil,
		},
		{
			name:         "an update replaces a posted message",
			mutate:       func(n *bot.Notice) { n.Update = "1700000000.000200" },
			wantMethod:   "chat.update",
			wantThreadTS: nil,
		},
		{
			name:       "a response URL answers the interaction that asked",
			mutate:     func(n *bot.Notice) { n.ReplyTo = "RESPONSE_URL" },
			wantMethod: "response",
		},
		{
			name: "a response URL with an update replaces the original",
			mutate: func(n *bot.Notice) {
				n.ReplyTo = "RESPONSE_URL"
				n.Update = "1700000000.000200"
			},
			wantMethod:  "response",
			wantReplace: true,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			stub := &stubSlack{}
			platform := newTestPlatform(t, stub, "stream")
			// Point the response URL at the stub, so the call is recorded like
			// any other.
			notice := noticeFor()
			test.mutate(&notice)
			if notice.ReplyTo == "RESPONSE_URL" {
				notice.ReplyTo = APIBase + "response"
			}

			if err := platform.Post(context.Background(), notice); err != nil {
				t.Fatalf("Post: %v", err)
			}
			calls := stub.recorded()
			if len(calls) != 1 {
				t.Fatalf("recorded %d calls, want 1: %+v", len(calls), calls)
			}
			call := calls[0]
			if call.method != test.wantMethod {
				t.Errorf("method = %q, want %q", call.method, test.wantMethod)
			}
			if test.wantThreadTS != nil && call.params["thread_ts"] != test.wantThreadTS {
				t.Errorf("thread_ts = %v, want %v", call.params["thread_ts"], test.wantThreadTS)
			}
			if test.wantReplace && call.params["replace_original"] != true {
				t.Errorf("replace_original = %v, want true", call.params["replace_original"])
			}
			if got := call.params["text"]; got != "hello" {
				t.Errorf("text = %v, want the notice's text", got)
			}
		})
	}
}

func TestPostRejectsAnEphemeralNoticeWithoutAUser(t *testing.T) {
	stub := &stubSlack{}
	platform := newTestPlatform(t, stub, "stream")

	err := platform.Post(context.Background(), bot.Notice{Channel: "C1", Ephemeral: true, Text: "hi"})
	if err == nil {
		t.Fatal("an ephemeral notice with no user was posted")
	}
	if len(stub.recorded()) != 0 {
		t.Error("a rejected notice reached the API")
	}
}

func TestPostSendsButtonsAsAnActionsBlock(t *testing.T) {
	stub := &stubSlack{}
	platform := newTestPlatform(t, stub, "stream")

	notice := noticeFor()
	notice.Ephemeral = true
	notice.Buttons = []bot.Button{
		{ActionID: bot.ActionResume, Text: "their-session", Value: "/sessions/a.jsonl"},
		{ActionID: bot.ActionResume, Text: "another", Value: "/sessions/b.jsonl", Style: "primary"},
	}
	if err := platform.Post(context.Background(), notice); err != nil {
		t.Fatalf("Post: %v", err)
	}

	call := stub.recorded()[0]
	blocks, ok := call.params["blocks"].([]any)
	if !ok || len(blocks) != 1 {
		t.Fatalf("blocks = %#v, want one actions block", call.params["blocks"])
	}
	actions := blocks[0].(map[string]any)
	if actions["type"] != "actions" {
		t.Fatalf("block type = %v, want actions", actions["type"])
	}
	elements := actions["elements"].([]any)
	if len(elements) != 2 {
		t.Fatalf("elements = %d, want 2", len(elements))
	}
	first := elements[0].(map[string]any)
	if first["action_id"] != bot.ActionResume || first["value"] != "/sessions/a.jsonl" {
		t.Errorf("first button = %#v", first)
	}
	if first["style"] != nil {
		t.Errorf("an unstyled button got a style: %#v", first)
	}
	if second := elements[1].(map[string]any); second["style"] != "primary" {
		t.Errorf("second button lost its style: %#v", second)
	}
	// A button's label is plain text, because Slack rejects markdown there.
	text := first["text"].(map[string]any)
	if text["type"] != "plain_text" {
		t.Errorf("button text type = %v, want plain_text", text["type"])
	}
}

func TestBlocksForCapsButtons(t *testing.T) {
	buttons := make([]bot.Button, buttonLimit+3)
	for i := range buttons {
		buttons[i] = bot.Button{ActionID: "x", Text: "b", Value: "v"}
	}
	blocks := blocksFor(buttons)
	elements := blocks[0]["elements"].([]block)
	if len(elements) != buttonLimit {
		t.Fatalf("rendered %d buttons, want Slack's cap of %d", len(elements), buttonLimit)
	}
	// A notice without buttons has no blocks: an empty actions block is
	// rejected by Slack.
	if got := blocksFor(nil); got != nil {
		t.Errorf("blocksFor(nil) = %#v, want nothing", got)
	}
}

func TestOpenThreadStartsAtTheNewMessagesTimestamp(t *testing.T) {
	stub := &stubSlack{}
	platform := newTestPlatform(t, stub, "stream")

	thread, err := platform.OpenThread(context.Background(), "C1", "Resuming something")
	if err != nil {
		t.Fatalf("OpenThread: %v", err)
	}
	// The workspace comes from the install, not from the caller: only the
	// adapter knows which workspace its token belongs to.
	if thread.Workspace != "T1" || thread.Channel != "C1" {
		t.Errorf("thread = %+v", thread)
	}
	if thread.ThreadTS != "1700000000.000001" {
		t.Errorf("thread root = %q, want the new message's ts", thread.ThreadTS)
	}
	call := stub.recorded()[0]
	if call.params["thread_ts"] != nil {
		t.Errorf("OpenThread posted into a thread: %+v", call.params)
	}
}

func TestProgressNamesTheReply(t *testing.T) {
	stub := &stubSlack{}
	r := newTestRenderer(t, stub, "stream")

	if err := r.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	// The core persists this so a restart can recognize the reply it left
	// half-written (DESIGN.md §7).
	if got, want := r.Progress(), "1700000000.000001"; got != want {
		t.Errorf("Progress() = %q, want %q", got, want)
	}
}

func TestRespondRejectsAnEmptyURL(t *testing.T) {
	stub := &stubSlack{}
	api := newStubAPI(t, stub)
	if err := api.Respond(context.Background(), "", "hi", nil, false); err == nil {
		t.Fatal("Respond accepted an empty response URL")
	}
}

func TestRespondAcceptsTheBareOK(t *testing.T) {
	// Slack documents the answer to a response_url POST as the bare text "ok",
	// while the rest of the Web API answers with an envelope. Neither shape may
	// be mistaken for the other.
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, "ok")
	}))
	defer server.Close()

	api := NewAPI("xoxb-test", discardLogger())
	if err := api.Respond(context.Background(), server.URL, "hi", nil, false); err != nil {
		t.Fatalf("Respond: %v", err)
	}
}

func TestRespondReportsAFailedStatusCode(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		fmt.Fprint(w, "invalid_url")
	}))
	defer server.Close()

	api := NewAPI("xoxb-test", discardLogger())
	err := api.Respond(context.Background(), server.URL, "hi", nil, false)
	if err == nil {
		t.Fatal("Respond accepted a 404, which is what an expired response URL looks like")
	}
	if !strings.Contains(err.Error(), "404") {
		t.Errorf("the error does not mention the status: %v", err)
	}
}
