package selfdm

import (
	"context"
	"log/slog"
	"strings"

	"github.com/tigersoldier/pi-chat/internal/bot"
	"github.com/tigersoldier/pi-chat/internal/slack"
)

// Router turns one message read from the self-DM into a core call. It is the
// self-DM's equivalent of internal/slack/router.go: the place where the
// platform's shapes become the core's, and the place that knows what to drop.
//
// The rule that makes the self-DM work at all: the daemon writes as the same
// person it reads, so authorship means nothing here. Everything the daemon
// posted is in its ledger, and the ledger — not `user` — is what separates the
// agent's answer from the human's request.
type Router struct {
	core    Core
	plat    *Platform
	state   State
	log     *slog.Logger
	id      slack.Identity
	channel string
}

// Handle processes one message. It never blocks on a turn: the core starts
// those in the background, and the poller has more messages to hand over.
func (r *Router) Handle(ctx context.Context, m slack.Message) {
	if m.TS == "" || m.Subtype != "" || m.BotID != "" {
		// Joins, edits, deletions and other apps' posts: none of them is a
		// request, and every one of them would otherwise become a prompt.
		return
	}
	if m.User == "" {
		return
	}
	posted, err := r.state.SurfacePosted(ctx, SurfaceName, m.TS)
	if err != nil {
		r.log.Warn("cannot read the self-DM posted ledger; treating the message as input",
			"ts", m.TS, "error", err)
	} else if posted {
		r.log.Debug("ignoring a message this daemon posted", "ts", m.TS)
		return
	}

	// A root message is its own thread; a reply belongs to the thread it replies
	// in. That is exactly what a DM with a bot does, and it is what gives the
	// self-DM the same session model: a new root is a new session, a reply
	// continues the one that thread owns.
	threadTS := m.ThreadTS
	if threadTS == "" || threadTS == m.TS {
		threadTS = m.TS
	}
	text := strings.TrimSpace(m.Text)
	if text == "" && len(m.Files) == 0 {
		return
	}

	// A number answering an open offer is the button press this surface cannot
	// receive. It is checked before anything else: "1" answering a picker in a
	// thread must not become a prompt to the agent.
	lookupThread := threadTS
	if threadTS == m.TS {
		lookupThread = ""
	}
	if action, ok := r.plat.TakeAction(ctx, r.channel, lookupThread, m.User, r.id.TeamID, text); ok {
		r.log.Debug("a numbered reply answers an offer", "channel", r.channel, "thread", threadTS)
		r.core.HandleAction(ctx, action)
		return
	}

	thread := &bot.Thread{Workspace: r.id.TeamID, Channel: r.channel, ThreadTS: threadTS}
	if command, ok := parseSelfCommand(text); ok {
		var scope *bot.Thread
		if threadTS != m.TS {
			scope = thread
		}
		r.core.HandleCommand(ctx, bot.Command{
			// A polled message has no platform event id, so the message's own
			// identity is synthesized for the core's dedupe: the same message read
			// twice — after a crash, say — must not run a command twice, and an
			// empty event id would be claimed by the first command and refuse every
			// command after it.
			EventID:   "selfdm:cmd:" + r.channel + ":" + m.TS,
			Channel:   r.channel,
			Thread:    scope,
			UserID:    m.User,
			Workspace: r.id.TeamID,
			Direct:    true,
			TS:        m.TS,
			Text:      command,
		})
		return
	}

	r.core.HandleMessage(ctx, bot.Message{
		Thread:    *thread,
		UserID:    m.User,
		Workspace: r.id.TeamID,
		TS:        m.TS,
		Text:      text,
		// In a self-DM everyone is the owner, and the conversation is the
		// address: plain text is a turn whether or not it says anybody's name.
		Mentioned: true,
		Direct:    true,
		Files:     len(m.Files),
	})
}

// parseSelfCommand reads the text form of a command in the self-DM.
//
// There are no slash commands here — slash commands are a feature of an
// installed app — so `/pi` is typed as text and this is what recognizes it. The
// prefix is required: a bare `/status` in Slack is somebody else's command or
// pi's own, and guessing on their behalf would be worse than asking for three
// more characters. The returned text is the normalized form the core parses,
// slash included, exactly as `@pi /status` arrives from the app.
func parseSelfCommand(text string) (string, bool) {
	trimmed := strings.TrimSpace(text)
	if !strings.HasPrefix(trimmed, "/pi") {
		return "", false
	}
	rest := strings.TrimPrefix(trimmed, "/pi")
	if rest != "" {
		first := rest[0]
		if first != ' ' && first != '\t' && first != '/' {
			return "", false // "/pizza" is a prompt, not a command
		}
	}
	rest = strings.TrimSpace(rest)
	if rest == "" {
		return "/", true
	}
	if !strings.HasPrefix(rest, "/") {
		rest = "/" + rest
	}
	return rest, true
}
