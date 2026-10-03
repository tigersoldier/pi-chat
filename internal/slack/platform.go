package slack

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"unicode/utf8"

	"github.com/tigersoldier/pi-chat/internal/bot"
	"github.com/tigersoldier/pi-chat/internal/config"
)

const (
	// placeholder is what a reply shows before the agent produces anything.
	placeholder = "_pi is working…_"
	// streamChunk is the largest piece sent in one chat.appendStream call,
	// well under Slack's per-request markdown_text limit.
	streamChunk = 3000
	// maxMessage is Slack's practical message text limit, with room to spare
	// for the truncation notice.
	maxMessage = 38000
)

// Identity is who this install is on Slack, from auth.test: the workspace, the
// bot user whose mentions are stripped from messages, and the bot ID its own
// posts carry.
type Identity struct {
	TeamID string
	UserID string
	BotID  string
}

// Platform renders turns and posts notices with Slack's Web API.
type Platform struct {
	api    *API
	cfg    *config.Config
	log    *slog.Logger
	id     Identity
	labels *labeler

	// recipientRefused records that this install rejects the recipient fields
	// on chat.startStream, so later turns do not repeat a doomed attempt.
	recipientRefused atomic.Bool

	// statusRefused records that this install cannot show agent statuses at all:
	// a workspace without the agent feature answers `feature_disabled`, and an
	// app whose feature was never enabled answers `missing_scope`. Turns run
	// without an indicator instead of asking again on every one.
	statusRefused atomic.Bool

	// promptsRefused is the same latch for suggestions: a plain-bot install has
	// no agent surface to show them on, and the method needs a scope the default
	// manifest deliberately does not request.
	promptsRefused atomic.Bool
}

// NewPlatform builds the Slack adapter. id is the install's own identity, which
// is what a thread key starts with and what marks the bot's own messages in a
// transcript.
func NewPlatform(api *API, cfg *config.Config, id Identity, log *slog.Logger) *Platform {
	return &Platform{api: api, cfg: cfg, log: log, id: id, labels: newLabeler(api, log)}
}

// Conversation reads a thread back: what was said in it, in order, so the core
// can put the conversation in front of the next prompt (DESIGN.md §4).
//
// The bot's own messages are marked rather than dropped, and joins, edits and
// the like are dropped rather than marked: the core decides what a transcript
// contains, while only this side knows which messages are the bot's own.
func (p *Platform) Conversation(ctx context.Context, t bot.Thread, oldest string) ([]bot.Said, error) {
	replies, err := p.api.Replies(ctx, t.Channel, t.ThreadTS, oldest)
	if err != nil {
		return nil, err
	}
	out := make([]bot.Said, 0, len(replies))
	for _, r := range replies {
		if (r.Subtype != "" && r.Subtype != "file_share") || r.TS == "" {
			continue
		}
		// An edited message arrives as its current text with no subtype, so it is
		// simply the newest version of something already in the transcript.
		said := bot.Said{TS: r.TS, UserID: r.User, Text: r.Text, Files: attachments(r.Files)}
		switch {
		case r.User != "" && r.User == p.id.UserID, r.BotID != "" && r.BotID == p.id.BotID:
			said.FromBot = true
		case r.User == "":
			// Another app: there is no user to resolve, and its bot ID is what
			// there is to name it by.
			said.Name = "bot"
			if r.BotID != "" {
				said.Name = "bot " + r.BotID
			}
		default:
			said.Name = p.labels.name(ctx, r.User)
		}
		out = append(out, said)
	}
	return out, nil
}

// StartTurn builds the renderer for a reply to m. It has no side effect: the
// core calls Start on the renderer when the turn begins.
func (p *Platform) StartTurn(_ context.Context, m bot.Message) (bot.Renderer, error) {
	return &renderer{
		platform:      p,
		api:           p.api,
		log:           p.log.With("channel", m.Thread.Channel, "thread", m.Thread.ThreadTS),
		channel:       m.Thread.Channel,
		threadTS:      m.Thread.ThreadTS,
		recipientUser: m.UserID,
		recipientTeam: m.Workspace,
		wantStream:    p.cfg.Render.Mode == "stream",
	}, nil
}

