package selfdm

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tigersoldier/pi-chat/internal/bot"
	"github.com/tigersoldier/pi-chat/internal/slack"
)

// postCounter answers chat.postMessage with a fresh timestamp per call, the way
// Slack does, so a test can tell a replacement from a continuation.
func postCounter() func(url.Values) string {
	var n atomic.Int32
	return func(url.Values) string {
		return fmt.Sprintf(`{"ok":true,"ts":"9.%d"}`, n.Add(1))
	}
}

func TestSelfDMRequestLifecycleThroughProductionMux(t *testing.T) {
	base := time.Now().Add(-3 * time.Minute)
	prior := selfDMTestTS(base)
	daemon := selfDMTestTS(base.Add(time.Minute))
	other := selfDMTestTS(base.Add(90 * time.Second))
	current := selfDMTestTS(base.Add(2 * time.Minute))
	stubReplies := `{"ok":true,"messages":[` +
		`{"ts":"` + prior + `","user":"U1","reactions":[` +
		`{"name":"eyes","users":["U1"]},` +
		`{"name":"hourglass_flowing_sand","users":["U1"]},` +
		`{"name":"shrug","users":["U1"]},` +
		`{"name":"white_check_mark","users":["U1"]},` +
		`{"name":"warning","users":["U1"]},` +
		`{"name":"heart","users":["U1"]}]},` +
		`{"ts":"` + daemon + `","user":"U1","reactions":[{"name":"eyes","users":["U1"]}]},` +
		`{"ts":"` + other + `","user":"U1","reactions":[{"name":"eyes","users":["U2"]}]},` +
		`{"ts":"` + current + `","user":"U1","reactions":[{"name":"heart","users":["U1"]}]}]}`
	surface, _, stub, state := surfaceFor(t, map[string]func(url.Values) string{
		"conversations.history": func(url.Values) string { return `{"ok":true,"messages":[]}` },
		"conversations.replies": func(url.Values) string { return stubReplies },
		"reactions.add":         func(url.Values) string { return `{"ok":true}` },
		"reactions.remove":      func(url.Values) string { return `{"ok":true}` },
	})
	state.markPosted(daemon)
	mux := slack.NewMultiPlatform(discardLogger(), nil, map[string]bot.Platform{"D1": surface.plat})
	message := bot.Message{
		Thread:    bot.Thread{Workspace: "T1", Channel: "D1", ThreadTS: prior},
		UserID:    "U1",
		Workspace: "T1",
		TS:        current,
		Mentioned: true,
		Direct:    true,
	}
	ctx := context.Background()

	if err := mux.AcknowledgeRequest(ctx, message, true); err != nil {
		t.Fatalf("AcknowledgeRequest through mux: %v", err)
	}
	if err := mux.QueueRequest(ctx, message); err != nil {
		t.Fatalf("QueueRequest through mux: %v", err)
	}
	if err := mux.StartRequest(ctx, message, true); err != nil {
		t.Fatalf("StartRequest through mux: %v", err)
	}
	if err := mux.CompleteRequest(ctx, message); err != nil {
		t.Fatalf("CompleteRequest through mux: %v", err)
	}

	calls := recordedCalls(stub)
	var got []stubCall
	for _, call := range calls {
		switch call.method {
		case "reactions.add", "reactions.remove", "conversations.replies":
			got = append(got, call)
		}
	}
	want := []struct{ method, name, ts string }{
		{"reactions.add", "eyes", current},
		{"reactions.remove", "eyes", current},
		{"reactions.add", "hourglass_flowing_sand", current},
		{"reactions.remove", "hourglass_flowing_sand", current},
		{"reactions.add", "eyes", current},
		{"conversations.replies", "", ""},
		{"reactions.remove", "eyes", prior},
		{"reactions.remove", "hourglass_flowing_sand", prior},
		{"reactions.remove", "shrug", prior},
		{"reactions.remove", "white_check_mark", prior},
		{"reactions.remove", "warning", prior},
		{"reactions.remove", "eyes", current},
		{"reactions.remove", "hourglass_flowing_sand", current},
		{"reactions.add", "white_check_mark", current},
	}
	if len(got) != len(want) {
		t.Fatalf("request reaction calls = %+v, want %d calls", got, len(want))
	}
	for i, expected := range want {
		if got[i].method != expected.method || got[i].params.Get("name") != expected.name ||
			(expected.ts != "" && got[i].params.Get("timestamp") != expected.ts) {
			t.Errorf("request reaction call %d = %+v, want %s %s at %s", i, got[i], expected.method, expected.name, expected.ts)
		}
	}
	if stub.count("reactions.remove") != 9 {
		t.Errorf("reactions.remove calls = %d, want current state transitions plus five prior request markers", stub.count("reactions.remove"))
	}
}

