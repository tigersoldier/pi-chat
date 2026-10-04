package selfdm

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tigersoldier/pi-chat/internal/bot"
	"github.com/tigersoldier/pi-chat/internal/slack"
)

func discardLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// stubCall is one recorded Web API request.
type stubCall struct {
	method string
	params url.Values
}

// stubSlack answers the Web API methods the self-DM uses, and records what it
// was asked, so a test can assert on the conversation that was read and the
// messages that were posted.
type stubSlack struct {
	mu     sync.Mutex
	calls  []stubCall
	routes map[string]func(url.Values) string
}

func newStub(routes map[string]func(url.Values) string) *stubSlack {
	if routes == nil {
		routes = map[string]func(url.Values) string{}
	}
	if _, ok := routes["auth.test"]; !ok {
		routes["auth.test"] = func(url.Values) string { return okAuth }
	}
	if _, ok := routes["conversations.history"]; !ok {
		routes["conversations.history"] = func(url.Values) string { return `{"ok":true,"messages":[]}` }
	}
	return &stubSlack{routes: routes}
}

// okAuth is the answer auth.test gives for a browser session: a person, not a
// bot, and no bot_id at all.
const okAuth = `{"ok":true,"url":"https://acme.slack.com/","team":"Acme","user":"me",` +
	`"user_id":"U1","team_id":"T1","enterprise_id":""}`

func (s *stubSlack) handler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		method := strings.TrimPrefix(r.URL.Path, "/api/")
		raw, _ := io.ReadAll(r.Body)
		params, _ := url.ParseQuery(string(raw))

		s.mu.Lock()
		s.calls = append(s.calls, stubCall{method: method, params: params})
		route := s.routes[method]
		s.mu.Unlock()

		w.Header().Set("Content-Type", "application/json")
		if route == nil {
			http.Error(w, `{"ok":false,"error":"unknown_method"}`, http.StatusOK)
			return
		}
		_, _ = io.WriteString(w, route(params))
	}
}

// api builds the client the daemon builds for this surface: the workspace's own
// host, the session token as a bearer token, and the session cookie.
func (s *stubSlack) api(t *testing.T) *slack.API {
	t.Helper()
	srv := httptest.NewServer(s.handler())
	t.Cleanup(srv.Close)
	return slack.NewAPI("xoxc-test", discardLogger(),
		slack.WithBaseURL(srv.URL+"/api"),
		slack.WithCookie(slack.CookieHeader("xoxd-test+value")))
}

// count is how many times a method was called.
func (s *stubSlack) count(method string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, call := range s.calls {
		if call.method == method {
			n++
		}
	}
	return n
}

// params is the nth (zero-based) call's arguments.
func (s *stubSlack) params(method string, n int) url.Values {
	s.mu.Lock()
	defer s.mu.Unlock()
	seen := 0
	for _, call := range s.calls {
		if call.method != method {
			continue
		}
		if seen == n {
			return call.params
		}
		seen++
	}
	return url.Values{}
}

// memState is the durable state, in a map.
type memState struct {
	mu     sync.Mutex
	kv     map[string]string
	posted map[string]bool
	prunes int
}

func newMemState() *memState {
	return &memState{kv: map[string]string{}, posted: map[string]bool{}}
}

func (m *memState) SurfaceKV(_ context.Context, surface, key string) (string, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	value, ok := m.kv[surface+"/"+key]
	return value, ok, nil
}

func (m *memState) SetSurfaceKV(_ context.Context, surface, key, value string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.kv[surface+"/"+key] = value
	return nil
}

func (m *memState) MarkSurfacePosted(_ context.Context, surface, ts string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.posted[surface+"/"+ts] = true
	return nil
}

func (m *memState) SurfacePosted(_ context.Context, surface, ts string) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.posted[surface+"/"+ts], nil
}

func (m *memState) PruneSurfacePosted(context.Context, time.Time) (int64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.prunes++
	return 0, nil
}

func (m *memState) get(surface, key string) string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.kv[surface+"/"+key]
}

func (m *memState) markPosted(ts string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.posted[SurfaceName+"/"+ts] = true
}

// recordingCore is the core as the router and the poller see it.
type recordingCore struct {
	mu       sync.Mutex
	messages []bot.Message
	commands []bot.Command
	actions  []bot.Action
}

func (c *recordingCore) HandleMessage(_ context.Context, m bot.Message) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.messages = append(c.messages, m)
}

func (c *recordingCore) HandleCommand(_ context.Context, cmd bot.Command) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.commands = append(c.commands, cmd)
}

func (c *recordingCore) HandleAction(_ context.Context, a bot.Action) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.actions = append(c.actions, a)
}

