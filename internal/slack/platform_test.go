package slack

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/tigersoldier/pi-chat/internal/bot"
	"github.com/tigersoldier/pi-chat/internal/config"
)

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// jsonParam reads a parameter that travels as a JSON string inside a form body —
// a message's blocks, a suggestion's prompts — back into the structure a test
// asserts on. That string is the shape Slack's form parser takes, so a
// parameter arriving as anything else would not be a request Slack could read.
func jsonParam(t *testing.T, params map[string]any, key string, out any) {
	t.Helper()
	raw, ok := params[key].(string)
	if !ok {
		t.Fatalf("%s = %#v, want a JSON string in the form body", key, params[key])
	}
	if err := json.Unmarshal([]byte(raw), out); err != nil {
		t.Fatalf("%s = %q, want JSON: %v", key, raw, err)
	}
}

// stubCall is one recorded Web API request.
type stubCall struct {
	method      string
	params      map[string]any
	auth        string
	contentType string
}

// stubSlack is a stand-in for Slack's Web API.
type stubSlack struct {
	mu     sync.Mutex
	calls  []stubCall
	failed map[string]string // method -> error code to answer with
	status int               // when set, the first request gets this HTTP status

	// policy, when set, decides per request which error to answer with; it
	// takes precedence over failed.
	policy func(method string, params map[string]any) string

	// bodies, when it returns a non-empty answer for a method, is the body sent
	// instead of the fixed shapes below. It is what tests that need data back
	// (conversations.replies, users.info) use.
	bodies func(method string, params map[string]any) string
}

// strictJSON names the methods that ignore a JSON body, with the error each
// answers when it gets one. Slack reports these as argument problems rather than
// as an unreadable body — conversations.replies says `invalid_arguments` and
// users.info answers `user_not_found` for a user that exists — which is what
// kept the mismatch quiet. The stub answers the same way, so a body Slack would
// ignore cannot pass a test and then fail in a workspace.
var strictJSON = map[string]string{
	"conversations.replies": "invalid_arguments",
	"users.info":            "user_not_found",
}

func (s *stubSlack) handler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		method := strings.TrimPrefix(r.URL.Path, "/")
		raw, _ := io.ReadAll(r.Body)
		contentType := r.Header.Get("Content-Type")

		// Slack reads arguments from a form body, and from a JSON body only for
		// the methods that accept one.
		params := map[string]any{}
		unread := ""
		switch {
		case strings.HasPrefix(contentType, "application/json"):
			if unread = strictJSON[method]; unread == "" && len(raw) > 0 {
				_ = json.Unmarshal(raw, &params)
			}
		case len(raw) > 0:
			if values, err := url.ParseQuery(string(raw)); err == nil {
				for key, value := range values {
					if len(value) > 0 {
						params[key] = value[0]
					}
				}
			}
		}

		s.mu.Lock()
		s.calls = append(s.calls, stubCall{
			method: method, params: params, auth: r.Header.Get("Authorization"), contentType: contentType,
		})
		code := s.failed[method]
		if s.policy != nil {
			code = s.policy(method, params)
		}
		status := s.status
		s.status = 0
		bodies := s.bodies
		s.mu.Unlock()

		w.Header().Set("Content-Type", "application/json")
		if unread != "" {
			fmt.Fprintf(w, `{"ok":false,"error":%q}`, unread)
			return
		}
		if status != 0 {
			w.Header().Set("Retry-After", "1")
			w.WriteHeader(status)
			fmt.Fprint(w, `{"ok":false,"error":"ratelimited"}`)
			return
		}
		if code != "" {
			fmt.Fprintf(w, `{"ok":false,"error":%q}`, code)
			return
		}
		// Called outside the lock: a test's body builder is its own code, and it
		// may look at what the stub has recorded so far.
		if bodies != nil {
			if body := bodies(method, params); body != "" {
				fmt.Fprint(w, body)
				return
			}
		}
		switch method {
		case "chat.postMessage", "chat.startStream":
			fmt.Fprint(w, `{"ok":true,"ts":"1700000000.000001"}`)
		default:
			fmt.Fprint(w, `{"ok":true}`)
		}
	}
}

func (s *stubSlack) recorded() []stubCall {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]stubCall(nil), s.calls...)
}

func (s *stubSlack) methods() []string {
	var out []string
	for _, call := range s.recorded() {
		out = append(out, call.method)
	}
	return out
}

// newStubAPI points the package's API client at a stub server.
func newStubAPI(t *testing.T, stub *stubSlack) *API {
	t.Helper()
	server := httptest.NewServer(stub.handler())
	t.Cleanup(server.Close)
	previous := APIBase
	APIBase = server.URL + "/"
	t.Cleanup(func() { APIBase = previous })
	return NewAPI("xoxb-test", discardLogger())
}

// testMessage is the inbound message a renderer needs: a channel thread.
func testMessage() bot.Message {
	return bot.Message{
		Thread:    bot.Thread{Workspace: "T1", Channel: "C1", ThreadTS: "1700000000.000100"},
		UserID:    "U1",
		Workspace: "T1",
	}
}

func newTestPlatform(t *testing.T, stub *stubSlack, mode string) *Platform {
	t.Helper()
	cfg := config.Defaults()
	cfg.Render.Mode = mode
	return NewPlatform(newStubAPI(t, stub), cfg, Identity{TeamID: "T1"}, discardLogger())
}

func TestAcknowledgeRequestReactsToOriginalMessage(t *testing.T) {
	for _, tc := range []struct {
		allowed  bool
		reaction string
	}{
		{allowed: true, reaction: "eyes"},
		{allowed: false, reaction: "shrug"},
	} {
		stub := &stubSlack{}
		platform := newTestPlatform(t, stub, "patch")
		message := testMessage()
		message.TS = "1700000000.000200"

		if err := platform.AcknowledgeRequest(context.Background(), message, tc.allowed); err != nil {
			t.Fatalf("AcknowledgeRequest(allowed=%v): %v", tc.allowed, err)
		}

		calls := stub.recorded()
		if len(calls) != 1 || calls[0].method != "reactions.add" {
			t.Fatalf("calls = %+v, want one reactions.add", calls)
		}
		params := calls[0].params
		if params["channel"] != message.Thread.Channel || params["timestamp"] != message.TS || params["name"] != tc.reaction {
			t.Errorf("reactions.add params = %+v, want channel=%q timestamp=%q name=%q",
				params, message.Thread.Channel, message.TS, tc.reaction)
		}
	}
}

func TestQueueRequestAddsQueueMarkerWhenEyesRemovalFails(t *testing.T) {
	stub := &stubSlack{failed: map[string]string{"reactions.remove": "missing_scope"}}
	platform := newTestPlatform(t, stub, "patch")
	message := testMessage()
	message.TS = "1700000000.000200"

	if err := platform.QueueRequest(context.Background(), message); !IsCode(err, "missing_scope") {
		t.Fatalf("QueueRequest error = %v, want missing_scope", err)
	}
	calls := stub.recorded()
	if len(calls) != 2 || calls[0].method != "reactions.remove" || calls[0].params["name"] != "eyes" ||
		calls[1].method != "reactions.add" || calls[1].params["name"] != "hourglass_flowing_sand" {
		t.Fatalf("calls = %+v, want queued marker attempt despite failed eyes removal", calls)
	}
}

func TestStartRequestAddsEyesWhenNotQueued(t *testing.T) {
	const currentTS = "1700000000.000300"
	stub := &stubSlack{bodies: func(method string, _ map[string]any) string {
		if method == "conversations.replies" {
			return repliesBody(`{"ts":"`+currentTS+`","user":"U1"}`, false, "")
		}
		return ""
	}}
	platform := testPlatformWithIdentity(t, stub, Identity{TeamID: "T1", UserID: "UBOT"})
	message := testMessage()
	message.TS = currentTS

	if err := platform.StartRequest(context.Background(), message, false); err != nil {
		t.Fatalf("StartRequest: %v", err)
	}
	calls := stub.recorded()
	if len(calls) < 2 || calls[0].method != "reactions.add" || calls[0].params["timestamp"] != currentTS || calls[0].params["name"] != "eyes" {
		t.Fatalf("calls = %+v, want eyes added before the history scan", calls)
	}
}