func TestSelfDMStartRequestAddsEyesWhenNotQueued(t *testing.T) {
	current := selfDMTestTS(time.Now().Add(-time.Minute))
	surface, _, stub, _ := surfaceFor(t, map[string]func(url.Values) string{
		"conversations.history": func(url.Values) string { return `{"ok":true,"messages":[]}` },
		"conversations.replies": func(url.Values) string {
			return `{"ok":true,"messages":[{"ts":"` + current + `","user":"U1"}]}`
		},
		"reactions.add": func(url.Values) string { return `{"ok":true}` },
	})
	message := bot.Message{
		Thread: bot.Thread{Workspace: "T1", Channel: "D1", ThreadTS: current},
		UserID: "U1", Workspace: "T1", TS: current, Direct: true,
	}

	if err := surface.plat.StartRequest(context.Background(), message, false); err != nil {
		t.Fatalf("StartRequest: %v", err)
	}
	calls := recordedCalls(stub)
	addAt, repliesAt := -1, -1
	for i, call := range calls {
		if call.method == "reactions.add" {
			addAt = i
			if call.params.Get("timestamp") != current || call.params.Get("name") != "eyes" {
				t.Errorf("reaction add = %+v, want eyes on current request", call.params)
			}
		}
		if call.method == "conversations.replies" {
			repliesAt = i
		}
	}
	if addAt < 0 || repliesAt < 0 || addAt > repliesAt {
		t.Fatalf("calls = %+v, want eyes added before history cleanup", calls)
	}
}

func TestSelfDMRequestCleanupFailsClosedOnPostedLedgerError(t *testing.T) {
	base := time.Now().Add(-3 * time.Minute)
	uncertain := selfDMTestTS(base)
	safe := selfDMTestTS(base.Add(time.Minute))
	current := selfDMTestTS(base.Add(2 * time.Minute))
	replies := `{"ok":true,"messages":[` +
		`{"ts":"` + uncertain + `","user":"U1","reactions":[{"name":"eyes","users":["U1"]}]},` +
		`{"ts":"` + safe + `","user":"U1","reactions":[{"name":"warning","users":["U1"]}]},` +
		`{"ts":"` + current + `","user":"U1"}]}`
	surface, _, stub, state := surfaceFor(t, map[string]func(url.Values) string{
		"conversations.history": func(url.Values) string { return `{"ok":true,"messages":[]}` },
		"conversations.replies": func(url.Values) string { return replies },
		"reactions.add":         func(url.Values) string { return `{"ok":true}` },
		"reactions.remove":      func(url.Values) string { return `{"ok":true}` },
	})
	failed := &postedLedgerError{memState: state, ts: uncertain}
	surface.state = failed
	surface.plat.state = failed
	message := bot.Message{Thread: bot.Thread{Workspace: "T1", Channel: "D1", ThreadTS: uncertain}, UserID: "U1", Workspace: "T1", TS: current, Mentioned: true, Direct: true}

	if err := surface.plat.StartRequest(context.Background(), message, false); err == nil {
		t.Fatal("StartRequest error = nil, want the ledger failure to be reported")
	}
	removals := 0
	for _, call := range recordedCalls(stub) {
		if call.method != "reactions.remove" {
			continue
		}
		removals++
		if call.params.Get("timestamp") != safe || call.params.Get("name") != "warning" {
			t.Errorf("removal = %+v, want only the certain user request", call.params)
		}
	}
	if removals != 1 {
		t.Errorf("reaction removals = %d, want one; uncertain daemon/user messages must be preserved", removals)
	}
}

