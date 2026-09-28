package slack

import (
	"context"
	"encoding/json"
	"net/url"
	"strconv"
	"strings"
	"testing"
)

// The two read methods this build added for observation have to be sent as a
// form body. Both halves of that were live failures: Slack parses a JSON body
// for some methods and ignores it for others, so conversations.replies answered
// `invalid_arguments` and users.info answered `user_not_found` — for the bot's
// own user ID — while the requests themselves looked perfectly well formed. The
// effect was quiet: a turn read no history, and every name fell back to a bare
// ID.
func TestReadMethodsSendAFormBody(t *testing.T) {
	stub := &stubSlack{bodies: func(method string, _ map[string]any) string {
		switch method {
		case "conversations.replies":
			return repliesBody(replyJSON("1700000000.000100", "U1", "first"), false, "")
		case "users.info":
			return `{"ok":true,"user":{"profile":{"display_name":"Alice"}}}`
		}
		return ""
	}}
	api := newStubAPI(t, stub)

	if _, err := api.Replies(context.Background(), "C1", "1700000000.000100", ""); err != nil {
		t.Fatalf("Replies: %v", err)
	}
	if name, err := api.UserInfo(context.Background(), "U1"); err != nil || name != "Alice" {
		t.Fatalf("UserInfo = %q, %v; want Alice", name, err)
	}

	calls := stub.recorded()
	if len(calls) != 2 {
		t.Fatalf("calls = %v, want conversations.replies then users.info", stub.methods())
	}
	for _, call := range calls {
		if got := call.contentType; !strings.HasPrefix(got, "application/x-www-form-urlencoded") {
			t.Errorf("%s sent Content-Type %q, want a form body", call.method, got)
		}
	}
	// A number has to arrive as text: an int would be the JSON number 200, and
	// Slack's argument parser is what has to read it.
	if got := calls[0].params["limit"]; got != strconv.Itoa(replyPageSize) {
		t.Errorf("limit = %#v (%T), want %q as text", got, got, strconv.Itoa(replyPageSize))
	}
	// Slack reads an absent oldest as "from the root of the thread", which is
	// exactly what an empty watermark means.
	if _, sent := calls[0].params["oldest"]; sent {
		t.Error("an empty oldest was sent")
	}
	if got := calls[1].params["user"]; got != "U1" {
		t.Errorf("user = %v, want the ID to resolve", got)
	}
}

// A parameter that is a structure travels as a JSON string inside the form body,
// which is the shape Slack documents for a message's blocks and a suggestion's
// prompts.
func TestStructuredParamsTravelAsJSONStrings(t *testing.T) {
	encoded, err := encodeParams(map[string]any{
		"channel": "C1",
		"limit":   200,
		"empty":   "",
		"blocks":  []map[string]string{{"type": "actions"}},
	})
	if err != nil {
		t.Fatalf("encodeParams: %v", err)
	}
	form, err := url.ParseQuery(string(encoded))
	if err != nil {
		t.Fatalf("the body %q is not a form: %v", encoded, err)
	}
	if got := form.Get("limit"); got != "200" {
		t.Errorf("limit = %q, want 200 as text", got)
	}
	if got := form.Get("channel"); got != "C1" {
		t.Errorf("channel = %q", got)
	}
	var blocks []map[string]string
	if err := json.Unmarshal([]byte(form.Get("blocks")), &blocks); err != nil {
		t.Fatalf("blocks = %q, want a JSON string: %v", form.Get("blocks"), err)
	}
	if len(blocks) != 1 || blocks[0]["type"] != "actions" {
		t.Errorf("blocks = %#v", blocks)
	}
}

// A call with no arguments sends no body at all: auth.test and
// apps.connections.open take none, and an empty form is a different request.
func TestNoParamsMeansNoBody(t *testing.T) {
	encoded, err := encodeParams(nil)
	if err != nil {
		t.Fatalf("encodeParams: %v", err)
	}
	if len(encoded) != 0 {
		t.Errorf("encodeParams(nil) = %q, want nothing", encoded)
	}
}
