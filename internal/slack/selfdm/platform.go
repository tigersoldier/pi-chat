package selfdm

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/tigersoldier/pi-chat/internal/bot"
	"github.com/tigersoldier/pi-chat/internal/slack"
)

const (
	// placeholder is what a reply shows before the agent has produced anything.
	placeholder = "_pi is working…_"

	// maxMessage is Slack's practical message limit with room for a truncation
	// notice. It is the self-DM's own constant rather than the app adapter's:
	// the surfaces render differently and neither should inherit the other's
	// bounds by accident.
	maxMessage = 38000

	// choiceTTL bounds how long a numbered reply still means the button it was
	// offered for. A stale picker answering a number typed much later would be
	// worse than making the person ask again.
	choiceTTL = time.Hour
)

// Platform renders turns and posts notices as the person whose session the
// surface uses.
//
// The core's `Notice` vocabulary is text plus buttons. There is no button here
// — nothing owns the message to receive the press — so each button becomes a
// numbered line and a bare number in a reply becomes the press itself. That is
// the whole of this surface's interactivity, and it is why `/pi resume` and the
// delete confirmation work here without an app.
type Platform struct {
	api   *slack.API
	state State
	id    slack.Identity
	log   *slog.Logger

	flush time.Duration

	mu      sync.Mutex
	pending map[string]*choice
}

// choice is one offer of buttons and the message that carried it.
type choice struct {
	thread    *bot.Thread
	buttons   []bot.Button
	messageTS string
	at        time.Time
}

func newPlatform(api *slack.API, st State, id slack.Identity, cfg Config, log *slog.Logger) *Platform {
	return &Platform{
		api:     api,
		state:   st,
		id:      id,
		log:     log,
		flush:   time.Duration(cfg.FlushMS) * time.Millisecond,
		pending: make(map[string]*choice),
	}
}

// StartTurn builds the renderer for a reply to m. It has no side effect; the
// core calls Start when the turn begins.
func (p *Platform) StartTurn(_ context.Context, m bot.Message) (bot.Renderer, error) {
	return &renderer{
		plat:     p,
		log:      p.log.With("channel", m.Thread.Channel, "thread", m.Thread.ThreadTS),
		channel:  m.Thread.Channel,
		threadTS: m.Thread.ThreadTS,
	}, nil
}

// Post posts a notice — a command answer, a refusal, a hint or a picker.
//
// Everything lands in the conversation as a normal message: the response URL an
// interaction would answer through does not exist here, and an ephemeral notice
// has no one to hide from in a conversation with a single reader.
func (p *Platform) Post(ctx context.Context, n bot.Notice) error {
	channel := n.Channel
	if channel == "" && n.Thread != nil {
		channel = n.Thread.Channel
	}
	if channel == "" {
		return errors.New("selfdm: a notice names no channel")
	}
	threadTS := ""
	if n.Thread != nil {
		threadTS = n.Thread.ThreadTS
	}
	if n.ReplyTo != "" {
		// There is nothing to answer through: this surface owns no interaction.
		// Posting is the honest equivalent, and the answer is still visible.
		p.log.Debug("the self-DM has no response URL; posting the notice instead")
	}

	text := n.Text
	if len(n.Buttons) > 0 {
		text = withChoices(text, n.Buttons)
	}
	if strings.TrimSpace(text) == "" {
		return nil
	}

	if n.Update != "" {
		if err := p.update(ctx, channel, n.Update, text); err != nil {
			return err
		}
		if len(n.Buttons) > 0 {
			p.remember(pendingKey(channel, threadTS), &choice{
				thread: threadOf(n.Thread), buttons: n.Buttons, messageTS: n.Update, at: time.Now(),
			})
		} else {
			p.forgetMessage(n.Update)
		}
		return nil
	}

	ts, err := p.post(ctx, channel, threadTS, text)
	if err != nil {
		return err
	}
	if len(n.Buttons) > 0 {
		p.remember(pendingKey(channel, threadTS), &choice{
			thread: threadOf(n.Thread), buttons: n.Buttons, messageTS: ts, at: time.Now(),
		})
	}
	return nil
}

// OpenThread posts a top-level message and returns the thread it starts. The
// core needs this when a root command has no message of its own to answer
// under — adopting a session with `/pi resume`, for instance — and here the
// notice is also the thread's first message, exactly as a person's would be.
func (p *Platform) OpenThread(ctx context.Context, channel, text string) (bot.Thread, error) {
	ts, err := p.post(ctx, channel, "", text)
	if err != nil {
		return bot.Thread{}, err
	}
	return bot.Thread{Workspace: p.id.TeamID, Channel: channel, ThreadTS: ts}, nil
}

// TakeAction turns a plain reply into the button press it means, when the reply
// is a number or a button's own text and an offer is still open in that thread.
// The second result is false when the reply is just a reply.
func (p *Platform) TakeAction(_ context.Context, channel, threadTS, userID, workspace, text string) (bot.Action, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()

	key := pendingKey(channel, threadTS)
	open, ok := p.pending[key]
	if !ok {
		return bot.Action{}, false
	}
	if time.Since(open.at) > choiceTTL {
		delete(p.pending, key)
		return bot.Action{}, false
	}
	index, ok := matchChoice(text, open.buttons)
	if !ok {
		return bot.Action{}, false
	}
	delete(p.pending, key)

	button := open.buttons[index]
	return bot.Action{
		// A synthesized press still needs a stable identity of its own: the core
		// claims actions by event id, and an empty one would be claimed by the
		// first press and refuse every later press for a day.
		EventID:   "selfdm:act:" + channel + ":" + open.messageTS + ":" + strconv.Itoa(index),
		Channel:   channel,
		Thread:    open.thread,
		UserID:    userID,
		Workspace: workspace,
		Direct:    true,
		ActionID:  button.ActionID,
		Value:     button.Value,
		MessageTS: open.messageTS,
	}, true
}