func TestSelfDMRootRequestCleanupSkipsNearLedgerCutoffThroughProductionMux(t *testing.T) {
	now := time.Now()
	current := selfDMTestTS(now.Add(-time.Minute))
	daemon := selfDMTestTS(now.Add(-time.Hour))
	prior := selfDMTestTS(now.Add(-2 * time.Hour))
	nearBoundary := selfDMTestTS(now.Add(-postedTTL + time.Minute))
	surface, _, stub, state := surfaceFor(t, map[string]func(url.Values) string{
		"conversations.history": func(params url.Values) string {
			if params.Get("latest") == "" {
				return `{"ok":true,"messages":[]}`
			}
			return `{"ok":true,"messages":[` +
				`{"ts":"` + daemon + `","user":"U1","reactions":[{"name":"eyes","users":["U1"]}]},` +
				`{"ts":"` + prior + `","user":"U1","reactions":[{"name":"warning","users":["U1"]}]},` +
				`{"ts":"` + nearBoundary + `","user":"U1","reactions":[{"name":"eyes","users":["U1"]}]}]}`
		},
		"conversations.replies": func(url.Values) string {
			return `{"ok":true,"messages":[{"ts":"` + current + `","user":"U1"}]}`
		},
		"reactions.add":    func(url.Values) string { return `{"ok":true}` },
		"reactions.remove": func(url.Values) string { return `{"ok":true}` },
	})
	state.markPosted(daemon)
	mux := slack.NewMultiPlatform(discardLogger(), nil, map[string]bot.Platform{"D1": surface.plat})
	message := bot.Message{
		Thread: bot.Thread{Workspace: "T1", Channel: "D1", ThreadTS: current},
		UserID: "U1", Workspace: "T1", TS: current, Mentioned: true, Direct: true,
	}
	if err := mux.StartRequest(context.Background(), message, false); err != nil {
		t.Fatalf("StartRequest through mux: %v", err)
	}
	if stub.count("conversations.history") != 2 {
		t.Fatalf("history calls = %d, want startup probe plus the prior-root scan", stub.count("conversations.history"))
	}
	if got := stub.params("conversations.history", 1).Get("latest"); got != current {
		t.Errorf("history latest = %q, want current root %q", got, current)
	}
	if stub.count("reactions.remove") != 1 || stub.params("reactions.remove", 0).Get("timestamp") != prior || stub.params("reactions.remove", 0).Get("name") != "warning" {
		t.Errorf("reaction removals = %+v, want only the recent prior user root; near-boundary roots are skipped with the ledger safety grace", recordedCalls(stub))
	}
}

func selfDMTestTS(t time.Time) string {
	return fmt.Sprintf("%d.%06d", t.Unix(), t.Nanosecond()/1000)
}

func recordedCalls(stub *stubSlack) []stubCall {
	stub.mu.Lock()
	defer stub.mu.Unlock()
	return append([]stubCall(nil), stub.calls...)
}

type postedLedgerError struct {
	*memState
	ts string
}

func (s *postedLedgerError) SurfacePosted(ctx context.Context, surface, ts string) (bool, error) {
	if ts == s.ts {
		return false, errors.New("posted ledger unavailable")
	}
	return s.memState.SurfacePosted(ctx, surface, ts)
}

// TestPostTurnsButtonsIntoNumbers is this surface's whole interactivity: a
// notice's buttons become a numbered list, and a bare number in a reply becomes
// the press the surface cannot receive.
func TestPostTurnsButtonsIntoNumbers(t *testing.T) {
	s, _, stub, st := surfaceFor(t, map[string]func(url.Values) string{
		"chat.postMessage": postCounter(),
		"chat.update":      func(url.Values) string { return `{"ok":true}` },
	})
	ctx := context.Background()

	if err := s.plat.Post(ctx, bot.Notice{
		Channel: "D1",
		Text:    "which session?",
		Buttons: []bot.Button{
			{ActionID: bot.ActionResume, Text: "session A", Value: "/a.jsonl"},
			{ActionID: bot.ActionResume, Text: "session B", Value: "/b.jsonl"},
		},
	}); err != nil {
		t.Fatalf("Post: %v", err)
	}

	text := stub.params("chat.postMessage", 0).Get("text")
	for _, want := range []string{"which session?", "*1)* session A", "*2)* session B", "Reply with a number"} {
		if !strings.Contains(text, want) {
			t.Errorf("posted text should contain %q, got:\n%s", want, text)
		}
	}
	if posted := st.posted[SurfaceName+"/9.1"]; !posted {
		t.Error("the posted message was not recorded in the ledger; it would come back as input")
	}

	action, ok := s.plat.TakeAction(ctx, "D1", "", "U1", "T1", "2")
	if !ok {
		t.Fatal("a number answering an open offer must become an action")
	}
	if action.ActionID != bot.ActionResume || action.Value != "/b.jsonl" || action.MessageTS != "9.1" {
		t.Errorf("action = %+v, want the second resume button on message 9.1", action)
	}
	if action.EventID == "" || action.Channel != "D1" || !action.Direct {
		t.Errorf("action = %+v, want a direct action in D1 with an event id of its own", action)
	}
	if _, ok := s.plat.TakeAction(ctx, "D1", "", "U1", "T1", "2"); ok {
		t.Error("the offer must be spent after it is answered")
	}
}