func TestQueuedRequestLifecycleCleansOwnStaleReactions(t *testing.T) {
	const replies = `{"ok":true,"messages":[` +
		`{"ts":"1700000000.000100","user":"U1","reactions":[` +
		`{"name":"eyes","users":["UBOT","U2"]},` +
		`{"name":"hourglass_flowing_sand","users":["UBOT"]},` +
		`{"name":"shrug","users":["UBOT"]},` +
		`{"name":"white_check_mark","users":["UBOT"]},` +
		`{"name":"warning","users":["UBOT"]},` +
		`{"name":"heart","users":["U2"]}]},` +
		`{"ts":"1700000000.000150","user":"UBOT","bot_id":"BPI","reactions":[{"name":"eyes","users":["UBOT"]}]},` +
		`{"ts":"1700000000.000200","user":"U2","reactions":[{"name":"eyes","users":["U2"]}]},` +
		`{"ts":"1700000000.000300","user":"U1","reactions":[{"name":"hourglass_flowing_sand","users":["UBOT"]}]}]}`
	stub := &stubSlack{bodies: func(method string, _ map[string]any) string {
		if method == "conversations.replies" {
			return replies
		}
		return ""
	}}
	cfg := config.Defaults()
	platform := NewPlatform(newStubAPI(t, stub), cfg,
		Identity{TeamID: "T1", UserID: "UBOT", BotID: "BPI"}, discardLogger())
	message := testMessage()
	message.TS = "1700000000.000300"

	if err := platform.AcknowledgeRequest(context.Background(), message, true); err != nil {
		t.Fatalf("AcknowledgeRequest: %v", err)
	}
	if err := platform.QueueRequest(context.Background(), message); err != nil {
		t.Fatalf("QueueRequest: %v", err)
	}
	if err := platform.StartRequest(context.Background(), message, true); err != nil {
		t.Fatalf("StartRequest: %v", err)
	}
	if err := platform.CompleteRequest(context.Background(), message); err != nil {
		t.Fatalf("CompleteRequest: %v", err)
	}

	calls := stub.recorded()
	want := []struct {
		method string
		name   string
		ts     string
	}{
		{"reactions.add", "eyes", message.TS},
		{"reactions.remove", "eyes", message.TS},
		{"reactions.add", "hourglass_flowing_sand", message.TS},
		{"reactions.remove", "hourglass_flowing_sand", message.TS},
		{"reactions.add", "eyes", message.TS},
		{"conversations.replies", "", ""},
		{"reactions.remove", "eyes", "1700000000.000100"},
		{"reactions.remove", "hourglass_flowing_sand", "1700000000.000100"},
		{"reactions.remove", "shrug", "1700000000.000100"},
		{"reactions.remove", "white_check_mark", "1700000000.000100"},
		{"reactions.remove", "warning", "1700000000.000100"},
		{"reactions.remove", "eyes", message.TS},
		{"reactions.remove", "hourglass_flowing_sand", message.TS},
		{"reactions.add", "white_check_mark", message.TS},
	}
	if len(calls) != len(want) {
		t.Fatalf("calls = %+v, want %d lifecycle calls", calls, len(want))
	}
	for i, expected := range want {
		if calls[i].method != expected.method {
			t.Errorf("call %d method = %q, want %q", i, calls[i].method, expected.method)
		}
		params := calls[i].params
		if expected.method == "conversations.replies" {
			if params["channel"] != message.Thread.Channel || params["ts"] != message.Thread.ThreadTS {
				t.Errorf("replies params = %+v, want the request's thread", params)
			}
			continue
		}
		if params["timestamp"] != expected.ts || params["name"] != expected.name {
			t.Errorf("call %d params = %+v, want timestamp=%q name=%q", i, params, expected.ts, expected.name)
		}
	}
}

func TestStartRequestFindsAnchorBeyondTranscriptReplyCap(t *testing.T) {
	pages := make(map[string]string)
	for page := range 4 {
		messages := make([]string, 0, replyPageSize)
		first := page*replyPageSize + 1
		last := min(first+replyPageSize-1, 605)
		for i := first; i <= last; i++ {
			ts := fmt.Sprintf("1700000000.%06d", i)
			var reactions string
			switch i {
			case 2:
				reactions = `,"reactions":[{"name":"eyes","users":["UBOT"]}]`
			case 602:
				reactions = `,"reactions":[{"name":"white_check_mark","users":["UBOT"]}]`
			case 603:
				reactions = `,"reactions":[{"name":"warning","users":["UBOT"]}]`
			case 604:
				reactions = `,"reactions":[{"name":"shrug","users":["UBOT"]}]`
			case 605:
				reactions = `,"reactions":[{"name":"warning","users":["UBOT"]}]`
			}
			messages = append(messages, fmt.Sprintf(`{"ts":%q,"user":"U1"%s}`, ts, reactions))
		}
		cursor := ""
		if page > 0 {
			cursor = fmt.Sprintf("page-%d", page)
		}
		nextCursor := ""
		if page < 3 {
			nextCursor = fmt.Sprintf("page-%d", page+1)
		}
		pages[cursor] = repliesBody(strings.Join(messages, ","), page < 3, nextCursor)
	}
	stub := &stubSlack{bodies: func(method string, params map[string]any) string {
		if method != "conversations.replies" {
			return ""
		}
		cursor, _ := params["cursor"].(string)
		return pages[cursor]
	}}
	platform := testPlatformWithIdentity(t, stub, Identity{TeamID: "T1", UserID: "UBOT"})
	message := testMessage()
	message.Thread.ThreadTS = "1700000000.000001"
	message.TS = "1700000000.000605"

	if err := platform.StartRequest(context.Background(), message, false); err != nil {
		t.Fatalf("StartRequest: %v", err)
	}
	calls := stub.recorded()
	if got := callsTo(stub, "conversations.replies"); got != 4 {
		t.Fatalf("conversations.replies calls = %d, want four pages through the current request", got)
	}
	want := []struct{ ts, name string }{
		{"1700000000.000002", "eyes"},
		{"1700000000.000602", "white_check_mark"},
		{"1700000000.000603", "warning"},
		{"1700000000.000604", "shrug"},
	}
	removals := 0
	for _, call := range calls {
		if call.method != "reactions.remove" {
			continue
		}
		if removals >= len(want) {
			t.Fatalf("unexpected removal: %+v", call)
		}
		if call.params["timestamp"] != want[removals].ts || call.params["name"] != want[removals].name {
			t.Errorf("removal %d = %+v, want timestamp=%q name=%q", removals, call.params, want[removals].ts, want[removals].name)
		}
		removals++
	}
	if removals != len(want) {
		t.Fatalf("reaction removals = %d, want %d", removals, len(want))
	}
}

