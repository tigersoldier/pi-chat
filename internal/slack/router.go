package slack

import (
	"context"
	"encoding/json"
	"log/slog"
	"strings"

	"github.com/tigersoldier/pi-chat/internal/bot"
)

// Core is what the adapter needs from the core, declared here rather than
// taken as a *bot.Bot so the adapter states the seam from its own side and can
// be tested with a recorder instead.
type Core interface {
	HandleMessage(ctx context.Context, m bot.Message)
	HandleCommand(ctx context.Context, c bot.Command)
	HandleAction(ctx context.Context, a bot.Action)
	HandleOpened(ctx context.Context, o bot.Opened)
}

// Router turns Socket Mode envelopes into core calls: it parses the envelope
// kinds pi-chat answers and drops the rest.
//
// Parsing lives here, not in the daemon's main, so that main only wires
// components together and the payload handling is testable with recorded
// envelopes (DESIGN.md §12).
type Router struct {
	core      Core
	botUserID string
	log       *slog.Logger
}

// NewRouter builds the router. botUserID is the bot's own member ID from
// auth.test, which is what mentions of it are stripped by.
func NewRouter(core Core, botUserID string, log *slog.Logger) *Router {
	return &Router{core: core, botUserID: botUserID, log: log}
}

// Handle processes one envelope. Socket calls it on its own goroutine per
// envelope, so a command that takes seconds delays nothing else.
func (r *Router) Handle(ctx context.Context, env Envelope) {
	if message, ok := parseMessage(env, r.botUserID); ok {
		r.log.Debug("inbound message",
			"channel", message.Thread.Channel,
			"user", message.UserID,
			"thread", message.Thread.Key(),
			"mentioned", message.Mentioned,
			"direct", message.Direct)
		r.core.HandleMessage(ctx, message)
		return
	}
	if command, ok := parseCommand(env); ok {
		r.log.Debug("inbound command",
			"channel", command.Channel, "user", command.UserID, "text", command.Text)
		r.core.HandleCommand(ctx, command)
		return
	}
	if action, ok := parseAction(env); ok {
		r.log.Debug("inbound action",
			"channel", action.Channel, "user", action.UserID, "action", action.ActionID)
		r.core.HandleAction(ctx, action)
		return
	}
	if stop, ok := parseStopped(env); ok {
		r.log.Debug("the user stopped the turn from Slack",
			"channel", stop.Channel, "user", stop.UserID, "thread", stop.Thread.Key())
		r.core.HandleAction(ctx, stop)
		return
	}
	if opened, ok := parseOpened(env); ok {
		r.log.Debug("the user opened the conversation",
			"channel", opened.Channel, "user", opened.UserID)
		r.core.HandleOpened(ctx, opened)
		return
	}
	r.logEvent(env)
}

// logEvent records what an envelope pi-chat did *not* answer actually carried.
//
// "Did the event arrive, and did we take it" is the first question a delivery
// problem starts with, and the answer was twice missing: a dropped message left no
// trace at all, which is indistinguishable from an event Slack never sent. One
// line per unanswered envelope, at debug level, so the cost is paid only when
// somebody is looking (DESIGN.md §12).
func (r *Router) logEvent(env Envelope) {
	if env.Type != "events_api" {
		r.log.Debug("ignoring an envelope this build does not answer", "type", env.Type)
		return
	}
	var callback struct {
		Event struct {
			Type     string `json:"type"`
			Subtype  string `json:"subtype"`
			BotID    string `json:"bot_id"`
			User     string `json:"user"`
			Text     string `json:"text"`
			TS       string `json:"ts"`
			ThreadTS string `json:"thread_ts"`
			Channel  string `json:"channel"`
		} `json:"event"`
	}
	if err := json.Unmarshal(env.Payload, &callback); err != nil {
		r.log.Debug("ignoring an events envelope this build cannot read", "error", err)
		return
	}
	event := callback.Event
	r.log.Debug("ignoring a slack event",
		"event", event.Type,
		"subtype", event.Subtype,
		"from_a_bot", event.BotID != "",
		"channel", event.Channel,
		"user", event.User,
		"ts", event.TS,
		"thread_ts", event.ThreadTS,
		"mentions_bot", strings.Contains(event.Text, "<@"+r.botUserID+">"))
}