func TestOpenThreadPostsPlainTextWithDynamicSlackSyntax(t *testing.T) {
	s, _, stub, st := surfaceFor(t, map[string]func(url.Values) string{
		"chat.postMessage": postCounter(),
	})
	text := "Resuming <!channel> *bold* `code` <https://example.test|link>"

	thread, err := s.plat.OpenThread(context.Background(), "D1", text)
	if err != nil {
		t.Fatalf("OpenThread: %v", err)
	}
	if thread.ThreadTS != "9.1" {
		t.Fatalf("thread root = %q, want posted timestamp 9.1", thread.ThreadTS)
	}
	params := stub.params("chat.postMessage", 0)
	if params.Get("text") != text {
		t.Errorf("OpenThread changed the plain text: %q", params.Get("text"))
	}
	if params.Get("mrkdwn") != "false" || params.Get("parse") != "none" {
		t.Errorf("OpenThread left Slack parsing enabled: %v", params)
	}
	if !st.posted[SurfaceName+"/9.1"] {
		t.Error("the plain-text thread root was not recorded in the surface ledger")
	}
}

// TestTakeActionAcceptsTheButtonText: a number is the hint, but the button's own
// words are just as unambiguous.
func TestTakeActionAcceptsTheButtonText(t *testing.T) {
	s, _, _, _ := surfaceFor(t, map[string]func(url.Values) string{
		"chat.postMessage": postCounter(),
	})
	if err := s.plat.Post(context.Background(), bot.Notice{
		Channel: "D1",
		Text:    "delete it?",
		Buttons: []bot.Button{
			{ActionID: bot.ActionDelete, Text: "Delete", Value: "now"},
			{ActionID: bot.ActionDeleteCancel, Text: "Cancel", Value: "cancel"},
		},
	}); err != nil {
		t.Fatalf("Post: %v", err)
	}
	action, ok := s.plat.TakeAction(context.Background(), "D1", "", "U1", "T1", "cancel")
	if !ok || action.ActionID != bot.ActionDeleteCancel {
		t.Errorf("TakeAction(cancel) = (%+v, %v), want the cancel button", action, ok)
	}
}

// TestTakeActionExpiresAnOldOffer: a number typed an hour later must not fire a
// button nobody remembers offering.
func TestTakeActionExpiresAnOldOffer(t *testing.T) {
	s, _, _, _ := surfaceFor(t, nil)
	s.plat.remember("D1\x00", &choice{
		buttons:   []bot.Button{{ActionID: bot.ActionDelete, Text: "Delete", Value: "now"}},
		messageTS: "9.1",
		at:        time.Now().Add(-2 * choiceTTL),
	})
	if _, ok := s.plat.TakeAction(context.Background(), "D1", "", "U1", "T1", "1"); ok {
		t.Error("an expired offer must not answer")
	}
}

// TestPostForgetsAnOfferWhenItIsReplaced: the core replaces a picker with the
// outcome of the choice; the spent message must stop answering numbers.
func TestPostForgetsAnOfferWhenItIsReplaced(t *testing.T) {
	s, _, _, _ := surfaceFor(t, map[string]func(url.Values) string{
		"chat.postMessage": postCounter(),
		"chat.update":      func(url.Values) string { return `{"ok":true}` },
	})
	ctx := context.Background()
	buttons := []bot.Button{{ActionID: bot.ActionResume, Text: "session A", Value: "/a.jsonl"}}
	if err := s.plat.Post(ctx, bot.Notice{Channel: "D1", Text: "pick", Buttons: buttons}); err != nil {
		t.Fatalf("Post: %v", err)
	}
	if err := s.plat.Post(ctx, bot.Notice{Channel: "D1", Update: "9.1", Text: "adopted session A"}); err != nil {
		t.Fatalf("Post (update): %v", err)
	}
	if _, ok := s.plat.TakeAction(ctx, "D1", "", "U1", "T1", "1"); ok {
		t.Error("a replaced picker must not keep answering numbers")
	}
}