func TestStartRequestCleansDMRootsBeyondOneHistoryPage(t *testing.T) {
	historyPages := make(map[string]string)
	for page := range 3 {
		messages := make([]string, 0, historyPageSize)
		newest := 499 - page*historyPageSize
		oldest := max(newest-historyPageSize+1, 1)
		for i := newest; i >= oldest; i-- {
			ts := fmt.Sprintf("1700000000.%06d", i)
			user := "U1"
			botID := ""
			var reactions string
			switch i {
			case 475:
				user, botID = "U2", "B2"
				reactions = `,"reactions":[{"name":"warning","users":["UBOT"]}]`
			case 450:
				user, botID = "UBOT", "BPI"
				reactions = `,"reactions":[{"name":"eyes","users":["UBOT"]}]`
			case 250:
				reactions = `,"reactions":[{"name":"white_check_mark","users":["UBOT"]}]`
			case 50:
				reactions = `,"reactions":[{"name":"warning","users":["UBOT"]}]`
			case 25:
				reactions = `,"reactions":[{"name":"shrug","users":["U2"]}]`
			}
			messages = append(messages, fmt.Sprintf(`{"ts":%q,"user":%q,"bot_id":%q%s}`, ts, user, botID, reactions))
		}
		cursor := ""
		if page > 0 {
			cursor = fmt.Sprintf("history-%d", page)
		}
		nextCursor := ""
		if page < 2 {
			nextCursor = fmt.Sprintf("history-%d", page+1)
		}
		historyPages[cursor] = fmt.Sprintf(`{"ok":true,"messages":[%s],"has_more":%t,"response_metadata":{"next_cursor":%q}}`,
			strings.Join(messages, ","), page < 2, nextCursor)
	}
	stub := &stubSlack{bodies: func(method string, params map[string]any) string {
		switch method {
		case "conversations.replies":
			return repliesBody(`{"ts":"1700000000.000500","user":"U1","reactions":[{"name":"warning","users":["UBOT"]}]}`, false, "")
		case "conversations.history":
			cursor, _ := params["cursor"].(string)
			return historyPages[cursor]
		}
		return ""
	}}
	platform := testPlatformWithIdentity(t, stub, Identity{TeamID: "T1", UserID: "UBOT", BotID: "BPI"})
	message := testMessage()
	message.TS = "1700000000.000500"
	message.Thread.ThreadTS = message.TS
	message.Direct = true

	if err := platform.StartRequest(context.Background(), message, false); err != nil {
		t.Fatalf("StartRequest: %v", err)
	}
	calls := stub.recorded()
	var historyCalls []stubCall
	for _, call := range calls {
		if call.method == "conversations.history" {
			historyCalls = append(historyCalls, call)
		}
	}
	if len(historyCalls) != 3 {
		t.Fatalf("history pages = %d, want all three pages of earlier DM roots", len(historyCalls))
	}
	for i, call := range historyCalls {
		if call.params["channel"] != message.Thread.Channel || call.params["latest"] != message.TS || call.params["limit"] != "200" {
			t.Errorf("history page %d params = %+v, want the channel and current-message anchor", i, call.params)
		}
		wantCursor := ""
		if i > 0 {
			wantCursor = fmt.Sprintf("history-%d", i)
		}
		cursor, _ := call.params["cursor"].(string)
		if cursor != wantCursor {
			t.Errorf("history page %d cursor = %q, want %q", i, cursor, wantCursor)
		}
	}
	want := []struct{ ts, name string }{
		{"1700000000.000250", "white_check_mark"},
		{"1700000000.000050", "warning"},
	}
	removals := 0
	for _, call := range calls {
		if call.method != "reactions.remove" {
			continue
		}
		if removals >= len(want) {
			t.Fatalf("unexpected reaction removal: %+v", call)
		}
		if call.params["timestamp"] != want[removals].ts || call.params["name"] != want[removals].name {
			t.Errorf("removal %d = %+v, want timestamp=%q name=%q", removals, call.params, want[removals].ts, want[removals].name)
		}
		removals++
	}
	if removals != len(want) {
		t.Fatalf("reaction removals = %d, want %d", removals, len(want))
	}
}

func TestCompleteRequestReactsToOriginalMessage(t *testing.T) {
	stub := &stubSlack{}
	platform := newTestPlatform(t, stub, "patch")
	message := testMessage()
	message.TS = "1700000000.000200"

	if err := platform.CompleteRequest(context.Background(), message); err != nil {
		t.Fatalf("CompleteRequest: %v", err)
	}
	calls := stub.recorded()
	if len(calls) != 3 || calls[0].method != "reactions.remove" || calls[1].method != "reactions.remove" || calls[2].method != "reactions.add" {
		t.Fatalf("calls = %+v, want removal of both in-progress reactions followed by checkmark", calls)
	}
	for i, want := range []string{"eyes", "hourglass_flowing_sand", "white_check_mark"} {
		params := calls[i].params
		if params["channel"] != message.Thread.Channel || params["timestamp"] != message.TS || params["name"] != want {
			t.Errorf("%s params = %+v, want channel=%q timestamp=%q name=%q",
				calls[i].method, params, message.Thread.Channel, message.TS, want)
		}
	}
}

func TestCompleteRequestAddsCheckWhenEyesAreAlreadyAbsent(t *testing.T) {
	stub := &stubSlack{failed: map[string]string{"reactions.remove": "no_reaction"}}
	platform := newTestPlatform(t, stub, "patch")
	message := testMessage()
	message.TS = "1700000000.000200"

	if err := platform.CompleteRequest(context.Background(), message); err != nil {
		t.Fatalf("CompleteRequest without an eyes reaction: %v", err)
	}
	calls := stub.recorded()
	if len(calls) != 3 || calls[0].method != "reactions.remove" || calls[0].params["name"] != "eyes" ||
		calls[1].method != "reactions.remove" || calls[1].params["name"] != "hourglass_flowing_sand" ||
		calls[2].method != "reactions.add" || calls[2].params["name"] != "white_check_mark" {
		t.Fatalf("calls = %+v, want both absent in-progress reactions tolerated before adding checkmark", calls)
	}
}

func TestCompleteRequestDoesNotAddCheckWhenEyesCannotBeRemoved(t *testing.T) {
	stub := &stubSlack{failed: map[string]string{"reactions.remove": "missing_scope"}}
	platform := newTestPlatform(t, stub, "patch")
	message := testMessage()
	message.TS = "1700000000.000200"

	err := platform.CompleteRequest(context.Background(), message)
	if !IsCode(err, "missing_scope") {
		t.Fatalf("CompleteRequest error = %v, want missing_scope", err)
	}
	calls := stub.recorded()
	if len(calls) != 1 || calls[0].method != "reactions.remove" {
		t.Fatalf("calls = %+v, want no checkmark unless eyes were removed", calls)
	}
}

func TestFailRequestReplacesEyesWithWarning(t *testing.T) {
	stub := &stubSlack{}
	platform := newTestPlatform(t, stub, "patch")
	message := testMessage()
	message.TS = "1700000000.000200"

	if err := platform.FailRequest(context.Background(), message); err != nil {
		t.Fatalf("FailRequest: %v", err)
	}
	calls := stub.recorded()
	if len(calls) != 3 || calls[0].method != "reactions.remove" || calls[0].params["name"] != "eyes" ||
		calls[1].method != "reactions.remove" || calls[1].params["name"] != "hourglass_flowing_sand" ||
		calls[2].method != "reactions.add" || calls[2].params["name"] != "warning" {
		t.Fatalf("calls = %+v, want both in-progress reactions removed before adding warning", calls)
	}
}

func TestFinalOnlyPostStaysWithinSlackMarkdownTextLimit(t *testing.T) {
	stub := &stubSlack{}
	r := newTestRenderer(t, stub, "stream")
	answer := strings.Repeat("x", maxMessage+1)

	if err := r.Finish(context.Background(), answer); err != nil {
		t.Fatalf("Finish: %v", err)
	}
	calls := stub.recorded()
	if len(calls) != 1 || calls[0].method != "chat.postMessage" {
		t.Fatalf("calls = %+v, want one final post", calls)
	}
	markdownText := calls[0].params["markdown_text"].(string)
	if got := utf8.RuneCountInString(markdownText); got > maxMessage {
		t.Errorf("posted markdown_text has %d characters, exceeds Slack's %d-character limit", got, maxMessage)
	}
	if got := len(markdownText); got > maxMessage {
		t.Errorf("posted markdown_text has %d bytes, exceeds the conservative %d-byte budget", got, maxMessage)
	}
}

func TestFinishWithoutStartPostsTheReadyAnswer(t *testing.T) {
	stub := &stubSlack{}
	r := newTestRenderer(t, stub, "stream")
	answer := "**The answer is ready.**\n\n[Documentation](https://example.com)"

	if err := r.Finish(context.Background(), answer); err != nil {
		t.Fatalf("Finish: %v", err)
	}
	calls := stub.recorded()
	if len(calls) != 1 || calls[0].method != "chat.postMessage" {
		t.Fatalf("calls = %+v, want one post only after final text is ready", calls)
	}
	params := calls[0].params
	if params["channel"] != r.channel || params["thread_ts"] != r.threadTS || params["markdown_text"] != answer || params["text"] != nil || params["blocks"] != nil {
		t.Errorf("posted answer params = %+v, want channel=%q thread_ts=%q markdown_text=%q",
			params, r.channel, r.threadTS, answer)
	}
	for _, call := range calls {
		if call.method == "chat.startStream" || call.method == "chat.update" {
			t.Errorf("final-only answer unexpectedly started or patched an empty reply: calls=%+v", calls)
		}
	}
}

func newTestRenderer(t *testing.T, stub *stubSlack, mode string) *renderer {
	t.Helper()
	return testRenderer(t, newTestPlatform(t, stub, mode))
}

