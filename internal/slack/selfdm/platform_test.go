package selfdm

import (
	"context"
	"fmt"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tigersoldier/pi-chat/internal/bot"
)

// postCounter answers chat.postMessage with a fresh timestamp per call, the way
// Slack does, so a test can tell a replacement from a continuation.
func postCounter() func(url.Values) string {
	var n atomic.Int32
	return func(url.Values) string {
		return fmt.Sprintf(`{"ok":true,"ts":"9.%d"}`, n.Add(1))
	}
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

// errNotEnough stands in for a gateway failure.
var errNotEnough = fmt.Errorf("not enough context")