func (c *recordingCore) got() (int, int, int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.messages), len(c.commands), len(c.actions)
}

// surfaceFor builds the surface the daemon builds — New, then the same router
// wiring Run does — over a stubbed Slack.
func surfaceFor(t *testing.T, routes map[string]func(url.Values) string) (*Surface, *recordingCore, *stubSlack, *memState) {
	t.Helper()
	stub := newStub(routes)
	st := newMemState()
	s, err := New(context.Background(), stub.api(t), st,
		Config{Channel: "D1", PollEvery: time.Millisecond, FlushMS: 1}, discardLogger())
	if err != nil {
		t.Fatalf("New over a stub Slack: %v", err)
	}
	core := &recordingCore{}
	s.router = &Router{core: core, plat: s.plat, state: st, log: discardLogger(), id: s.id, channel: s.cfg.Channel}
	return s, core, stub, st
}

// TestNewRefusesAChannelItCannotRead: a self-DM pointed at the wrong id must
// fail at startup, not run and answer nothing.
func TestNewRefusesAChannelItCannotRead(t *testing.T) {
	routes := map[string]func(url.Values) string{
		"conversations.history": func(url.Values) string {
			return `{"ok":false,"error":"channel_not_found"}`
		},
	}
	stub := newStub(routes)
	_, err := New(context.Background(), stub.api(t), newMemState(),
		Config{Channel: "D404", PollEvery: time.Millisecond}, discardLogger())
	if err == nil {
		t.Fatal("want an error for a channel the token cannot read")
	}
	if !strings.Contains(err.Error(), "channel_not_found") {
		t.Errorf("the error should carry Slack's own code, got: %v", err)
	}
}

// TestNewStartsAFirstRunAtTheNewestMessage: enabling the surface must not replay
// a personal DM as a batch of prompts.
func TestNewStartsAFirstRunAtTheNewestMessage(t *testing.T) {
	routes := map[string]func(url.Values) string{
		"conversations.history": func(url.Values) string {
			return `{"ok":true,"messages":[{"ts":"1700000005.000000","user":"U1","text":"the newest thing"}]}`
		},
	}
	stub := newStub(routes)
	s, err := New(context.Background(), stub.api(t), newMemState(),
		Config{Channel: "D1", PollEvery: time.Millisecond}, discardLogger())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if s.newest != "1700000005.000000" {
		t.Errorf("newest = %q, want the conversation's newest message", s.newest)
	}
	if s.Identity().UserID != "U1" || s.Identity().TeamID != "T1" {
		t.Errorf("identity = %+v, want the authenticated person", s.Identity())
	}
}

// TestPollReadsOnlyWhatIsNewerThanTheCursor is the cursor's whole job: a message
// already handled is not handled twice, and a message that is not is.
func TestPollReadsOnlyWhatIsNewerThanTheCursor(t *testing.T) {
	routes := map[string]func(url.Values) string{
		"conversations.history": func(url.Values) string {
			return `{"ok":true,"messages":[` +
				`{"ts":"5.0","user":"U1","text":"newest"},` +
				`{"ts":"4.0","user":"U1","text":"older"},` +
				`{"ts":"3.0","user":"U1","text":"oldest"}]}`
		},
	}
	s, core, stub, st := surfaceFor(t, routes)
	cursor := "3.0"
	if err := s.poll(context.Background(), &cursor); err != nil {
		t.Fatalf("poll: %v", err)
	}

	core.mu.Lock()
	got := append([]bot.Message(nil), core.messages...)
	core.mu.Unlock()
	if len(got) != 2 {
		t.Fatalf("routed %d messages, want the two newer than the cursor: %+v", len(got), got)
	}
	if got[0].TS != "4.0" || got[1].TS != "5.0" {
		t.Errorf("routed timestamps = %q,%q, want 4.0 then 5.0 in order", got[0].TS, got[1].TS)
	}
	if cursor != "5.0" {
		t.Errorf("cursor = %q, want the newest handled message", cursor)
	}
	if onDisk := st.get(SurfaceName, cursorKey); onDisk != "5.0" {
		t.Errorf("stored cursor = %q, want 5.0", onDisk)
	}
	// The probe at construction asked without a bound; the poll asks from the
	// cursor, and that is what Slack is told.
	if oldest := stub.params("conversations.history", 1).Get("oldest"); oldest != "3.0" {
		t.Errorf("conversations.history oldest = %q, want 3.0", oldest)
	}
}

