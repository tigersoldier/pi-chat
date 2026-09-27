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

// Platform renders turns and posts notices with Slack's Web API.
type Platform struct {
	api    *API
	cfg    *config.Config
	log    *slog.Logger
	teamID string // this install's workspace, from auth.test

	// recipientRefused records that this install rejects the recipient fields
	// on chat.startStream, so later turns do not repeat a doomed attempt.
	recipientRefused atomic.Bool
}

// NewPlatform builds the Slack adapter. teamID is the workspace the bot token
// belongs to, which is what a thread key starts with.
func NewPlatform(api *API, cfg *config.Config, teamID string, log *slog.Logger) *Platform {
	return &Platform{api: api, cfg: cfg, log: log, teamID: teamID}
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
	switch {
	case n.ReplyTo != "":
		return p.api.Respond(ctx, n.ReplyTo, n.Text, blocks, n.Update != "")
	case n.Update != "":
		return p.api.UpdateBlocks(ctx, n.Channel, n.Update, n.Text, blocks)
	case n.Ephemeral:
		if n.UserID == "" {
			return errors.New("slack: an ephemeral notice needs a user")
		}
		_, err := p.api.PostEphemeral(ctx, n.Channel, n.UserID, threadTS(n.Thread), n.Text, blocks)
		return err
	default:
		_, err := p.api.PostBlocks(ctx, n.Channel, threadTS(n.Thread), n.Text, blocks)
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
	return bot.Thread{Workspace: p.teamID, Channel: channel, ThreadTS: ts}, nil
}

// threadTS is the thread a notice belongs to, and "" for a channel root.
func threadTS(t *bot.Thread) string {
	if t == nil {
		return ""
	}
	return t.ThreadTS
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