// Post posts a notice — a command answer, a refusal, a hint or a picker.
//
// Where it goes follows the notice: a response URL answers the interaction
// that asked (and is the only way to replace an ephemeral message), an Update
// replaces a message the bot already posted, an ephemeral notice is shown to
// one user, and everything else is a normal message in the channel or thread.
func (p *Platform) Post(ctx context.Context, n bot.Notice) error {
	blocks := blocksFor(n.Buttons)
	// A notice in a thread names its channel twice at most: the Thread carries it,
	// and Channel repeats it for the channel-root case. Take whichever is set — a
	// caller that named only the thread must not post into the void, which is how
	// the "attached to this session" notice after /pi resume was answered with
	// channel_not_found and the session looked unusable.
	channel := n.Channel
	if channel == "" && n.Thread != nil {
		channel = n.Thread.Channel
	}
	switch {
	case n.ReplyTo != "":
		err := p.api.Respond(ctx, n.ReplyTo, n.Text, blocks, n.Update != "")
		if err == nil {
			return nil
		}
		// An answer the platform refuses must not become an answer nobody ever sees
		// — whether it was the first post or the replacement of a confirmation. A
		// replace that fails leaves the original in place, which is stale but
		// readable; silence is not.
		if n.Update != "" {
			p.log.Warn("cannot replace the message; posting the answer instead", "error", err)
		} else {
			p.log.Warn("cannot answer through the response URL; posting instead", "error", err)
		}
		if n.Ephemeral && n.UserID != "" {
			_, perr := p.api.PostEphemeral(ctx, channel, n.UserID, threadTS(n.Thread), n.Text, blocks)
			return perr
		}
		_, perr := p.api.PostBlocks(ctx, channel, threadTS(n.Thread), n.Text, blocks)
		return perr
	case n.Update != "":
		return p.api.UpdateBlocks(ctx, channel, n.Update, n.Text, blocks)
	case n.Ephemeral:
		if n.UserID == "" {
			return errors.New("slack: an ephemeral notice needs a user")
		}
		_, err := p.api.PostEphemeral(ctx, channel, n.UserID, threadTS(n.Thread), n.Text, blocks)
		return err
	default:
		_, err := p.api.PostBlocks(ctx, channel, threadTS(n.Thread), n.Text, blocks)
		return err
	}
}

// OpenThread posts a top-level message and returns the thread it starts: the
// new message's own timestamp is the thread root. The core needs this when a
// root command has no message of its own to answer under.
func (p *Platform) OpenThread(ctx context.Context, channel, text string) (bot.Thread, error) {
	ts, err := p.api.PostMessage(ctx, channel, "", text)
	if err != nil {
		return bot.Thread{}, err
	}
	return bot.Thread{Workspace: p.id.TeamID, Channel: channel, ThreadTS: ts}, nil
}

// threadTS is the thread a notice belongs to, and "" for a channel root.
func threadTS(t *bot.Thread) string {
	if t == nil {
		return ""
	}
	return t.ThreadTS
}

// agentStatus maps the core's platform-neutral states onto Slack's agent
// session statuses. Slack renders `processing` as a loading indicator and a
// stop button (DESIGN.md §6).
var agentStatus = map[bot.Status]string{
	bot.StatusBusy:    "processing",
	bot.StatusWaiting: "suspended",
	bot.StatusIdle:    "active",
	bot.StatusClosed:  "closed",
}