// TestPollReadsThreadRepliesOnce: a reply is a continuation of its thread's
// session, and the per-thread cursor is what keeps a late reply in an older
// thread from being missed by a conversation-wide cursor that has moved on.
func TestPollReadsThreadRepliesOnce(t *testing.T) {
	routes := map[string]func(url.Values) string{
		"conversations.history": func(url.Values) string {
			return `{"ok":true,"messages":[` +
				`{"ts":"5.0","user":"U1","text":"start a session","reply_count":1,"latest_reply":"6.0"}]}`
		},
		"conversations.replies": func(url.Values) string {
			// The reply deliberately omits thread_ts: Slack omits it on a thread's
			// parent, and the poller has to take the thread from the fetch it made
			// rather than depend on which shape arrived.
			return `{"ok":true,"messages":[` +
				`{"ts":"5.0","user":"U1","text":"start a session"},` +
				`{"ts":"6.0","user":"U1","text":"and continue it"}]}`
		},
	}
	s, core, stub, st := surfaceFor(t, routes)
	cursor := "4.0"
	if err := s.poll(context.Background(), &cursor); err != nil {
		t.Fatalf("poll: %v", err)
	}
	if n := stub.count("conversations.replies"); n != 1 {
		t.Fatalf("conversations.replies called %d times, want 1", n)
	}
	if got := stub.params("conversations.replies", 0).Get("oldest"); got != "" {
		t.Errorf("the first reply read used oldest = %q, want the whole thread", got)
	}

	core.mu.Lock()
	got := append([]bot.Message(nil), core.messages...)
	core.mu.Unlock()
	if len(got) != 2 {
		t.Fatalf("routed %d messages, want the root and its reply: %+v", len(got), got)
	}
	reply := got[1]
	if reply.TS != "6.0" || reply.Thread.ThreadTS != "5.0" {
		t.Errorf("reply = ts %q in thread %q, want 6.0 in 5.0", reply.TS, reply.Thread.ThreadTS)
	}
	if onDisk := st.get(SurfaceName, replyKeyPrefix+"5.0"); onDisk != "6.0" {
		t.Errorf("reply cursor = %q, want 6.0", onDisk)
	}

	// A second poll has nothing new to say about the thread: the conversation
	// still carries the parent, and its latest reply is not newer than where the
	// thread cursor already is.
	if err := s.poll(context.Background(), &cursor); err != nil {
		t.Fatalf("second poll: %v", err)
	}
	if n := stub.count("conversations.replies"); n != 1 {
		t.Errorf("conversations.replies called %d times after two polls, want 1", n)
	}
}

// TestPollAdvancesPastItsOwnPosts: the daemon posts as the person it reads as,
// so only the ledger can tell the agent's answer from the human's request. The
// answer is skipped, and the cursor still moves past it — otherwise the next
// poll would read it again, forever.
func TestPollAdvancesPastItsOwnPosts(t *testing.T) {
	routes := map[string]func(url.Values) string{
		"conversations.history": func(url.Values) string {
			return `{"ok":true,"messages":[{"ts":"7.0","user":"U1","text":"an answer from pi"}]}`
		},
	}
	s, core, _, st := surfaceFor(t, routes)
	st.markPosted("7.0")

	cursor := "6.0"
	if err := s.poll(context.Background(), &cursor); err != nil {
		t.Fatalf("poll: %v", err)
	}
	if n, _, _ := core.got(); n != 0 {
		t.Errorf("routed %d messages, want none: this one was posted by the daemon itself", n)
	}
	if cursor != "7.0" {
		t.Errorf("cursor = %q, want 7.0 so the post is not read again", cursor)
	}
}

// TestRunRetriesAFailureInsteadOfReturningIt: an expired cookie must not take
// the daemon — or the app surface beside it — down. The loop keeps trying, so
// fixing the cookie and restarting is enough to bring this surface back.
func TestRunRetriesAFailureInsteadOfReturningIt(t *testing.T) {
	var attempts atomic.Int32
	routes := map[string]func(url.Values) string{
		"conversations.history": func(url.Values) string {
			if attempts.Add(1) == 1 {
				return `{"ok":true,"messages":[]}` // the startup probe succeeds
			}
			return `{"ok":false,"error":"invalid_auth"}`
		},
	}
	s, core, stub, _ := surfaceFor(t, routes)

	ctx, cancel := context.WithTimeout(context.Background(), 80*time.Millisecond)
	defer cancel()
	err := s.Run(ctx, core)
	// The context a timeout cancels reports DeadlineExceeded, a signal-cancelled one
	// reports Canceled; either is the shutdown this test asked for. What matters is
	// that the API's own error was not returned.
	if !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Run = %v, want a context error: a failed poll is retried, not returned", err)
	}
	if attempts := stub.count("conversations.history"); attempts < 3 {
		t.Errorf("conversations.history called %d times, want the poll to keep retrying", attempts)
	}
}
