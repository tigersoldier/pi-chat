// Package bot is pi-chat's platform-independent core (DESIGN.md §5): it maps a
// chat thread onto a pi-gateway session, drives turns, and renders the answer
// through a Platform.
//
// The dependency direction is one-way. Adapters (internal/slack) import this
// package and implement Platform; this package never imports an adapter, so
// adding a second chat platform means adding an adapter, not touching the core.
package bot

import (
	"context"
	"errors"
	"log/slog"
	"slices"
	"sync"

	"github.com/tigersoldier/pi-chat/internal/config"
)

// ErrNotAllowed reports a message from a user or channel outside the
// allowlist.
var ErrNotAllowed = errors.New("not on the allowlist")

// Bot owns the thread→session map and drives turns.
type Bot struct {
	cfg  *config.Config
	log  *slog.Logger
	plat Platform

	mu      sync.Mutex
	threads map[string]*thread
	seen    *seenSet
}

// New builds a Bot. The caller keeps ownership of cfg and plat.
func New(cfg *config.Config, log *slog.Logger, plat Platform) *Bot {
	return &Bot{
		cfg:     cfg,
		log:     log,
		plat:    plat,
		threads: make(map[string]*thread),
		seen:    newSeenSet(4096),
	}
}

// HandleMessage accepts one inbound message and runs its turn in the
// background, so the caller can acknowledge the platform immediately
// (DESIGN.md §4: the ack budget is three seconds).
func (b *Bot) HandleMessage(ctx context.Context, m Message) {
	switch {
	case !b.seen.claim(m.EventID):
		b.log.Debug("duplicate event ignored", "event", m.EventID, "thread", m.Thread.Key())
		return
	case !b.allowed(m):
		// Deny by default, and before any side effect (DESIGN.md §10). Phase 0
		// stays silent; M4 adds the reply that tells the user why.
		b.log.Warn("refused a message from outside the allowlist",
			"user", m.UserID, "channel", m.Thread.Channel)
		return
	}
	go b.run(ctx, m)
}

// allowed applies the platform's allowlist: a user must always be listed, and
// a non-empty channel list additionally restricts where the bot will work.
func (b *Bot) allowed(m Message) bool {
	if !slices.Contains(b.cfg.Slack.Access.AllowedUsers, m.UserID) {
		return false
	}
	channels := b.cfg.Slack.Access.AllowedChannels
	return len(channels) == 0 || slices.Contains(channels, m.Thread.Channel)
}

// run renders one message's turn.
func (b *Bot) run(ctx context.Context, m Message) {
	th := b.threadFor(m.Thread)
	r, err := b.plat.StartTurn(ctx, m)
	if err != nil {
		b.log.Error("cannot start the reply", "thread", th.key, "error", err)
		return
	}
	if err := th.runTurn(ctx, m, r); err != nil {
		b.log.Warn("turn failed", "thread", th.key, "error", err)
		if ferr := r.Fail(ctx, err); ferr != nil {
			b.log.Error("cannot report the failure", "thread", th.key, "error", ferr)
		}
	}
}

// threadFor returns the thread's agent, creating it on first use.
func (b *Bot) threadFor(t Thread) *thread {
	b.mu.Lock()
	defer b.mu.Unlock()
	if th, ok := b.threads[t.Key()]; ok {
		return th
	}
	th := &thread{
		key: t.Key(),
		t:   t,
		b:   b,
		log: b.log.With("thread", t.Key()),
	}
	b.threads[t.Key()] = th
	b.log.Debug("tracking a new thread", "thread", th.key, "session", t.SessionName())
	return th
}

// Close ends every gateway connection. Phase 0 leaves sessions running: they
// are pi's own state, and M4 hibernates them deliberately instead.
func (b *Bot) Close() {
	b.mu.Lock()
	threads := make([]*thread, 0, len(b.threads))
	for _, th := range b.threads {
		threads = append(threads, th)
	}
	b.mu.Unlock()
	for _, th := range threads {
		th.close()
	}
}