// testRenderer starts one turn's renderer on an existing platform, so a test
// can follow what a platform remembers across turns.
func testRenderer(t *testing.T, platform *Platform) *renderer {
	t.Helper()
	r, err := platform.StartTurn(context.Background(), testMessage())
	if err != nil {
		t.Fatal(err)
	}
	return r.(*renderer)
}

func TestStartStreamPassesTheRecipient(t *testing.T) {
	stub := &stubSlack{}
	r := newTestRenderer(t, stub, "stream")

	if err := r.Start(context.Background()); err != nil {
		t.Fatal(err)
	}

	calls := stub.recorded()
	if len(calls) != 1 || calls[0].method != "chat.startStream" {
		t.Fatalf("calls = %v, want one chat.startStream", stub.methods())
	}
	params := calls[0].params
	if params["task_display_mode"] != "timeline" {
		t.Fatalf("task_display_mode = %v, want timeline", params["task_display_mode"])
	}
	var initial []streamPayloadChunk
	jsonParam(t, params, "chunks", &initial)
	if len(initial) != 0 {
		t.Fatalf("text-first stream starts with chunks %v, want an empty chunks array", initial)
	}
	for _, key := range []string{"channel", "thread_ts", "recipient_user_id", "recipient_team_id"} {
		if params[key] == nil {
			t.Fatalf("chat.startStream is missing %s: %v", key, params)
		}
	}
	if calls[0].auth != "Bearer xoxb-test" {
		t.Fatalf("auth header = %q", calls[0].auth)
	}
}

func TestStartStreamRetriesWithoutARecipient(t *testing.T) {
	// Slack documents the recipient as required when streaming to channels,
	// while pi-chat always streams into a thread. If a given thread refuses the
	// field, the answer still streams instead of degrading to a patched message.
	stub := &stubSlack{policy: func(method string, params map[string]any) string {
		if method == "chat.startStream" && params["recipient_user_id"] != nil {
			return "invalid_arguments"
		}
		return ""
	}}
	platform := newTestPlatform(t, stub, "stream")
	ctx := context.Background()

	first := testRenderer(t, platform)
	if err := first.Start(ctx); err != nil {
		t.Fatal(err)
	}
	if first.streamState != streamActive {
		t.Fatalf("stream state = %v, want active", first.streamState)
	}

	// The refusal is remembered, so the next turn does not repeat a doomed
	// call. A per-turn retry would double the API traffic of every answer.
	if err := testRenderer(t, platform).Start(ctx); err != nil {
		t.Fatal(err)
	}

	var withRecipient, withoutRecipient int
	for _, call := range stub.recorded() {
		switch {
		case call.method == "chat.postMessage":
			t.Fatal("it should not have fallen back to patching")
		case call.method != "chat.startStream":
		case call.params["recipient_user_id"] != nil:
			withRecipient++
		default:
			withoutRecipient++
		}
	}
	if withRecipient != 1 || withoutRecipient != 2 {
		t.Fatalf("startStream attempts: %d with a recipient, %d without; want 1 and 2",
			withRecipient, withoutRecipient)
	}
}

func TestStreamingAppendsAndFinishes(t *testing.T) {
	stub := &stubSlack{}
	r := newTestRenderer(t, stub, "stream")
	ctx := context.Background()

	if err := r.Start(ctx); err != nil {
		t.Fatal(err)
	}
	if err := r.Delta(ctx, "hello"); err != nil {
		t.Fatal(err)
	}
	if err := r.Finish(ctx, "hello"); err != nil {
		t.Fatal(err)
	}

	got := stub.methods()
	want := []string{"chat.startStream", "chat.appendStream", "chat.stopStream"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("calls = %v, want %v", got, want)
	}
	var chunks []streamPayloadChunk
	jsonParam(t, stub.recorded()[1].params, "chunks", &chunks)
	if len(chunks) != 1 || chunks[0].Type != "markdown_text" || chunks[0].Text != "hello" {
		t.Fatalf("appended chunks = %+v, want one markdown_text chunk with hello", chunks)
	}
}

func TestTaskUpdatesShareTheReplyStream(t *testing.T) {
	stub := &stubSlack{}
	r := newTestRenderer(t, stub, "stream")
	ctx := context.Background()

	progress := bot.TaskProgress{ID: "tool-call-1", Title: "Running a command", Status: bot.TaskInProgress}
	if err := r.UpdateTasks(ctx, []bot.TaskProgress{progress}); err != nil {
		t.Fatal(err)
	}
	if err := r.Start(ctx); err != nil {
		t.Fatal(err)
	}
	if err := r.Delta(ctx, "hello"); err != nil {
		t.Fatal(err)
	}
	progress.Status = bot.TaskComplete
	progress.Details = "Completed actions:\n• Running a command"
	other := bot.TaskProgress{ID: "tool-call-2", Title: "Reading files", Status: bot.TaskInProgress}
	if err := r.UpdateTasks(ctx, []bot.TaskProgress{progress, other}); err != nil {
		t.Fatal(err)
	}
	if err := r.Finish(ctx, "hello"); err != nil {
		t.Fatal(err)
	}

	calls := stub.recorded()
	want := []string{"chat.startStream", "chat.appendStream", "chat.appendStream", "chat.stopStream"}
	if got := stub.methods(); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("calls = %v, want %v", got, want)
	}
	var initial, text, complete []streamPayloadChunk
	jsonParam(t, calls[0].params, "chunks", &initial)
	jsonParam(t, calls[1].params, "chunks", &text)
	jsonParam(t, calls[2].params, "chunks", &complete)
	if len(initial) != 1 || initial[0].Type != "task_update" || initial[0].ID != progress.ID || initial[0].Status != string(bot.TaskInProgress) {
		t.Fatalf("initial task chunks = %+v", initial)
	}
	if len(text) != 1 || text[0].Type != "markdown_text" || text[0].Text != "hello" {
		t.Fatalf("text chunks = %+v", text)
	}
	if got := calls[0].params["task_display_mode"]; got != "timeline" {
		t.Fatalf("task_display_mode = %v, want timeline", got)
	}
	if len(complete) != 2 || complete[0].Type != "task_update" || complete[0].ID != progress.ID || complete[0].Status != string(bot.TaskComplete) || complete[0].Details != progress.Details || complete[1].Type != "task_update" || complete[1].ID != other.ID || complete[1].Status != string(bot.TaskInProgress) {
		t.Fatalf("batched task chunks = %+v", complete)
	}
}

func TestTaskAppendFailureFallsBackToMessageUpdates(t *testing.T) {
	stub := &stubSlack{policy: func(method string, params map[string]any) string {
		if method != "chat.appendStream" {
			return ""
		}
		var chunks []streamPayloadChunk
		raw, ok := params["chunks"].(string)
		if !ok || json.Unmarshal([]byte(raw), &chunks) != nil {
			return "invalid_chunks"
		}
		if len(chunks) > 0 && chunks[0].Type == "task_update" {
			return "invalid_chunks"
		}
		return ""
	}}
	r := newTestRenderer(t, stub, "stream")
	ctx := context.Background()
	progress := bot.TaskProgress{ID: "tool-call-1", Title: "Reading files", Status: bot.TaskInProgress}
	if err := r.UpdateTasks(ctx, []bot.TaskProgress{progress}); err != nil {
		t.Fatal(err)
	}
	progress.Status = bot.TaskComplete
	if err := r.UpdateTasks(ctx, []bot.TaskProgress{progress}); err == nil {
		t.Fatal("a refused task append should be reported")
	}
	if err := r.Delta(ctx, "answer"); err != nil {
		t.Fatal(err)
	}
	if err := r.Finish(ctx, "answer"); err != nil {
		t.Fatal(err)
	}

	want := []string{"chat.startStream", "chat.appendStream", "chat.stopStream", "chat.update"}
	if got := stub.methods(); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("calls = %v, want stream closure before message-update fallback", got)
	}
}

