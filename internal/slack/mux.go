package slack

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/tigersoldier/pi-chat/internal/bot"
)

// MultiPlatform is the adapter half of the core/adapter split when more than one
// Slack surface is running in the same daemon (DESIGN.md §12): the app surface
// and the self-DM surface both answer to one core, and this is what decides
// which of them owns a call.
//
// Routing is by channel id and nothing else. A thread's channel is its address
// on both surfaces — the bot's DM with you and your DM with yourself are
// different `D…` conversations — so one configured self-DM channel id is enough
// to keep the two apart, and every other channel goes to the app surface.
type MultiPlatform struct {
	log    *slog.Logger
	def    bot.Platform            // the app surface; nil when it is disabled
	routes map[string]bot.Platform // channel id -> surface
}

// NewMultiPlatform builds the mux. def may be nil, and so may any route's
// platform; an empty channel id is ignored rather than becoming a route
// everything falls into.
func NewMultiPlatform(log *slog.Logger, def bot.Platform, routes map[string]bot.Platform) *MultiPlatform {
	clean := make(map[string]bot.Platform, len(routes))
	for channel, platform := range routes {
		if channel == "" || platform == nil {
			continue
		}
		clean[channel] = platform
	}
	return &MultiPlatform{log: log, def: def, routes: clean}
}

// StartTurn builds the renderer for a reply in the message's own channel.
func (m *MultiPlatform) StartTurn(ctx context.Context, msg bot.Message) (bot.Renderer, error) {
	platform, err := m.forChannel(msg.Thread.Channel)
	if err != nil {
		return nil, err
	}
	return platform.StartTurn(ctx, msg)
}

// Post posts a notice through the surface that owns the notice's channel. A
// notice that names only its thread still names its channel, which is the same
// rule the app adapter applies for the same reason.
func (m *MultiPlatform) Post(ctx context.Context, n bot.Notice) error {
	channel := n.Channel
	if channel == "" && n.Thread != nil {
		channel = n.Thread.Channel
	}
	platform, err := m.forChannel(channel)
	if err != nil {
		return err
	}
	return platform.Post(ctx, n)
}

// AcknowledgeRequest reacts to the message through the surface that owns it.
func (m *MultiPlatform) AcknowledgeRequest(ctx context.Context, msg bot.Message, allowed bool) error {
	platform, err := m.forChannel(msg.Thread.Channel)
	if err != nil {
		return err
	}
	reporter, ok := platform.(bot.RequestAcknowledger)
	if !ok {
		return nil
	}
	return reporter.AcknowledgeRequest(ctx, msg, allowed)
}

// OpenThread opens the thread on the surface that owns the channel it is
// opened in.
func (m *MultiPlatform) OpenThread(ctx context.Context, channel, text string) (bot.Thread, error) {
	platform, err := m.forChannel(channel)
	if err != nil {
		return bot.Thread{}, err
	}
	return platform.OpenThread(ctx, channel, text)
}

// Optional capabilities below are claimed by the mux on behalf of the
// surfaces, because the core asks the platform it holds and cannot ask per
// thread (DESIGN.md §12). A surface that does not implement one means "nothing
// to do here", not "failed": there is no conversation to read back on the
// self-DM, no status indicator, and no agent surface for suggestions, and the
// empty answer is exactly the behaviour the core gets from a platform that
// never implemented the interface at all.

// Conversation reads a thread back through its surface. A surface that cannot
// read threads back reports an empty conversation, which is what the core would
// have seen had it not claimed the interface.
func (m *MultiPlatform) Conversation(ctx context.Context, t bot.Thread, oldest string) ([]bot.Said, error) {
	platform, err := m.forChannel(t.Channel)
	if err != nil {
		return nil, err
	}
	observer, ok := platform.(bot.Observer)
	if !ok {
		return nil, nil
	}
	return observer.Conversation(ctx, t, oldest)
}

// SetStatus displays a lifecycle state where the owning surface can show one.
func (m *MultiPlatform) SetStatus(ctx context.Context, t bot.Thread, s bot.Status) error {
	platform, err := m.forChannel(t.Channel)
	if err != nil {
		return err
	}
	reporter, ok := platform.(bot.StatusReporter)
	if !ok {
		return nil
	}
	return reporter.SetStatus(ctx, t, s)
}

// SetSuggestedPrompts offers prompts where the owning surface has somewhere to
// show them.
func (m *MultiPlatform) SetSuggestedPrompts(ctx context.Context, channel, title string, suggestions []bot.Suggestion) error {
	platform, err := m.forChannel(channel)
	if err != nil {
		return err
	}
	reporter, ok := platform.(bot.PromptReporter)
	if !ok {
		return nil
	}
	return reporter.SetSuggestedPrompts(ctx, channel, title, suggestions)
}

// forChannel resolves the surface that owns one channel.
func (m *MultiPlatform) forChannel(channel string) (bot.Platform, error) {
	if platform, ok := m.routes[channel]; ok {
		return platform, nil
	}
	if m.def != nil {
		return m.def, nil
	}
	return nil, fmt.Errorf("slack: no surface is configured for channel %s", channel)
}

// ErrNoSurface reports that every configured surface is unavailable. It is what
// the daemon fails with when the app's tokens are missing and the self-DM was
// never turned on.
var ErrNoSurface = errors.New("slack: no surface is available")