// SetStatus displays a thread session's lifecycle state in Slack's agent
// surface. It is best-effort by contract (bot.StatusReporter): the core logs a
// failure and carries on.
func (p *Platform) SetStatus(ctx context.Context, t bot.Thread, s bot.Status) error {
	status, ok := agentStatus[s]
	if !ok {
		return fmt.Errorf("slack: no agent status for %q", s)
	}
	if p.statusRefused.Load() {
		return nil
	}
	err := p.api.SetAgentStatus(ctx, t.Channel, t.ThreadTS, status)
	if err == nil {
		return nil
	}
	if !capabilityUnavailable(err) {
		return err
	}
	// Ask once, then stop: the answer will not change while this process lives,
	// and a per-turn warning would be noise about a cosmetic loss.
	p.statusRefused.Store(true)
	p.log.Info("this install has no agent status; turns will run without an indicator",
		"error", err)
	return nil
}

// SetSuggestedPrompts offers prompts in Slack's agent surface, best-effort by
// contract (bot.PromptReporter).
func (p *Platform) SetSuggestedPrompts(ctx context.Context, channel, title string, suggestions []bot.Suggestion) error {
	if len(suggestions) == 0 || p.promptsRefused.Load() {
		return nil
	}
	prompts := make([]suggestion, 0, len(suggestions))
	for _, s := range suggestions {
		prompts = append(prompts, suggestion{Title: s.Title, Message: s.Message})
	}
	err := p.api.SetSuggestedPrompts(ctx, channel, title, prompts)
	if err == nil {
		return nil
	}
	if !capabilityUnavailable(err) {
		return err
	}
	p.promptsRefused.Store(true)
	p.log.Info("this install cannot show agent suggestions; the conversation stays as it is",
		"error", err)
	return nil
}

// capabilityUnavailable reports whether err means "this install can never do
// this", as opposed to a failure that may pass: a workspace without the agent
// feature, an app that was never granted the scope, or an API surface that has
// moved on. The answer will not change while this process lives, so it is worth
// asking exactly once and then leaving alone.
func capabilityUnavailable(err error) bool {
	for _, code := range []string{
		"feature_disabled",       // no agent feature in this workspace
		"missing_scope",          // declared agent_view but never granted the scope
		"not_agent_app",          // the app is not an agent app at all
		"unknown_method",         // an older API surface
		"method_deprecated",      //
		"not_allowed_token_type", // a token Slack will not accept here at all
	} {
		if IsCode(err, code) {
			return true
		}
	}
	return false
}

// renderer writes one turn into one thread. Streaming is preferred and patching
// a message is the fallback, so a workspace or thread that refuses
// chat.startStream still gets an answer (DESIGN.md §6).
type renderer struct {
	platform *Platform
	api      *API
	log      *slog.Logger

	channel       string
	threadTS      string
	recipientUser string
	recipientTeam string
	wantStream    bool

	mu        sync.Mutex
	ts        string
	streaming bool
	text      strings.Builder
}

func (r *renderer) Start(ctx context.Context) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	if r.wantStream {
		// Slack documents the recipient as required "when streaming to
		// channels", and pi-chat always streams into a thread. Send it when we
		// have it, and retry without it rather than giving up on streaming over
		// a field Slack may not want here. The refusal is remembered so an
		// install that rejects it pays for the discovery only once.
		withRecipient := r.recipientUser != "" && !r.platform.recipientRefused.Load()
		recipientUser, recipientTeam := r.recipientFields(withRecipient)
		ts, err := r.api.StartStream(ctx, r.channel, r.threadTS, recipientUser, recipientTeam)
		if err != nil && withRecipient {
			r.log.Warn("streaming with a recipient was refused; retrying without one", "error", err)
			ts, err = r.api.StartStream(ctx, r.channel, r.threadTS, "", "")
			if err == nil {
				r.platform.recipientRefused.Store(true)
			}
		}
		if err == nil {
			r.ts, r.streaming = ts, true
			return nil
		}
		r.log.Warn("cannot stream into this thread; patching one message instead", "error", err)
	}
	ts, err := r.api.PostMessage(ctx, r.channel, r.threadTS, placeholder)
	if err != nil {
		return err
	}
	r.ts = ts
	return nil
}