func TestTaskStreamFailureDefersFallbackUntilText(t *testing.T) {
	stub := &stubSlack{failed: map[string]string{"chat.startStream": "invalid_thread_ts"}}
	r := newTestRenderer(t, stub, "stream")
	ctx := context.Background()
	progress := bot.TaskProgress{ID: "tool-call-1", Title: "Reading files", Status: bot.TaskInProgress}

	if err := r.UpdateTasks(ctx, []bot.TaskProgress{progress}); err == nil {
		t.Fatal("a refused task stream should be reported to the best-effort caller")
	}
	if got := stub.methods(); strings.Join(got, ",") != "chat.startStream,chat.startStream" {
		t.Fatalf("task update created a fallback reply before text: calls = %v", got)
	}
	if err := r.Start(ctx); err != nil {
		t.Fatal(err)
	}
	if err := r.Delta(ctx, "answer"); err != nil {
		t.Fatal(err)
	}
	if got := stub.methods(); strings.Join(got, ",") != "chat.startStream,chat.startStream,chat.postMessage,chat.update" {
		t.Fatalf("fallback calls = %v", got)
	}
}

func TestFinishCorrectsADivergentAnswer(t *testing.T) {
	stub := &stubSlack{}
	r := newTestRenderer(t, stub, "stream")
	ctx := context.Background()

	if err := r.Start(ctx); err != nil {
		t.Fatal(err)
	}
	if err := r.Delta(ctx, "partial"); err != nil {
		t.Fatal(err)
	}
	// The authoritative answer differs from what streamed, so the message is
	// corrected instead of left truncated.
	if err := r.Finish(ctx, "partial, with more"); err != nil {
		t.Fatal(err)
	}

	got := strings.Join(stub.methods(), ",")
	if got != "chat.startStream,chat.appendStream,chat.stopStream,chat.update" {
		t.Fatalf("calls = %v", got)
	}
	if text := stub.recorded()[3].params["markdown_text"]; text != "partial, with more" {
		t.Fatalf("corrected text = %v", text)
	}
}

func TestStreamFailureFallsBackToPatching(t *testing.T) {
	stub := &stubSlack{failed: map[string]string{"chat.startStream": "invalid_thread_ts"}}
	r := newTestRenderer(t, stub, "stream")
	ctx := context.Background()

	if err := r.Start(ctx); err != nil {
		t.Fatalf("a refused stream should still produce a reply: %v", err)
	}
	if err := r.Delta(ctx, "hello"); err != nil {
		t.Fatal(err)
	}
	if err := r.Finish(ctx, "hello"); err != nil {
		t.Fatal(err)
	}

	got := strings.Join(stub.methods(), ",")
	// Two attempts at streaming (with a recipient, then without), then the
	// patched-message fallback.
	want := "chat.startStream,chat.startStream,chat.postMessage,chat.update,chat.update"
	if got != want {
		t.Fatalf("calls = %v, want %v", got, want)
	}
	if text := stub.recorded()[3].params["markdown_text"]; text != "hello" {
		t.Fatalf("patched text = %v, want \"hello\"", text)
	}
}

func TestAppendFailureMidTurnDefersFallbackUntilSettlement(t *testing.T) {
	stub := &stubSlack{}
	r := newTestRenderer(t, stub, "stream")
	ctx := context.Background()

	if err := r.Start(ctx); err != nil {
		t.Fatal(err)
	}
	stub.mu.Lock()
	stub.failed = map[string]string{"chat.appendStream": "ratelimited"}
	stub.mu.Unlock()

	if err := r.Delta(ctx, "hello"); err != nil {
		t.Fatalf("a failed append should be retained for settlement: %v", err)
	}
	if got := strings.Join(stub.methods(), ","); got != "chat.startStream,chat.appendStream" {
		t.Fatalf("calls after ambiguous append = %v, want no unsafe update", got)
	}
	if r.ts == "" || r.streamState != streamUnknown {
		t.Fatalf("append failure lost stream handle/state: ts=%q state=%v", r.ts, r.streamState)
	}
	if err := r.Finish(ctx, "hello"); err != nil {
		t.Fatal(err)
	}
	want := "chat.startStream,chat.appendStream,chat.stopStream,chat.update"
	if got := strings.Join(stub.methods(), ","); got != want {
		t.Fatalf("calls = %v, want closure before fallback", got)
	}
}

func TestClosedMarkdownStreamIsRepairedWithMarkdownText(t *testing.T) {
	answer := "**What exists now:**\n\n- See [the guide](https://example.com)."
	stub := &stubSlack{failed: map[string]string{"chat.appendStream": "message_not_in_streaming_state"}}
	r := newTestRenderer(t, stub, "stream")
	ctx := context.Background()

	if err := r.Start(ctx); err != nil {
		t.Fatal(err)
	}
	if err := r.Delta(ctx, answer); err != nil {
		t.Fatal(err)
	}
	if r.streamState != streamClosed || !r.streamTextIncomplete {
		t.Fatalf("stream state = %v, incomplete = %v; want closed and incomplete", r.streamState, r.streamTextIncomplete)
	}
	if err := r.Finish(ctx, answer); err != nil {
		t.Fatal(err)
	}

	calls := stub.recorded()
	if got, want := strings.Join(stub.methods(), ","), "chat.startStream,chat.appendStream,chat.update,chat.update"; got != want {
		t.Fatalf("calls = %s, want %s", got, want)
	}
	for _, call := range calls {
		if call.method != "chat.update" {
			continue
		}
		if got := call.params["markdown_text"]; got != answer {
			t.Errorf("repair markdown_text = %v, want the original Markdown", got)
		}
		if _, hasText := call.params["text"]; hasText {
			t.Errorf("Markdown repair also sent the incompatible text field: %+v", call.params)
		}
		if _, hasBlocks := call.params["blocks"]; hasBlocks {
			t.Errorf("Markdown repair also sent the incompatible blocks field: %+v", call.params)
		}
	}
}

func TestMessageTooLongAppendLogsSizeAndRepairsFinal(t *testing.T) {
	var logs bytes.Buffer
	log := slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	stub := &stubSlack{failed: map[string]string{"chat.appendStream": "msg_too_long"}}
	r := newTestRenderer(t, stub, "stream")
	r.log = log
	ctx := context.Background()

	if err := r.Start(ctx); err != nil {
		t.Fatal(err)
	}
	if err := r.Delta(ctx, "private partial"); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(stub.methods(), ","); got != "chat.startStream,chat.appendStream" {
		t.Fatalf("calls before settlement = %v, want no unsafe update", got)
	}
	if err := r.Finish(ctx, "final answer"); err != nil {
		t.Fatal(err)
	}
	want := "chat.startStream,chat.appendStream,chat.stopStream,chat.update"
	if got := strings.Join(stub.methods(), ","); got != want {
		t.Fatalf("calls = %v, want stop then patch of the original reply", got)
	}
	calls := stub.recorded()
	if got := calls[len(calls)-1].params["markdown_text"]; got != "final answer" {
		t.Fatalf("patched text = %v, want authoritative final", got)
	}

	got := logs.String()
	for _, field := range []string{
		"append_text_bytes=15", "append_text_chars=15",
		"stream_text_bytes=15", "stream_text_chars=15",
		"append_chunk_index=1", "append_chunk_count=1", "append_chunk_bytes=15",
		"repaired Slack reply after incomplete stream",
	} {
		if !strings.Contains(got, field) {
			t.Errorf("logs do not contain %q: %s", field, got)
		}
	}
	if strings.Contains(got, "private partial") || strings.Contains(got, "final answer") {
		t.Fatalf("logs exposed reply text: %s", got)
	}
}

func TestStreamTextBudgetDefersToBoundedFinalPatch(t *testing.T) {
	stub := &stubSlack{}
	r := newTestRenderer(t, stub, "stream")
	ctx := context.Background()
	text := strings.Repeat("x", maxMessage+1)

	if err := r.Start(ctx); err != nil {
		t.Fatal(err)
	}
	if err := r.Delta(ctx, text); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(stub.methods(), ","); got != "chat.startStream" {
		t.Fatalf("calls before settlement = %v, want no append beyond budget", got)
	}
	if !r.streamTextIncomplete || r.streamState != streamActive {
		t.Fatalf("budget overflow state = incomplete:%v stream:%v, want incomplete and active", r.streamTextIncomplete, r.streamState)
	}
	if err := r.Finish(ctx, text); err != nil {
		t.Fatal(err)
	}
	want := "chat.startStream,chat.stopStream,chat.update"
	if got := strings.Join(stub.methods(), ","); got != want {
		t.Fatalf("calls = %v, want stop then bounded patch", got)
	}
	calls := stub.recorded()
	patched := calls[len(calls)-1].params["markdown_text"].(string)
	if patched != truncate(text, maxMessage) {
		t.Fatalf("patched text length = %d, want bounded final length %d", len(patched), len(truncate(text, maxMessage)))
	}
	if got := utf8.RuneCountInString(patched); got > maxMessage {
		t.Errorf("patched markdown_text has %d characters, exceeds Slack's %d-character limit", got, maxMessage)
	}
	if len(patched) > maxMessage {
		t.Errorf("patched markdown_text has %d bytes, exceeds the conservative %d-byte budget", len(patched), maxMessage)
	}
}

