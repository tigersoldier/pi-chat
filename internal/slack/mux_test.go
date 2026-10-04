package slack

import (
	"context"
	"strings"
	"testing"

	"github.com/tigersoldier/pi-chat/internal/bot"
)

// fakePlatform records what the mux sent it, so a routing test can assert on the
// destination instead of on an API call.
type fakePlatform struct {
	turns    []bot.Message
	notices  []bot.Notice
	opened   []string
	observed []bot.Said
	statuses []bot.Status
	prompts  []string
}

func (f *fakePlatform) StartTurn(_ context.Context, m bot.Message) (bot.Renderer, error) {
	f.turns = append(f.turns, m)
	return fakeRenderer{}, nil
}

func (f *fakePlatform) Post(_ context.Context, n bot.Notice) error {
	f.notices = append(f.notices, n)
	return nil
}

func (f *fakePlatform) OpenThread(_ context.Context, channel, text string) (bot.Thread, error) {
	f.opened = append(f.opened, channel)
	return bot.Thread{Workspace: "T1", Channel: channel, ThreadTS: "1.1"}, nil
}

// fakeRenderer is the do-nothing renderer every fake surface hands back.
type fakeRenderer struct{}

func (fakeRenderer) Start(context.Context) error          { return nil }
func (fakeRenderer) Delta(context.Context, string) error  { return nil }
func (fakeRenderer) Finish(context.Context, string) error { return nil }
func (fakeRenderer) Fail(context.Context, error) error    { return nil }

// capablePlatform adds the optional capabilities the mux has to delegate.
type capablePlatform struct {
	fakePlatform
}

func (c *capablePlatform) Conversation(_ context.Context, _ bot.Thread, _ string) ([]bot.Said, error) {
	return c.observed, nil
}

func (c *capablePlatform) SetStatus(_ context.Context, _ bot.Thread, s bot.Status) error {
	c.statuses = append(c.statuses, s)
	return nil
}

func (c *capablePlatform) SetSuggestedPrompts(_ context.Context, _, title string, _ []bot.Suggestion) error {
	c.prompts = append(c.prompts, title)
	return nil
}

// TestMuxRoutesByChannel is the whole rule: the configured self-DM channel goes
// to the self-DM surface, and every other channel goes to the app.
func TestMuxRoutesByChannel(t *testing.T) {
	ctx := context.Background()
	app := &fakePlatform{}
	dm := &fakePlatform{}
	mux := NewMultiPlatform(discardLogger(), app, map[string]bot.Platform{"D1": dm})

	if _, err := mux.StartTurn(ctx, bot.Message{Thread: bot.Thread{Channel: "D1", ThreadTS: "1.1"}}); err != nil {
		t.Fatalf("StartTurn in the self-DM: %v", err)
	}
	if _, err := mux.StartTurn(ctx, bot.Message{Thread: bot.Thread{Channel: "C9", ThreadTS: "2.2"}}); err != nil {
		t.Fatalf("StartTurn in a channel: %v", err)
	}
	// A notice that names only its thread still names its channel.
	if err := mux.Post(ctx, bot.Notice{Thread: &bot.Thread{Channel: "D1", ThreadTS: "1.1"}}); err != nil {
		t.Fatalf("Post into the self-DM thread: %v", err)
	}
	// A root notice names the channel directly.
	if err := mux.Post(ctx, bot.Notice{Channel: "C9"}); err != nil {
		t.Fatalf("Post into a channel root: %v", err)
	}
	if _, err := mux.OpenThread(ctx, "D1", "adopting"); err != nil {
		t.Fatalf("OpenThread in the self-DM: %v", err)
	}

	if len(dm.turns) != 1 || dm.turns[0].Thread.Channel != "D1" {
		t.Errorf("self-DM turns = %+v, want exactly the D1 message", dm.turns)
	}
	if len(dm.notices) != 1 || dm.notices[0].Thread.Channel != "D1" {
		t.Errorf("self-DM notices = %+v, want exactly the D1 notice", dm.notices)
	}
	if len(dm.opened) != 1 {
		t.Errorf("self-DM OpenThread calls = %v, want one", dm.opened)
	}
	if len(app.turns) != 1 || app.turns[0].Thread.Channel != "C9" {
		t.Errorf("app turns = %+v, want exactly the C9 message", app.turns)
	}
	if len(app.notices) != 1 || app.notices[0].Channel != "C9" {
		t.Errorf("app notices = %+v, want exactly the C9 notice", app.notices)
	}
}

