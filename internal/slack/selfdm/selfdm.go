// Package selfdm is pi-chat's Slack self-DM surface (DESIGN.md §12).
//
// It drives pi sessions from the conversation you have with yourself in Slack
// ("Notes to self"), using your own browser session credentials instead of an
// installed app: an `xoxc-` token and the `d` cookie, and the conversation is
// read by polling rather than pushed by Socket Mode.
//
// What this surface is not, and the reason the daemon asks for it explicitly:
//
//   - It cannot notify. Every message in the self-DM is authored by you, so
//     Slack never marks it unread and never notifies — the answer is there when
//     you look, and nowhere else.
//   - It has no slash commands, no buttons and no status, because those belong
//     to an installed app. Commands are typed as `/pi …` and a numbered reply
//     stands in for a button press.
//   - The credentials are an account-wide secret, and Slack documents that it
//     detects non-official clients. docs/self-dm-feasibility.md is the full
//     account of the cost; it is not a surface to enable on a work workspace.
//
// The mechanics are deliberately boring: one durable cursor per conversation,
// one per thread that has replies, a ledger of what the daemon posted (both
// users are the same person, so authorship cannot tell them apart), and the
// core's own claim table as the net under a redelivery.
package selfdm

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/tigersoldier/pi-chat/internal/bot"
	"github.com/tigersoldier/pi-chat/internal/slack"
)

const (
	// SurfaceName namespaces this surface's durable state in the store.
	SurfaceName = "self_dm"

	// cursorKey is the conversation cursor: the newest message the poller has
	// handled. It is separate from the per-thread cursors below because the
	// conversation and each of its threads advance at their own pace.
	cursorKey = "cursor"
	// replyKeyPrefix namespaces one thread's reply cursor. Threads interleave in
	// time, so a single conversation-wide cursor would skip a late reply in an
	// older thread; one cursor per thread is what makes that impossible.
	replyKeyPrefix = "reply:"

	// historyPagesPerPoll bounds one poll's read of the conversation. A page is
	// Slack's maximum (200); three of them is far more than a personal DM sees
	// between polls, and the next poll continues where this one stopped.
	historyPagesPerPoll = 3

	// pollBackoffMax caps the wait after a failure. An expired cookie can only
	// be fixed by hand, so the surface stops hammering and says so once a poll
	// rather than once a second.
	pollBackoffMax = 5 * time.Minute

	// postedTTL is how long the posted ledger is kept. It only has to outlive
	// the window in which a restart could read a message back, and the cursor
	// has moved past every entry long before a day is out.
	postedTTL  = 24 * time.Hour
	pruneEvery = 6 * time.Hour
)

// State is the durable part of the surface: where the poller got to, and which
// messages the daemon itself posted. The store implements it; tests use a map.
type State interface {
	SurfaceKV(ctx context.Context, surface, key string) (string, bool, error)
	SetSurfaceKV(ctx context.Context, surface, key, value string) error
	MarkSurfacePosted(ctx context.Context, surface, ts string) error
	SurfacePosted(ctx context.Context, surface, ts string) (bool, error)
	PruneSurfacePosted(ctx context.Context, before time.Time) (int64, error)
}

// Core is what the surface needs from the core, declared here so this side
// states the seam from its own side (the same shape as internal/slack/router.go).
type Core interface {
	HandleMessage(ctx context.Context, m bot.Message)
	HandleCommand(ctx context.Context, c bot.Command)
	HandleAction(ctx context.Context, a bot.Action)
}

// Config is what the daemon knows about the surface before it is built.
type Config struct {
	// Channel is the self-DM's D… conversation id.
	Channel string
	// PollEvery is how often the conversation is read.
	PollEvery time.Duration
	// FlushMS is the coalescing window for message updates while an answer
	// streams. The self-DM patches a message instead of streaming into one.
	FlushMS int
}

// Surface is one running self-DM integration: the platform the core renders
// through, the router that turns messages into core calls, and the poller that
// reads them.
type Surface struct {
	api   *slack.API
	state State
	log   *slog.Logger
	cfg   Config
	id    slack.Identity

	plat   *Platform
	router *Router

	// newest is the conversation's newest timestamp at startup, which is where a
	// first run starts: replaying somebody's personal DM as a batch of commands
	// is not what enabling this surface should do.
	newest string
}