func TestTooLongFinalPatchUsesDeliveryNotice(t *testing.T) {
	var logs bytes.Buffer
	log := slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	stub := &stubSlack{policy: func(method string, params map[string]any) string {
		if method == "chat.update" && params["markdown_text"] != nil {
			return "msg_too_long"
		}
		return ""
	}}
	r := newTestRenderer(t, stub, "stream")
	r.log = log
	ctx := context.Background()
	text := strings.Repeat("x", maxMessage+1)

	if err := r.Start(ctx); err != nil {
		t.Fatal(err)
	}
	if err := r.Finish(ctx, text); err != nil {
		t.Fatal(err)
	}
	want := "chat.startStream,chat.stopStream,chat.update,chat.update"
	if got := strings.Join(stub.methods(), ","); got != want {
		t.Fatalf("calls = %v, want update then short notice on the same reply", got)
	}
	calls := stub.recorded()
	if got := calls[len(calls)-1].params["text"]; got != replyTooLongNotice {
		t.Fatalf("fallback text = %v, want delivery notice", got)
	}
	if strings.Contains(logs.String(), strings.Repeat("x", 100)) {
		t.Fatalf("logs exposed the oversized reply")
	}
	if !strings.Contains(logs.String(), "Slack reply replaced with a size-limit notice") {
		t.Fatalf("missing fallback outcome log: %s", logs.String())
	}
}

func TestTerminalAppendFailureIsSafeForFinishAndFail(t *testing.T) {
	for _, outcome := range []string{"finish", "failure"} {
		t.Run(outcome, func(t *testing.T) {
			stub := &stubSlack{policy: func(method string, _ map[string]any) string {
				if method == "chat.appendStream" {
					return "message_not_in_streaming_state"
				}
				return ""
			}}
			r := newTestRenderer(t, stub, "stream")
			ctx := context.Background()
			if err := r.Start(ctx); err != nil {
				t.Fatal(err)
			}
			if err := r.Delta(ctx, "partial"); err != nil {
				t.Fatal(err)
			}
			if r.streamState != streamClosed || r.ts == "" {
				t.Fatalf("terminal append lost closure or timestamp: state=%v ts=%q", r.streamState, r.ts)
			}
			switch outcome {
			case "finish":
				if err := r.Finish(ctx, "authoritative answer"); err != nil {
					t.Fatal(err)
				}
			case "failure":
				if err := r.Fail(ctx, errors.New("turn failed")); err != nil {
					t.Fatal(err)
				}
			}
			calls := stub.recorded()
			for _, call := range calls {
				if call.method == "chat.stopStream" {
					t.Fatal("an already-stopped stream should not be stopped again")
				}
			}
			last := calls[len(calls)-1]
			if last.method != "chat.update" {
				t.Fatalf("last call = %s, want update of the original message", last.method)
			}
			got := last.params["markdown_text"].(string)
			if outcome == "finish" && got != "authoritative answer" {
				t.Fatalf("final text = %q", got)
			}
			if outcome == "failure" && !strings.Contains(got, "turn failed") {
				t.Fatalf("failure text = %q", got)
			}
		})
	}
}

func TestAmbiguousAppendWaitsForSettlementToStopStream(t *testing.T) {
	stub := &stubSlack{failed: map[string]string{"chat.appendStream": "ratelimited"}}
	r := newTestRenderer(t, stub, "stream")
	ctx := context.Background()
	if err := r.Start(ctx); err != nil {
		t.Fatal(err)
	}
	if err := r.Delta(ctx, "partial"); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(stub.methods(), ","); got != "chat.startStream,chat.appendStream" {
		t.Fatalf("calls before settlement = %v, want no fallback update", got)
	}
	if err := r.Finish(ctx, "authoritative answer"); err != nil {
		t.Fatal(err)
	}
	want := "chat.startStream,chat.appendStream,chat.stopStream,chat.update"
	if got := strings.Join(stub.methods(), ","); got != want {
		t.Fatalf("calls = %v, want stop before fallback update", got)
	}
}

func TestStopFailurePreservesStreamStateAndHandle(t *testing.T) {
	for _, initial := range []streamState{streamActive, streamUnknown} {
		t.Run(map[streamState]string{streamActive: "active", streamUnknown: "unknown"}[initial], func(t *testing.T) {
			stub := &stubSlack{policy: func(method string, _ map[string]any) string {
				switch method {
				case "chat.appendStream":
					return "ratelimited"
				case "chat.stopStream":
					return "ratelimited"
				default:
					return ""
				}
			}}
			r := newTestRenderer(t, stub, "stream")
			ctx := context.Background()
			if err := r.Start(ctx); err != nil {
				t.Fatal(err)
			}
			if initial == streamUnknown {
				if err := r.Delta(ctx, "partial"); err != nil {
					t.Fatal(err)
				}
			}
			ts := r.ts
			if err := r.Finish(ctx, "answer"); err == nil {
				t.Fatal("a failed stop should fail final delivery")
			}
			if r.ts != ts || r.streamState != initial {
				t.Fatalf("failed stop changed handle/state: ts=%q state=%v, want ts=%q state=%v", r.ts, r.streamState, ts, initial)
			}
			if err := r.Fail(ctx, errors.New("turn failed")); err == nil {
				t.Fatal("failure reporting must also surface an unclosed stream")
			}
			if r.ts != ts || r.streamState != initial {
				t.Fatalf("failed failure-report stop changed handle/state: ts=%q state=%v", r.ts, r.streamState)
			}
			for _, call := range stub.recorded() {
				if call.method == "chat.update" || call.method == "chat.postMessage" {
					t.Fatalf("fallback attempted with unclosed stream: %v", stub.methods())
				}
			}
		})
	}
}

func TestUpdateConflictStopsBeforeRetryingFallback(t *testing.T) {
	updates, stops := 0, 0
	stub := &stubSlack{policy: func(method string, _ map[string]any) string {
		switch method {
		case "chat.appendStream":
			return "ratelimited"
		case "chat.stopStream":
			stops++
		case "chat.update":
			updates++
			if updates == 1 {
				return "streaming_state_conflict"
			}
		}
		return ""
	}}
	r := newTestRenderer(t, stub, "stream")
	ctx := context.Background()
	if err := r.Start(ctx); err != nil {
		t.Fatal(err)
	}
	if err := r.Delta(ctx, "partial"); err != nil {
		t.Fatal(err)
	}
	if err := r.Finish(ctx, "authoritative answer"); err != nil {
		t.Fatal(err)
	}
	want := "chat.startStream,chat.appendStream,chat.stopStream,chat.update,chat.stopStream,chat.update"
	if got := strings.Join(stub.methods(), ","); got != want {
		t.Fatalf("calls = %v, want stop after conflict before retry: %s", got, want)
	}
	if stops != 2 || updates != 2 {
		t.Fatalf("stops/updates = %d/%d, want 2/2", stops, updates)
	}
}