// TestMuxWithoutADefaultRefusesAnUnknownChannel: with only the self-DM running,
// a call for any other channel is a bug in the wiring, not something to guess
// about.
func TestMuxWithoutADefaultRefusesAnUnknownChannel(t *testing.T) {
	ctx := context.Background()
	dm := &fakePlatform{}
	mux := NewMultiPlatform(discardLogger(), nil, map[string]bot.Platform{"D1": dm})

	_, err := mux.StartTurn(ctx, bot.Message{Thread: bot.Thread{Channel: "C9"}})
	if err == nil {
		t.Fatal("want an error for a channel no surface owns")
	}
	if !strings.Contains(err.Error(), "C9") {
		t.Errorf("the error should name the channel, got: %v", err)
	}
	if err := mux.Post(ctx, bot.Notice{Channel: "C9"}); err == nil {
		t.Error("Post must refuse an unowned channel too")
	}
}

// TestMuxCapabilitiesFollowTheSurface: the mux claims the optional interfaces on
// behalf of surfaces that have them, and reports the empty answer — not an error
// — for surfaces that do not. An empty conversation and no status are exactly
// what a surface without the interface would have given the core.
func TestMuxCapabilitiesFollowTheSurface(t *testing.T) {
	ctx := context.Background()
	app := &fakePlatform{}
	dm := &capablePlatform{}
	dm.observed = []bot.Said{{TS: "1.0", Text: "hello"}}
	mux := NewMultiPlatform(discardLogger(), app, map[string]bot.Platform{"D1": dm})

	said, err := mux.Conversation(ctx, bot.Thread{Channel: "D1"}, "")
	if err != nil || len(said) != 1 {
		t.Fatalf("Conversation on the capable surface = (%+v, %v), want its own answer", said, err)
	}
	said, err = mux.Conversation(ctx, bot.Thread{Channel: "C9"}, "")
	if err != nil || said != nil {
		t.Fatalf("Conversation on a surface that cannot read threads = (%+v, %v), want (nil, nil)", said, err)
	}

	if err := mux.SetStatus(ctx, bot.Thread{Channel: "D1"}, bot.StatusBusy); err != nil {
		t.Fatalf("SetStatus on the capable surface: %v", err)
	}
	if len(dm.statuses) != 1 {
		t.Errorf("statuses = %v, want one", dm.statuses)
	}
	if err := mux.SetStatus(ctx, bot.Thread{Channel: "C9"}, bot.StatusBusy); err != nil {
		t.Fatalf("SetStatus on a surface that cannot show one must be a no-op, got: %v", err)
	}

	if err := mux.SetSuggestedPrompts(ctx, "D1", "try these", nil); err != nil {
		t.Fatalf("SetSuggestedPrompts on the capable surface: %v", err)
	}
	if len(dm.prompts) != 1 {
		t.Errorf("prompts = %v, want one", dm.prompts)
	}
	if err := mux.SetSuggestedPrompts(ctx, "C9", "try these", nil); err != nil {
		t.Errorf("SetSuggestedPrompts on a surface without one must be a no-op, got: %v", err)
	}
}

// TestMuxIgnoresEmptyRoutes: an unset self-DM channel id must not become a route
// that swallows every channel.
func TestMuxIgnoresEmptyRoutes(t *testing.T) {
	app := &fakePlatform{}
	mux := NewMultiPlatform(discardLogger(), app, map[string]bot.Platform{"": &capablePlatform{}, "D1": nil})
	if _, err := mux.StartTurn(context.Background(), bot.Message{Thread: bot.Thread{Channel: "C9"}}); err != nil {
		t.Fatalf("an empty route must not capture other channels: %v", err)
	}
	if len(app.turns) != 1 {
		t.Errorf("app turns = %+v, want the C9 message", app.turns)
	}
}