// TestPostPostsWhatAnInteractionWouldHaveAnswered: there is no response_url
// here, so a notice that carries one is posted rather than dropped.
func TestPostPostsWhatAnInteractionWouldHaveAnswered(t *testing.T) {
	s, _, stub, _ := surfaceFor(t, map[string]func(url.Values) string{
		"chat.postMessage": postCounter(),
	})
	err := s.plat.Post(context.Background(), bot.Notice{
		Channel:   "D1",
		UserID:    "U1",
		Ephemeral: true,
		ReplyTo:   "https://hooks.slack.com/actions/response",
		Text:      "nothing to delete here",
	})
	if err != nil {
		t.Fatalf("Post: %v", err)
	}
	if n := stub.count("chat.postMessage"); n != 1 {
		t.Fatalf("chat.postMessage called %d times, want 1", n)
	}
	if got := stub.params("chat.postMessage", 0).Get("text"); got != "nothing to delete here" {
		t.Errorf("posted text = %q, want the notice text", got)
	}
}

// TestRendererPatchesTheSameMessage is the self-DM's rendering contract: one
// placeholder message per turn, replaced in place as the answer grows, and
// recorded so the poller knows whose it is.
func TestRendererPatchesTheSameMessage(t *testing.T) {
	s, _, stub, st := surfaceFor(t, map[string]func(url.Values) string{
		"chat.postMessage": postCounter(),
		"chat.update":      func(url.Values) string { return `{"ok":true}` },
	})
	ctx := context.Background()
	// Streaming updates are coalesced by a timer; keep the flush out of the way
	// so the assertions are about the rendering and not about a race with it.
	s.plat.flush = time.Hour

	r, err := s.plat.StartTurn(ctx, bot.Message{Thread: bot.Thread{Channel: "D1", ThreadTS: "5.0"}})
	if err != nil {
		t.Fatalf("StartTurn: %v", err)
	}
	if err := r.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if got := stub.params("chat.postMessage", 0).Get("text"); got != placeholder {
		t.Errorf("the placeholder = %q, want %q", got, placeholder)
	}
	if got := stub.params("chat.postMessage", 0).Get("thread_ts"); got != "5.0" {
		t.Errorf("the reply was posted in thread %q, want 5.0", got)
	}
	progress, ok := r.(bot.ProgressReporter)
	if !ok {
		t.Fatal("the renderer must name the message it writes into, so a restart can find it")
	}
	if progress.Progress() != "9.1" {
		t.Errorf("Progress = %q, want the placeholder's ts", progress.Progress())
	}
	if !st.posted[SurfaceName+"/9.1"] {
		t.Error("the placeholder was not recorded in the ledger")
	}

	if err := r.Delta(ctx, "first "); err != nil {
		t.Fatalf("Delta: %v", err)
	}
	if err := r.Delta(ctx, "second"); err != nil {
		t.Fatalf("Delta: %v", err)
	}
	if err := r.Finish(ctx, "first second"); err != nil {
		t.Fatalf("Finish: %v", err)
	}
	if n := stub.count("chat.update"); n != 1 {
		t.Fatalf("chat.update called %d times, want exactly one replacement", n)
	}
	if got := stub.params("chat.update", 0).Get("ts"); got != "9.1" {
		t.Errorf("chat.update target = %q, want the placeholder's ts 9.1", got)
	}
	if got := stub.params("chat.update", 0).Get("text"); got != "first second" {
		t.Errorf("chat.update text = %q, want the final answer", got)
	}
}

func TestFinishWithoutStartPostsTheReadyAnswer(t *testing.T) {
	s, _, stub, st := surfaceFor(t, map[string]func(url.Values) string{
		"chat.postMessage": postCounter(),
		"chat.update":      func(url.Values) string { return `{"ok":true}` },
	})
	ctx := context.Background()
	r, err := s.plat.StartTurn(ctx, bot.Message{Thread: bot.Thread{Channel: "D1", ThreadTS: "5.0"}})
	if err != nil {
		t.Fatalf("StartTurn: %v", err)
	}
	answer := "The answer is ready."
	if err := r.Finish(ctx, answer); err != nil {
		t.Fatalf("Finish: %v", err)
	}

	if got := stub.count("chat.postMessage"); got != 1 {
		t.Fatalf("chat.postMessage called %d times, want one final answer", got)
	}
	if got := stub.count("chat.update"); got != 0 {
		t.Fatalf("chat.update called %d times, want no empty placeholder update", got)
	}
	params := stub.params("chat.postMessage", 0)
	if params.Get("thread_ts") != "5.0" || params.Get("text") != answer {
		t.Errorf("posted answer = %+v, want thread_ts=5.0 and text=%q", params, answer)
	}
	if !st.posted[SurfaceName+"/9.1"] {
		t.Error("final answer was not recorded in the surface ledger")
	}
}