// Progress names the message this renderer is writing into, so the core can
// record which reply a restart interrupted (DESIGN.md §7).
func (r *renderer) Progress() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.ts
}

// recipientFields returns the chat.startStream recipient arguments.
func (r *renderer) recipientFields(withRecipient bool) (string, string) {
	if !withRecipient {
		return "", ""
	}
	return r.recipientUser, r.recipientTeam
}

func (r *renderer) Delta(ctx context.Context, text string) error {
	if text == "" {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()

	r.text.WriteString(text)
	if r.streaming {
		err := r.appendStream(ctx, text)
		if err == nil {
			return nil
		}
		r.log.Warn("streaming failed; switching to message updates", "error", err)
		r.streaming = false
	}
	return r.patch(ctx)
}

// Finish presents the authoritative answer. The streamed text usually is the
// answer already, in which case finishing the stream is all that is left; when
// they differ (a dropped append, or a turn that produced several messages) the
// message is corrected rather than left wrong.
func (r *renderer) Finish(ctx context.Context, final string) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	streamed := r.text.String()
	if final == "" {
		final = streamed
	}
	if r.streaming {
		switch err := r.api.StopStream(ctx, r.channel, r.ts); {
		case err != nil:
			r.log.Warn("cannot finish the stream; patching the message instead", "error", err)
			r.streaming = false
		case final == streamed:
			return nil
		default:
			if err := r.api.UpdateMessage(ctx, r.channel, r.ts, truncate(final, maxMessage)); err != nil {
				return fmt.Errorf("correct the streamed answer: %w", err)
			}
			r.text.Reset()
			r.text.WriteString(final)
			return nil
		}
	}
	r.text.Reset()
	r.text.WriteString(final)
	return r.patch(ctx)
}

// Fail reports a failed turn in the same message, keeping whatever partial
// answer arrived first.
func (r *renderer) Fail(ctx context.Context, cause error) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	text := r.text.String()
	if text != "" {
		text += "\n\n"
	}
	text += ":warning: pi-chat: " + cause.Error()

	if r.streaming {
		if err := r.api.StopStream(ctx, r.channel, r.ts); err != nil {
			r.log.Warn("cannot finish the stream after a failure", "error", err)
		}
		r.streaming = false
	}
	r.text.Reset()
	r.text.WriteString(text)
	return r.patch(ctx)
}

// appendStream sends text in pieces Slack will accept, so one long delta
// cannot fail as a whole.
func (r *renderer) appendStream(ctx context.Context, text string) error {
	for _, piece := range chunk(text, streamChunk) {
		if err := r.api.AppendStream(ctx, r.channel, r.ts, piece); err != nil {
			return err
		}
	}
	return nil
}

// patch replaces the whole message with what has been rendered so far.
func (r *renderer) patch(ctx context.Context) error {
	return r.api.UpdateMessage(ctx, r.channel, r.ts, truncate(r.text.String(), maxMessage))
}

// chunk splits s into pieces of at most n bytes without cutting a rune in half.
func chunk(s string, n int) []string {
	var pieces []string
	for len(s) > n {
		cut := cutAtRune(s, n)
		pieces = append(pieces, s[:cut])
		s = s[cut:]
	}
	if s != "" {
		pieces = append(pieces, s)
	}
	return pieces
}

// truncate bounds text to what Slack accepts, marking that it was cut.
func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:cutAtRune(s, max)] + "\n\n… _(truncated)_"
}

// cutAtRune returns the largest cut at or below n that does not split a rune.
// When there is no boundary to find — the first n bytes are one long character —
// it cuts at n anyway: something has to be sent, and an empty chunk is worse.
func cutAtRune(s string, n int) int {
	if n >= len(s) {
		return len(s)
	}
	cut := n
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	if cut == 0 {
		return n
	}
	return cut
}