// New checks the credentials and the conversation, and builds the surface. It
// is where a misconfiguration is found: a session token Slack will not accept,
// a channel the token cannot read. Both fail here, before the daemon is ready,
// rather than silently answering nothing later.
//
// The core is not a parameter: the core renders through this surface's platform,
// so the platform has to exist first. Run is where the two are joined.
func New(ctx context.Context, api *slack.API, st State, cfg Config, log *slog.Logger) (*Surface, error) {
	if strings.TrimSpace(cfg.Channel) == "" {
		return nil, errors.New("self-DM: no channel id configured")
	}
	if cfg.PollEvery <= 0 {
		cfg.PollEvery = 5 * time.Second
	}
	if cfg.FlushMS <= 0 {
		cfg.FlushMS = 1000
	}

	me, err := api.AuthTest(ctx)
	if err != nil {
		return nil, fmt.Errorf("self-DM: the session token was refused: %w", err)
	}
	if me.UserID == "" || me.TeamID == "" {
		return nil, errors.New("self-DM: auth.test named no user or workspace")
	}

	// One read of the conversation, for three things at once: that the token may
	// read it, that the channel is the one configured, and where "now" is on a
	// first run.
	page, err := api.History(ctx, cfg.Channel, "", "", 1)
	if err != nil {
		return nil, fmt.Errorf("self-DM: cannot read channel %s: %w", cfg.Channel, err)
	}
	s := &Surface{
		api:   api,
		state: st,
		log:   log,
		cfg:   cfg,
		id:    slack.Identity{TeamID: me.TeamID, UserID: me.UserID},
	}
	if len(page.Messages) > 0 {
		s.newest = page.Messages[0].TS
	}
	s.plat = newPlatform(api, st, s.id, cfg, log)
	return s, nil
}

// Platform is the core's half of the surface.
func (s *Surface) Platform() bot.Platform { return s.plat }

// Identity is who this surface acts as: the person whose session it uses. The
// daemon checks it against the allowlist, because a self-DM whose owner is not
// allowed would refuse every message.
func (s *Surface) Identity() slack.Identity { return s.id }

// Run polls until the context ends, routing what it reads into the core.
// Failures are retried with a growing wait and are not fatal to the process:
// the app surface is a different surface, and a cookie that expired while the
// daemon was down must not take it down too.
func (s *Surface) Run(ctx context.Context, core Core) error {
	s.router = &Router{core: core, plat: s.plat, state: s.state, log: s.log, id: s.id, channel: s.cfg.Channel}

	cursor, found, err := s.state.SurfaceKV(ctx, SurfaceName, cursorKey)
	if err != nil {
		return fmt.Errorf("self-DM: read the cursor: %w", err)
	}
	if !found || cursor == "" {
		cursor = s.newest
	}
	s.log.Info("the self-DM surface is watching",
		"channel", s.cfg.Channel, "workspace", s.id.TeamID,
		"poll", s.cfg.PollEvery, "cursor", orNone(cursor))

	wait := s.cfg.PollEvery
	lastPrune := time.Now()
	for {
		if err := s.poll(ctx, &cursor); err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			s.log.Warn("cannot read the self-DM; retrying",
				"error", err, "in", wait, "hint", authHint(err))
			if wait *= 2; wait > pollBackoffMax {
				wait = pollBackoffMax
			}
		} else {
			wait = s.cfg.PollEvery
			if time.Since(lastPrune) >= pruneEvery {
				lastPrune = time.Now()
				if n, err := s.state.PruneSurfacePosted(ctx, time.Now().Add(-postedTTL)); err != nil {
					s.log.Warn("cannot prune the self-DM posted ledger", "error", err)
				} else if n > 0 {
					s.log.Debug("pruned posted self-DM messages", "count", n)
				}
			}
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(wait):
		}
	}
}

// poll reads what is new and hands it to the router, then remembers how far it
// got. The cursor is written once per poll; a crash between a turn and the write
// costs nothing, because the core claims each message by its own identity and a
// redelivery is dropped there.
func (s *Surface) poll(ctx context.Context, cursor *string) error {
	messages, err := s.fetch(ctx, *cursor)
	if err != nil {
		return err
	}
	advanced := false
	for _, m := range messages {
		if compareTS(m.TS, *cursor) <= 0 {
			continue
		}
		s.router.Handle(ctx, m)
		*cursor = m.TS
		advanced = true
	}
	if advanced {
		if err := s.state.SetSurfaceKV(ctx, SurfaceName, cursorKey, *cursor); err != nil {
			return fmt.Errorf("record the cursor: %w", err)
		}
	}
	return nil
}

