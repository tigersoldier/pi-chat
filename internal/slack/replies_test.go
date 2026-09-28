package slack

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/tigersoldier/pi-chat/internal/bot"
	"github.com/tigersoldier/pi-chat/internal/config"
)

// repliesBody builds one `conversations.replies` answer.
func repliesBody(messages string, hasMore bool, cursor string) string {
	return fmt.Sprintf(
		`{"ok":true,"messages":[%s],"has_more":%v,"response_metadata":{"next_cursor":%q}}`,
		messages, hasMore, cursor)
}

// replyJSON is one message in a page.
func replyJSON(ts, user, text string) string {
	return fmt.Sprintf(`{"ts":%q,"user":%q,"text":%q}`, ts, user, text)
}

// callsTo counts the requests the stub answered for one method.
func callsTo(stub *stubSlack, method string) int {
	n := 0
	for _, call := range stub.recorded() {
		if call.method == method {
			n++
		}
	}
	return n
}

func TestRepliesFollowsTheCursor(t *testing.T) {
	stub := &stubSlack{bodies: func(method string, params map[string]any) string {
		if method != "conversations.replies" {
			return ""
		}
		if params["cursor"] == "page-2" {
			return repliesBody(replyJSON("1700000000.000300", "U3", "third"), false, "")
		}
		return repliesBody(replyJSON("1700000000.000100", "U1", "first")+","+
			replyJSON("1700000000.000200", "U2", "second"), true, "page-2")
	}}
	api := newStubAPI(t, stub)

	got, err := api.Replies(context.Background(), "C1", "1700000000.000100", "1700000000.000050")
	if err != nil {
		t.Fatalf("Replies: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("messages = %d, want 3 across both pages", len(got))
	}
	if got[2].Text != "third" {
		t.Errorf("last message = %q, want the second page", got[2].Text)
	}

	calls := stub.recorded()
	if len(calls) != 2 {
		t.Fatalf("requests = %d, want one per page", len(calls))
	}
	if calls[0].params["ts"] != "1700000000.000100" {
		t.Errorf("the thread root = %v, want the thread timestamp", calls[0].params["ts"])
	}
	if calls[0].params["oldest"] != "1700000000.000050" {
		t.Errorf("oldest = %v, want the watermark", calls[0].params["oldest"])
	}
	if _, sent := calls[0].params["cursor"]; sent {
		t.Error("the first request carried a cursor")
	}
	if calls[1].params["cursor"] != "page-2" {
		t.Errorf("the second request's cursor = %v, want page-2", calls[1].params["cursor"])
	}
}

func TestRepliesStopsAtTheFetchLimit(t *testing.T) {
	// A page that always claims there is more: the fetch has to stop itself, and
	// the whole point of the cap is that a thread can be too long to read.
	page := make([]string, 0, replyPageSize)
	for i := range replyPageSize {
		page = append(page, replyJSON(fmt.Sprintf("1700000000.%06d", i), "U1", "x"))
	}
	body := repliesBody(strings.Join(page, ","), true, "more")
	stub := &stubSlack{bodies: func(method string, _ map[string]any) string {
		if method == "conversations.replies" {
			return body
		}
		return ""
	}}
	api := newStubAPI(t, stub)

	got, err := api.Replies(context.Background(), "C1", "1700000000.000100", "")
	if err != nil {
		t.Fatalf("Replies: %v", err)
	}
	if len(got) != replyFetchLimit {
		t.Errorf("messages = %d, want the %d cap", len(got), replyFetchLimit)
	}
	if n := callsTo(stub, "conversations.replies"); n != replyFetchLimit/replyPageSize {
		t.Errorf("requests = %d, want %d: paging should stop at the cap",
			n, replyFetchLimit/replyPageSize)
	}
}

func TestRepliesSurfacesAnError(t *testing.T) {
	stub := &stubSlack{failed: map[string]string{"conversations.replies": "missing_scope"}}
	api := newStubAPI(t, stub)

	if _, err := api.Replies(context.Background(), "C1", "1.1", ""); !IsCode(err, "missing_scope") {
		t.Fatalf("Replies error = %v, want missing_scope", err)
	}
}

// testPlatformWithIdentity builds a platform that knows which messages are its
// own, which is what a transcript's filtering needs.
func testPlatformWithIdentity(t *testing.T, stub *stubSlack, id Identity) *Platform {
	t.Helper()
	return NewPlatform(newStubAPI(t, stub), config.Defaults(), id, discardLogger())
}

func TestConversationLabelsPeopleAndMarksTheBotsOwnMessages(t *testing.T) {
	stub := &stubSlack{bodies: func(method string, params map[string]any) string {
		switch method {
		case "conversations.replies":
			return repliesBody(strings.Join([]string{
				replyJSON("1.1", "U2", "I think the test is wrong"),
				replyJSON("1.2", "U0", "I will look"),                 // ours, by user id
				`{"ts":"1.3","bot_id":"B2","text":"deployment done"}`, // another app
				`{"ts":"1.4","subtype":"channel_join","user":"U4","text":"joined"}`,
			}, ","), false, "")
		case "users.info":
			if params["user"] == "U2" {
				return `{"ok":true,"user":{"name":"alice","profile":{"display_name":"Alice","real_name":"Alice A"}}}`
			}
			return `{"ok":true,"user":{"name":"other"}}`
		}
		return ""
	}}
	platform := testPlatformWithIdentity(t, stub, Identity{TeamID: "T1", UserID: "U0", BotID: "B1"})

	said, err := platform.Conversation(context.Background(),
		bot.Thread{Workspace: "T1", Channel: "C1", ThreadTS: "1.1"}, "")
	if err != nil {
		t.Fatalf("Conversation: %v", err)
	}
	if len(said) != 3 {
		t.Fatalf("said = %d messages, want 3: a join is not part of the conversation", len(said))
	}
	if said[0].Name != "Alice" || said[0].UserID != "U2" || said[0].Text == "" {
		t.Errorf("first message = %+v, want Alice's, named", said[0])
	}
	if !said[1].FromBot {
		t.Error("the bot's own message was not marked as its own")
	}
	if said[2].FromBot || said[2].Name != "bot B2" {
		t.Errorf("another app = %+v, want it kept and named by bot ID", said[2])
	}
	if !strings.HasPrefix(said[2].TS, "1.3") {
		t.Errorf("message order changed: %+v", said)
	}
}

func TestConversationFallsBackToIDsWithoutTheUsersScope(t *testing.T) {
	stub := &stubSlack{
		failed: map[string]string{"users.info": "missing_scope"},
		bodies: func(method string, _ map[string]any) string {
			if method == "conversations.replies" {
				return repliesBody(replyJSON("1.1", "U2", "hello")+","+
					replyJSON("1.2", "U2", "again"), false, "")
			}
			return ""
		},
	}
	platform := testPlatformWithIdentity(t, stub, Identity{TeamID: "T1", UserID: "U0", BotID: "B1"})

	said, err := platform.Conversation(context.Background(),
		bot.Thread{Workspace: "T1", Channel: "C1", ThreadTS: "1.1"}, "")
	if err != nil {
		t.Fatalf("Conversation: %v", err)
	}
	for _, s := range said {
		if s.Name != "" {
			t.Errorf("name = %q, want the ID-only fallback", s.Name)
		}
		if s.UserID != "U2" {
			t.Errorf("user = %q, want the ID kept", s.UserID)
		}
	}
	// The refusal is the install's, not this message's: asking again for the
	// second message would double the cost of every transcript.
	if n := callsTo(stub, "users.info"); n != 1 {
		t.Errorf("users.info requests = %d, want 1 for the whole fetch", n)
	}
}