// TestRendererSplitsALongAnswer: truncating the end of an agent's work is worse
// than a second message, so a long answer continues in a new one.
func TestRendererSplitsALongAnswer(t *testing.T) {
	s, _, stub, st := surfaceFor(t, map[string]func(url.Values) string{
		"chat.postMessage": postCounter(),
		"chat.update":      func(url.Values) string { return `{"ok":true}` },
	})
	ctx := context.Background()
	s.plat.flush = time.Hour

	r, err := s.plat.StartTurn(ctx, bot.Message{Thread: bot.Thread{Channel: "D1", ThreadTS: "5.0"}})
	if err != nil {
		t.Fatalf("StartTurn: %v", err)
	}
	if err := r.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	long := strings.Repeat("x", maxMessage+10)
	if err := r.Finish(ctx, long); err != nil {
		t.Fatalf("Finish: %v", err)
	}

	if n := stub.count("chat.update"); n != 1 {
		t.Fatalf("chat.update called %d times, want 1", n)
	}
	if got := len(stub.params("chat.update", 0).Get("text")); got != maxMessage {
		t.Errorf("the first message carries %d bytes, want %d", got, maxMessage)
	}
	if n := stub.count("chat.postMessage"); n != 2 {
		t.Fatalf("chat.postMessage called %d times, want the placeholder and one continuation", n)
	}
	if got := stub.params("chat.postMessage", 1).Get("text"); got != strings.Repeat("x", 10) {
		t.Errorf("the continuation = %q…, want the remaining 10 bytes", got[:min(20, len(got))])
	}
	if !st.posted[SurfaceName+"/9.2"] {
		t.Error("the continuation was not recorded in the ledger")
	}
}

// TestFailKeepsThePartialAnswer: a turn that failed after saying something must
// say both.
func TestFailKeepsThePartialAnswer(t *testing.T) {
	s, _, stub, _ := surfaceFor(t, map[string]func(url.Values) string{
		"chat.postMessage": postCounter(),
		"chat.update":      func(url.Values) string { return `{"ok":true}` },
	})
	ctx := context.Background()
	s.plat.flush = time.Hour

	r, err := s.plat.StartTurn(ctx, bot.Message{Thread: bot.Thread{Channel: "D1", ThreadTS: "5.0"}})
	if err != nil {
		t.Fatalf("StartTurn: %v", err)
	}
	if err := r.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if err := r.Delta(ctx, "I was saying"); err != nil {
		t.Fatalf("Delta: %v", err)
	}
	if err := r.Fail(ctx, errNotEnough); err != nil {
		t.Fatalf("Fail: %v", err)
	}
	got := stub.params("chat.update", 0).Get("text")
	if !strings.Contains(got, "I was saying") || !strings.Contains(got, "not enough") {
		t.Errorf("the failure message = %q, want the partial answer and the cause", got)
	}
}

