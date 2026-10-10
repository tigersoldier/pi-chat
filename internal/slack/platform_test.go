package slack

import (
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
	if !first.streaming {
		t.Fatal("the retry should have kept streaming")
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
	appended := stub.recorded()[1].params["markdown_text"]
	if appended != "hello" {
		t.Fatalf("appended %v, want \"hello\"", appended)
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
	if text := stub.recorded()[3].params["text"]; text != "partial, with more" {
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
	if text := stub.recorded()[3].params["text"]; text != "hello" {
		t.Fatalf("patched text = %v, want \"hello\"", text)
	}
}

func TestAppendFailureMidTurnFallsBackToPatching(t *testing.T) {
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
		t.Fatalf("a failed append should degrade, not fail the turn: %v", err)
	}
	if got := strings.Join(stub.methods(), ","); !strings.HasSuffix(got, "chat.appendStream,chat.update") {
		t.Fatalf("calls = %v, want an update after the failed append", got)
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

	text, _ := stub.recorded()[len(stub.recorded())-1].params["text"].(string)
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
	text := calls[len(calls)-1].params["text"].(string)
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
	if got := calls[0].params["text"]; got != ":warning: pi-chat: "+cause {
		t.Errorf("failure text = %v, want the full cause", got)
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
	text := last.params["text"].(string)
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
	for _, call := range stub.recorded() {
		if call.method != "chat.appendStream" {
			continue
		}
		appends++
		text, _ := call.params["markdown_text"].(string)
		if len(text) > streamChunk {
			t.Fatalf("append of %d bytes exceeds the %d-byte chunk", len(text), streamChunk)
		}
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
	if len(got) > 100+len("\n\n… _(truncated)_") {
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