// fetch reads the conversation since the cursor, and each thread that has
// something newer than the last time it was read, and returns everything in
// order. A message can arrive here twice (a thread's parent is also in the
// conversation); the router's cursor check and the core's claim table both cope.
func (s *Surface) fetch(ctx context.Context, cursor string) ([]slack.Message, error) {
	var got []slack.Message
	latest := ""
	for page := 0; page < historyPagesPerPoll; page++ {
		p, err := s.api.History(ctx, s.cfg.Channel, cursor, latest, 0)
		if err != nil {
			return nil, err
		}
		if len(p.Messages) == 0 {
			break
		}
		got = append(got, p.Messages...)
		if !p.HasMore {
			break
		}
		// Slack pages backwards: the last message of the page is the oldest, and
		// asking for messages before it asks for the next page.
		latest = p.Messages[len(p.Messages)-1].TS
	}

	// Thread replies are a second read, one per thread that has any. The
	// conversation listing does not promise them, and the self-DM is a
	// conversation people do thread in.
	for _, parent := range got {
		if parent.ReplyCount == 0 || parent.LatestReply == "" {
			continue
		}
		if parent.ThreadTS != "" && parent.ThreadTS != parent.TS {
			continue // a reply already, not a thread parent
		}
		key := replyKeyPrefix + parent.TS
		since, _, err := s.state.SurfaceKV(ctx, SurfaceName, key)
		if err != nil {
			return nil, err
		}
		if compareTS(parent.LatestReply, since) <= 0 {
			continue
		}
		replies, err := s.api.Replies(ctx, s.cfg.Channel, parent.TS, since)
		if err != nil {
			return nil, err
		}
		newest := since
		for _, reply := range replies {
			if compareTS(reply.TS, since) <= 0 {
				continue
			}
			// The reply knows which thread it is in, but only the fetch does when the
			// payload omits it — Slack does not set thread_ts on a reply that is the
			// thread's parent, and an adapter should not depend on which shape arrived.
			if reply.ThreadTS == "" {
				reply.ThreadTS = parent.TS
			}
			got = append(got, reply)
			if compareTS(reply.TS, newest) > 0 {
				newest = reply.TS
			}
		}
		if compareTS(newest, since) > 0 {
			if err := s.state.SetSurfaceKV(ctx, SurfaceName, key, newest); err != nil {
				return nil, err
			}
		}
	}

	sort.SliceStable(got, func(i, j int) bool { return compareTS(got[i].TS, got[j].TS) < 0 })
	return got, nil
}

// authHint spells out the one failure whose message does not name its own fix.
func authHint(err error) string {
	if slack.IsCode(err, "invalid_auth") || slack.IsCode(err, "account_inactive") {
		return "the Slack session cookie or xoxc token is stale: sign in to the workspace in a browser, " +
			"copy them again into slack.self_dm.xoxc_file and slack.self_dm.xoxd_file, and restart"
	}
	return ""
}

// compareTS orders two Slack timestamps, which are seconds and a fraction, as
// strings. The fraction is compared as digits rather than as a float: two
// microsecond timestamps in the same second differ by more than a float64 can
// hold next to a 1.7-billion-second epoch.
func compareTS(a, b string) int {
	as, af := splitTS(a)
	bs, bf := splitTS(b)
	switch {
	case as != bs:
		if as < bs {
			return -1
		}
		return 1
	case af == bf:
		return 0
	case af < bf:
		return -1
	default:
		return 1
	}
}

// splitTS splits a Slack timestamp into its seconds and its fraction, the
// fraction right-padded to a fixed width so digit comparison is numeric.
func splitTS(ts string) (int64, string) {
	seconds, fraction, _ := strings.Cut(ts, ".")
	sec, err := strconv.ParseInt(seconds, 10, 64)
	if err != nil {
		return 0, ""
	}
	if len(fraction) > 6 {
		fraction = fraction[:6]
	}
	return sec, fraction + strings.Repeat("0", 6-len(fraction))
}

func orNone(s string) string {
	if s == "" {
		return "none"
	}
	return s
}