func TestStreamStateTransitionLogsTrackRecoveryWithoutReplyText(t *testing.T) {
	var logs bytes.Buffer
	log := slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	updates := 0
	stub := &stubSlack{policy: func(method string, _ map[string]any) string {
		if method == "chat.update" {
			updates++
			if updates == 1 {
				return "streaming_state_conflict"
			}
		}
		return ""
	}}
	r := newTestRenderer(t, stub, "stream")
	r.log = log
	ctx := context.Background()

	if err := r.Start(ctx); err != nil {
		t.Fatal(err)
	}
	if err := r.Delta(ctx, "private partial"); err != nil {
		t.Fatal(err)
	}
	if err := r.Finish(ctx, "private final"); err != nil {
		t.Fatal(err)
	}

	got := logs.String()
	for _, want := range []string{
		"Slack reply stream state changed",
		"from=none", "to=active", "reason=start_stream",
		"finishing Slack reply", "final_matches_streamed=false",
		"stopping Slack reply stream", "reason=stop_succeeded",
		"to=unknown", "reason=update_conflict",
		"Slack message update conflicted with an active stream", "message_ts=",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("logs do not contain %q: %s", want, got)
		}
	}
	if strings.Contains(got, "private partial") || strings.Contains(got, "private final") {
		t.Fatalf("logs exposed reply text: %s", got)
	}
}

func TestUpdateConflictStopFailureDoesNotOrphanOrDuplicate(t *testing.T) {
	stops, updates := 0, 0
	stub := &stubSlack{policy: func(method string, _ map[string]any) string {
		switch method {
		case "chat.appendStream":
			return "ratelimited"
		case "chat.stopStream":
			stops++
			if stops == 2 {
				return "ratelimited"
			}
		case "chat.update":
			updates++
			return "streaming_state_conflict"
		}
		return ""
	}}
	r := newTestRenderer(t, stub, "stream")
	ctx := context.Background()
	if err := r.Start(ctx); err != nil {
		t.Fatal(err)
	}
	if err := r.Delta(ctx, "partial"); err != nil {
		t.Fatal(err)
	}
	ts := r.ts
	if err := r.Finish(ctx, "authoritative answer"); err == nil {
		t.Fatal("a failed stop after update conflict should surface as a transport error")
	}
	if r.ts != ts || r.streamState != streamUnknown {
		t.Fatalf("unclosed stream lost its handle/state: ts=%q state=%v", r.ts, r.streamState)
	}
	want := "chat.startStream,chat.appendStream,chat.stopStream,chat.update,chat.stopStream"
	if got := strings.Join(stub.methods(), ","); got != want {
		t.Fatalf("calls = %v, want no fallback after failed conflict closure", got)
	}
	if stops != 2 || updates != 1 {
		t.Fatalf("stops/updates = %d/%d, want 2/1", stops, updates)
	}
}

func TestAlreadyStoppedStreamCanBePatched(t *testing.T) {
	stub := &stubSlack{failed: map[string]string{"chat.stopStream": "message_not_in_streaming_state"}}
	r := newTestRenderer(t, stub, "stream")
	ctx := context.Background()
	if err := r.Start(ctx); err != nil {
		t.Fatal(err)
	}
	if err := r.Delta(ctx, "partial"); err != nil {
		t.Fatal(err)
	}
	if err := r.Finish(ctx, "authoritative answer"); err != nil {
		t.Fatal(err)
	}
	want := "chat.startStream,chat.appendStream,chat.stopStream,chat.update"
	if got := strings.Join(stub.methods(), ","); got != want {
		t.Fatalf("calls = %v, want explicit already-stopped response to allow update", got)
	}
	if r.streamState != streamClosed || r.ts == "" {
		t.Fatalf("already-stopped response lost state/handle: state=%v ts=%q", r.streamState, r.ts)
	}
}

func TestPatchModeNeverStreams(t *testing.T) {
	stub := &stubSlack{}
	r := newTestRenderer(t, stub, "patch")
	ctx := context.Background()

	if err := r.Start(ctx); err != nil {
		t.Fatal(err)
	}
	if err := r.Delta(ctx, "hello"); err != nil {
		t.Fatal(err)
	}
	if err := r.Finish(ctx, "hello"); err != nil {
		t.Fatal(err)
	}

	got := strings.Join(stub.methods(), ",")
	want := "chat.postMessage,chat.update,chat.update"
	if got != want {
		t.Fatalf("calls = %v, want %v", got, want)
	}
}

func TestEachTurnPostsANewReply(t *testing.T) {
	posted := 0
	stub := &stubSlack{bodies: func(method string, _ map[string]any) string {
		if method != "chat.postMessage" {
			return ""
		}
		posted++
		return fmt.Sprintf(`{"ok":true,"ts":"1700000000.0000%02d"}`, posted)
	}}
	platform := newTestPlatform(t, stub, "patch")
	ctx := context.Background()

	// A later user prompt gets its own renderer and response; it must not reuse
	// the previous turn's message handle.
	for _, answer := range []string{"first response", "second response"} {
		r := testRenderer(t, platform)
		if err := r.Start(ctx); err != nil {
			t.Fatal(err)
		}
		if err := r.Delta(ctx, answer); err != nil {
			t.Fatal(err)
		}
		if err := r.Finish(ctx, answer); err != nil {
			t.Fatal(err)
		}
	}

	var messages, updated []string
	for _, call := range stub.recorded() {
		switch call.method {
		case "chat.postMessage":
			messages = append(messages, call.params["markdown_text"].(string))
		case "chat.update":
			updated = append(updated, call.params["ts"].(string))
		}
	}
	if len(messages) != 2 {
		t.Fatalf("posted %d replies, want one new reply for each turn", len(messages))
	}
	if len(updated) != 4 {
		t.Fatalf("updated %d times, want streaming patches to each reply", len(updated))
	}
	for i, ts := range updated {
		if want := fmt.Sprintf("1700000000.0000%02d", i/2+1); ts != want {
			t.Errorf("update %d targeted %q, want the current turn's reply %q", i, ts, want)
		}
	}
}

func TestFailKeepsThePartialAnswer(t *testing.T) {
	stub := &stubSlack{}
	r := newTestRenderer(t, stub, "patch")
	ctx := context.Background()

	if err := r.Start(ctx); err != nil {
		t.Fatal(err)
	}
	if err := r.Delta(ctx, "half an answer"); err != nil {
		t.Fatal(err)
	}
	if err := r.Fail(ctx, errors.New("prompt: connection lost")); err != nil {
		t.Fatal(err)
	}

	text, _ := stub.recorded()[len(stub.recorded())-1].params["markdown_text"].(string)
	if !strings.Contains(text, "half an answer") || !strings.Contains(text, "connection lost") {
		t.Fatalf("failure text = %q, want the partial answer and the cause", text)
	}
}

func TestFailKeepsCauseWhenPartialAnswerIsLong(t *testing.T) {
	stub := &stubSlack{}
	r := newTestRenderer(t, stub, "patch")
	ctx := context.Background()
	if err := r.Start(ctx); err != nil {
		t.Fatal(err)
	}
	r.text.WriteString(strings.Repeat("p", maxMessage))
	cause := errors.New("create the session: pi binary not found")

	if err := r.Fail(ctx, cause); err != nil {
		t.Fatal(err)
	}

	calls := stub.recorded()
	text := calls[len(calls)-1].params["markdown_text"].(string)
	if !strings.Contains(text, cause.Error()) {
		t.Fatalf("failure text omitted cause %q", cause)
	}
	if !strings.Contains(text, "truncated") {
		t.Fatalf("failure text should mark the partial answer as truncated: %q", text)
	}
	if len(text) > maxMessage {
		t.Fatalf("failure text is %d bytes, exceeds %d", len(text), maxMessage)
	}
}

func TestFailBeforeStartPostsFailureInThread(t *testing.T) {
	stub := &stubSlack{}
	r := newTestRenderer(t, stub, "patch")
	ctx := context.Background()
	cause := "create the session: gwclient: find pi binary: no such file"

	if err := r.Fail(ctx, errors.New(cause)); err != nil {
		t.Fatal(err)
	}

	calls := stub.recorded()
	if len(calls) != 1 || calls[0].method != "chat.postMessage" {
		t.Fatalf("calls = %v, want a new failure message without an update", stub.methods())
	}
	if got := calls[0].params["thread_ts"]; got != testMessage().Thread.ThreadTS {
		t.Errorf("thread_ts = %v, want %q", got, testMessage().Thread.ThreadTS)
	}
	if got := calls[0].params["markdown_text"]; got != ":warning: pi-chat: "+cause {
		t.Errorf("failure text = %v, want the full cause", got)
	}
}