func TestFailKeepsCauseWhenPartialAnswerIsLong(t *testing.T) {
	s, _, stub, _ := surfaceFor(t, map[string]func(url.Values) string{
		"chat.postMessage": postCounter(),
		"chat.update":      func(url.Values) string { return `{"ok":true}` },
	})
	ctx := context.Background()
	s.plat.flush = time.Hour
	r, err := s.plat.StartTurn(ctx, bot.Message{Thread: bot.Thread{Channel: "D1", ThreadTS: "5.0"}})
	if err != nil {
		t.Fatalf("StartTurn: %v", err)
	}
	if err := r.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	r.(*renderer).text.WriteString(strings.Repeat("p", maxMessage))
	cause := fmt.Errorf("create the session: pi binary not found")
	if err := r.Fail(ctx, cause); err != nil {
		t.Fatalf("Fail: %v", err)
	}

	text := stub.params("chat.update", 0).Get("text")
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

func TestFailPreservesTextWhenNoReplyWasStarted(t *testing.T) {
	s, _, stub, _ := surfaceFor(t, map[string]func(url.Values) string{
		"chat.postMessage": postCounter(),
	})
	ctx := context.Background()
	r, err := s.plat.StartTurn(ctx, bot.Message{Thread: bot.Thread{Channel: "D1", ThreadTS: "5.0"}})
	if err != nil {
		t.Fatalf("StartTurn: %v", err)
	}
	if err := r.Delta(ctx, "partial answer"); err != nil {
		t.Fatalf("Delta before Start: %v", err)
	}
	if got := stub.count("chat.postMessage"); got != 0 {
		t.Fatalf("chat.postMessage called %d times while buffering", got)
	}
	cause := errors.New("Slack was unavailable when the reply started")
	if err := r.Fail(ctx, cause); err != nil {
		t.Fatalf("Fail: %v", err)
	}
	text := stub.params("chat.postMessage", 0).Get("text")
	if !strings.Contains(text, "partial answer") || !strings.Contains(text, cause.Error()) {
		t.Errorf("failure text = %q, want partial answer and cause", text)
	}
}

func TestFailBeforeStartPostsFailureInThread(t *testing.T) {
	s, _, stub, st := surfaceFor(t, map[string]func(url.Values) string{
		"chat.postMessage": postCounter(),
		"chat.update":      func(url.Values) string { return `{"ok":true}` },
	})
	ctx := context.Background()
	r, err := s.plat.StartTurn(ctx, bot.Message{Thread: bot.Thread{Channel: "D1", ThreadTS: "5.0"}})
	if err != nil {
		t.Fatalf("StartTurn: %v", err)
	}
	cause := "create the session: gwclient: find pi binary: no such file"
	if err := r.Fail(ctx, fmt.Errorf("%s", cause)); err != nil {
		t.Fatalf("Fail: %v", err)
	}

	if got := stub.count("chat.postMessage"); got != 1 {
		t.Fatalf("chat.postMessage called %d times, want one fallback", got)
	}
	if got := stub.count("chat.update"); got != 0 {
		t.Fatalf("chat.update called %d times, want none", got)
	}
	if got := stub.params("chat.postMessage", 0).Get("thread_ts"); got != "5.0" {
		t.Errorf("thread_ts = %q, want 5.0", got)
	}
	if got, want := stub.params("chat.postMessage", 0).Get("text"), ":warning: pi-chat: "+cause; got != want {
		t.Errorf("failure text = %q, want %q", got, want)
	}
	if !st.posted[SurfaceName+"/9.1"] {
		t.Error("fallback failure message was not recorded in the surface ledger")
	}
}

func TestFailDeletedMessagePostsFailureInThread(t *testing.T) {
	s, _, stub, _ := surfaceFor(t, map[string]func(url.Values) string{
		"chat.postMessage": postCounter(),
		"chat.update":      func(url.Values) string { return `{"ok":false,"error":"message_not_found"}` },
	})
	ctx := context.Background()
	s.plat.flush = time.Hour
	r, err := s.plat.StartTurn(ctx, bot.Message{Thread: bot.Thread{Channel: "D1", ThreadTS: "5.0"}})
	if err != nil {
		t.Fatalf("StartTurn: %v", err)
	}
	if err := r.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	r.(*renderer).text.WriteString(strings.Repeat("p", maxMessage))
	if err := r.Fail(ctx, errNotEnough); err != nil {
		t.Fatalf("Fail: %v", err)
	}

	if got := stub.count("chat.postMessage"); got != 2 {
		t.Fatalf("chat.postMessage called %d times, want placeholder and fallback", got)
	}
	if got := stub.params("chat.postMessage", 1).Get("thread_ts"); got != "5.0" {
		t.Errorf("fallback thread_ts = %q, want 5.0", got)
	}
	text := stub.params("chat.postMessage", 1).Get("text")
	if !strings.Contains(text, errNotEnough.Error()) || !strings.Contains(text, "truncated") {
		t.Errorf("fallback text = %q, want truncated partial answer and full cause", text)
	}
	if len(text) > maxMessage {
		t.Errorf("fallback text is %d bytes, exceeds %d", len(text), maxMessage)
	}
}

// errNotEnough stands in for a gateway failure.
var errNotEnough = fmt.Errorf("not enough context")