// post sends a new message and records it in the ledger. A ledger write that
// fails is logged rather than returned: the message is already posted, and
// failing the turn would leave it posted anyway with the turn reported broken.
func (p *Platform) post(ctx context.Context, channel, threadTS, text string) (string, error) {
	ts, err := p.api.PostMessage(ctx, channel, threadTS, text)
	if err != nil {
		return "", err
	}
	if err := p.state.MarkSurfacePosted(ctx, SurfaceName, ts); err != nil {
		p.log.Warn("cannot record a posted message; it may be read back as input", "ts", ts, "error", err)
	}
	return ts, nil
}

// update replaces a message the surface already posted.
func (p *Platform) update(ctx context.Context, channel, ts, text string) error {
	return p.api.UpdateMessage(ctx, channel, ts, text)
}

// remember stores one open offer, replacing whatever the same thread had open:
// a second picker in a thread makes the first one's numbers ambiguous.
func (p *Platform) remember(key string, c *choice) {
	p.mu.Lock()
	defer p.mu.Unlock()
	now := time.Now()
	for k, open := range p.pending {
		if now.Sub(open.at) > choiceTTL {
			delete(p.pending, k)
		}
	}
	p.pending[key] = c
}

// forgetMessage drops any offer carried by a message that has been replaced
// without buttons, so a spent picker does not keep answering numbers.
func (p *Platform) forgetMessage(ts string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for key, open := range p.pending {
		if open.messageTS == ts {
			delete(p.pending, key)
		}
	}
}

// pendingKey names the place an offer was made and the reply to it will land:
// a thread, or the conversation's root when the notice was posted there.
func pendingKey(channel, threadTS string) string { return channel + "\x00" + threadTS }

func threadOf(t *bot.Thread) *bot.Thread {
	if t == nil {
		return nil
	}
	clone := *t
	return &clone
}

// withChoices renders the numbered stand-in for buttons.
func withChoices(text string, buttons []bot.Button) string {
	var b strings.Builder
	b.WriteString(text)
	b.WriteString("\n")
	for i, button := range buttons {
		fmt.Fprintf(&b, "\n*%d)* %s", i+1, button.Text)
	}
	b.WriteString("\n\n_Reply with a number._")
	return b.String()
}

// matchChoice reads a reply as one of the offered choices: its number, or the
// button's own text.
func matchChoice(text string, buttons []bot.Button) (int, bool) {
	trimmed := strings.TrimSpace(text)
	trimmed = strings.TrimRight(trimmed, ".)")
	if n, err := strconv.Atoi(strings.TrimSpace(trimmed)); err == nil {
		if n >= 1 && n <= len(buttons) {
			return n - 1, true
		}
		return 0, false
	}
	for i, button := range buttons {
		if strings.EqualFold(strings.TrimSpace(button.Text), strings.TrimSpace(text)) {
			return i, true
		}
	}
	return 0, false
}

// renderer writes one turn into one thread by posting a placeholder and
// replacing it as the answer grows. There is no streaming here: Slack's
// streaming methods belong to an installed app.
type renderer struct {
	plat *Platform
	log  *slog.Logger

	channel  string
	threadTS string

	mu        sync.Mutex
	ts        string
	text      strings.Builder
	lastFlush time.Time
}

func (r *renderer) Start(ctx context.Context) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	ts, err := r.plat.post(ctx, r.channel, r.threadTS, placeholder)
	if err != nil {
		return err
	}
	r.ts = ts
	r.lastFlush = time.Now()
	return nil
}

// Progress names the message this renderer is writing into, so the core can
// record which reply a restart interrupted.
func (r *renderer) Progress() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.ts
}

func (r *renderer) Delta(ctx context.Context, text string) error {
	if text == "" {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.text.WriteString(text)
	if time.Since(r.lastFlush) < r.plat.flush {
		return nil
	}
	return r.flush(ctx)
}

// Finish presents the authoritative answer. An answer longer than one message
// is continued in further messages, because truncating the end of an agent's
// work is worse than a second message.
func (r *renderer) Finish(ctx context.Context, final string) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	if final == "" {
		final = r.text.String()
	}
	r.text.Reset()
	r.text.WriteString(final)

	pieces := splitForSlack(final)
	if err := r.plat.update(ctx, r.channel, r.ts, pieces[0]); err != nil {
		return err
	}
	for _, piece := range pieces[1:] {
		if _, err := r.plat.post(ctx, r.channel, r.threadTS, piece); err != nil {
			return fmt.Errorf("post the rest of a long answer: %w", err)
		}
	}
	r.lastFlush = time.Now()
	return nil
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
	r.text.Reset()
	r.text.WriteString(text)
	return r.flush(ctx)
}

// flush replaces the message with what has been rendered so far.
func (r *renderer) flush(ctx context.Context) error {
	if r.ts == "" {
		return nil
	}
	text := splitForSlack(r.text.String())[0]
	if err := r.plat.update(ctx, r.channel, r.ts, text); err != nil {
		return err
	}
	r.lastFlush = time.Now()
	return nil
}

// splitForSlack cuts text into messages Slack accepts, on a rune boundary. The
// first piece is what a live message shows while the rest is still arriving.
func splitForSlack(text string) []string {
	if text == "" {
		return []string{""}
	}
	var pieces []string
	for len(text) > maxMessage {
		cut := maxMessage
		for cut > 0 && !utf8.RuneStart(text[cut]) {
			cut--
		}
		if cut == 0 {
			cut = maxMessage
		}
		pieces = append(pieces, text[:cut])
		text = text[cut:]
	}
	return append(pieces, text)
}