func TestFailPreservesTextWhenNoReplyWasStarted(t *testing.T) {
	stub := &stubSlack{}
	r := newTestRenderer(t, stub, "patch")
	ctx := context.Background()
	if err := r.Delta(ctx, "partial answer"); err != nil {
		t.Fatalf("Delta before Start: %v", err)
	}
	if calls := stub.recorded(); len(calls) != 0 {
		t.Fatalf("buffering text without a reply made API calls: %+v", calls)
	}
	cause := errors.New("Slack was unavailable when the reply started")

	if err := r.Fail(ctx, cause); err != nil {
		t.Fatalf("Fail: %v", err)
	}
	calls := stub.recorded()
	if len(calls) != 1 || calls[0].method != "chat.postMessage" {
		t.Fatalf("calls = %+v, want a failure message posted after buffering", calls)
	}
	text := calls[0].params["markdown_text"].(string)
	if !strings.Contains(text, "partial answer") || !strings.Contains(text, cause.Error()) {
		t.Errorf("failure text = %q, want partial answer and cause", text)
	}
}

func TestFailDeletedMessagePostsFailureInThread(t *testing.T) {
	stub := &stubSlack{}
	r := newTestRenderer(t, stub, "patch")
	ctx := context.Background()
	if err := r.Start(ctx); err != nil {
		t.Fatal(err)
	}
	r.text.WriteString(strings.Repeat("p", maxMessage))
	stub.mu.Lock()
	stub.failed = map[string]string{"chat.update": "message_not_found"}
	stub.mu.Unlock()

	cause := errors.New("prompt: connection lost")
	if err := r.Fail(ctx, cause); err != nil {
		t.Fatal(err)
	}

	calls := stub.recorded()
	if got, want := strings.Join(stub.methods(), ","), "chat.postMessage,chat.update,chat.postMessage"; got != want {
		t.Fatalf("calls = %s, want %s", got, want)
	}
	last := calls[len(calls)-1]
	if got := last.params["thread_ts"]; got != testMessage().Thread.ThreadTS {
		t.Errorf("fallback thread_ts = %v, want %q", got, testMessage().Thread.ThreadTS)
	}
	text := last.params["markdown_text"].(string)
	if !strings.Contains(text, cause.Error()) || !strings.Contains(text, "truncated") {
		t.Errorf("fallback text = %q, want truncated partial answer and full cause", text)
	}
	if len(text) > maxMessage {
		t.Errorf("fallback text is %d bytes, exceeds %d", len(text), maxMessage)
	}
}

func TestDeltaChunksLongText(t *testing.T) {
	stub := &stubSlack{}
	r := newTestRenderer(t, stub, "stream")
	ctx := context.Background()

	if err := r.Start(ctx); err != nil {
		t.Fatal(err)
	}
	if err := r.Delta(ctx, strings.Repeat("x", 7000)); err != nil {
		t.Fatal(err)
	}

	appends := 0
	var streamed strings.Builder
	for _, call := range stub.recorded() {
		if call.method != "chat.appendStream" {
			continue
		}
		appends++
		var chunks []streamPayloadChunk
		jsonParam(t, call.params, "chunks", &chunks)
		if len(chunks) != 1 || chunks[0].Type != "markdown_text" {
			t.Fatalf("append chunks = %+v, want one markdown_text chunk", chunks)
		}
		text := chunks[0].Text
		if len(text) > streamChunk {
			t.Fatalf("append of %d bytes exceeds the %d-byte chunk", len(text), streamChunk)
		}
		streamed.WriteString(text)
	}
	if got, want := streamed.String(), strings.Repeat("x", 7000); got != want {
		t.Fatalf("streamed %d bytes, want the original %d-byte text", len(got), len(want))
	}
	if appends < 3 {
		t.Fatalf("7000 bytes went out in %d appends, want at least 3", appends)
	}
}

func TestChunkKeepsRunesIntact(t *testing.T) {
	// Every cut has to land on a rune boundary, or Slack receives invalid
	// UTF-8 and renders replacement characters.
	text := strings.Repeat("héllo wörld ", 40)
	pieces := chunk(text, 7)
	if strings.Join(pieces, "") != text {
		t.Fatal("chunking changed the text")
	}
	for _, piece := range pieces {
		if !utf8.ValidString(piece) {
			t.Fatalf("chunk %q is not valid UTF-8", piece)
		}
	}
}

func TestTruncateMarksAndBounds(t *testing.T) {
	short := "small"
	if got := truncate(short, 100); got != short {
		t.Fatalf("truncate(%q, 100) = %q", short, got)
	}
	long := strings.Repeat("a", 200)
	got := truncate(long, 100)
	if !strings.Contains(got, "truncated") {
		t.Fatalf("truncate did not mark the cut: %q", got)
	}
	if len(got) > 100 {
		t.Fatalf("truncate returned %d bytes, want it bounded", len(got))
	}
	if !utf8.ValidString(truncate(strings.Repeat("é", 100), 101)) {
		t.Fatal("truncate split a rune")
	}
}

func TestRetryDelay(t *testing.T) {
	tests := []struct {
		header string
		want   time.Duration
	}{
		{"", time.Second},
		{"0", time.Second},
		{"garbage", time.Second},
		{"-3", time.Second},
		{"2", 2 * time.Second},
		{"60", 5 * time.Second},
	}
	for _, test := range tests {
		if got := retryDelay(test.header); got != test.want {
			t.Errorf("retryDelay(%q) = %v, want %v", test.header, got, test.want)
		}
	}
}

func TestAPIErrorIsTyped(t *testing.T) {
	stub := &stubSlack{failed: map[string]string{"chat.postMessage": "not_in_channel"}}
	api := newStubAPI(t, stub)

	_, err := api.PostMessage(context.Background(), "C1", "1", "hi")
	if err == nil {
		t.Fatal("want an error")
	}
	if !IsCode(err, "not_in_channel") {
		t.Fatalf("err = %v, want a not_in_channel slack error", err)
	}
	var slackErr *Error
	if !errors.As(err, &slackErr) || slackErr.Method != "chat.postMessage" {
		t.Fatalf("err = %#v, want a *slack.Error naming the method", err)
	}
}

func TestRateLimitedRequestIsRetried(t *testing.T) {
	stub := &stubSlack{status: http.StatusTooManyRequests}
	api := newStubAPI(t, stub)

	ts, err := api.PostMessage(context.Background(), "C1", "", "hi")
	if err != nil {
		t.Fatalf("the retry should have succeeded: %v", err)
	}
	if ts == "" {
		t.Fatal("no timestamp returned")
	}
	calls := stub.recorded()
	if len(calls) != 2 {
		t.Fatalf("%d requests, want a retry after the 429", len(calls))
	}
	// The retry must carry the request again: a reused reader would have been
	// drained by the first attempt, so the second request would arrive with no
	// parameters at all — a 429 turned into an invalid_arguments failure.
	if got := calls[1].params["channel"]; got != "C1" {
		t.Errorf("the retried request lost its parameters: %#v", calls[1].params)
	}
	if got := calls[1].params["text"]; got != "hi" {
		t.Errorf("the retried request lost its text: %#v", calls[1].params)
	}
}

func TestConnectionsOpenUsesTheAppToken(t *testing.T) {
	stub := &stubSlack{}
	api := newStubAPI(t, stub)

	// The stub answers without a url, so this fails — but the request it made
	// is what matters: the app-level token authorizes only this call.
	if _, err := api.ConnectionsOpen(context.Background(), "xapp-test"); err == nil {
		t.Fatal("an answer without a url should be an error")
	}
	calls := stub.recorded()
	if len(calls) != 1 || calls[0].method != "apps.connections.open" {
		t.Fatalf("calls = %v", stub.methods())
	}
	if calls[0].auth != "Bearer xapp-test" {
		t.Fatalf("auth header = %q, want the app token", calls[0].auth)
	}
}

func TestRefreshAfter(t *testing.T) {
	tests := []struct {
		seconds int
		want    time.Duration
	}{
		{0, 50 * time.Minute},
		{60, 5 * time.Minute},    // clamped up: too eager to refresh
		{3600, 50 * time.Minute}, // 54 minutes of slack time, capped
		{600, 9 * time.Minute},
	}
	for _, test := range tests {
		if got := refreshAfter(test.seconds); got != test.want {
			t.Errorf("refreshAfter(%d) = %v, want %v", test.seconds, got, test.want)
		}
	}
}
