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
	requestReactionHistoryPageSize = 200

	// placeholder is the patch-mode reply state until its first text update.
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

func (p *Platform) ChatSurface() string { return bot.SurfaceSelfDM }

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
// core calls Start only after the first user-visible text arrives.
func (p *Platform) StartTurn(_ context.Context, m bot.Message) (bot.Renderer, error) {
	return &renderer{
		plat:     p,
		log:      p.log.With("channel", m.Thread.Channel, "thread", m.Thread.ThreadTS),
		channel:  m.Thread.Channel,
		threadTS: m.Thread.ThreadTS,
	}, nil
}

// AcknowledgeRequest reacts to the user's request through the self-DM token.
func (p *Platform) AcknowledgeRequest(ctx context.Context, m bot.Message, allowed bool) error {
	reaction := "shrug"
	if allowed {
		reaction = "eyes"
	}
	return p.api.AddReaction(ctx, m.Thread.Channel, m.TS, reaction)
}

// QueueRequest marks an accepted request while it waits behind an active turn.
func (p *Platform) QueueRequest(ctx context.Context, m bot.Message) error {
	return p.transitionRequestReaction(ctx, m, "eyes", "hourglass_flowing_sand")
}

// StartRequest marks an immediate request with eyes or promotes a queued
// request from the queue marker to eyes, then removes this install's old request
// reactions. The posted-message ledger is authoritative on this surface: daemon
// messages are authored by the same Slack user, so cleanup only considers
// messages younger than the ledger TTL minus its pruning safety grace.
func (p *Platform) StartRequest(ctx context.Context, m bot.Message, queued bool) error {
	var result error
	if queued {
		result = p.transitionRequestReaction(ctx, m, "hourglass_flowing_sand", "eyes")
	} else if err := p.api.AddReaction(ctx, m.Thread.Channel, m.TS, "eyes"); err != nil && !slack.IsCode(err, "already_reacted") {
		result = fmt.Errorf("add the eyes request reaction: %w", err)
	}
	if p.id.UserID == "" {
		return result
	}

	found, err := p.api.WalkRepliesUntil(ctx, m.Thread.Channel, m.Thread.ThreadTS, m.TS, func(prior slack.Message) error {
		if compareTS(prior.TS, m.TS) >= 0 {
			return nil
		}
		if err := p.removePriorRequestReactions(ctx, m.Thread.Channel, prior); err != nil && result == nil {
			result = err
		}
		return nil // a reaction or ledger failure must not stop a best-effort scan
	})
	if err != nil && result == nil {
		result = fmt.Errorf("read the self-DM thread's request reactions: %w", err)
	} else if err == nil && !found && result == nil {
		result = fmt.Errorf("request %s was not in the self-DM thread reply scan", m.TS)
	}

	if m.Direct && m.TS == m.Thread.ThreadTS {
		cursor := ""
		for {
			history, err := p.api.HistoryCursorPage(ctx, m.Thread.Channel, m.TS, cursor, requestReactionHistoryPageSize)
			if err != nil {
				if result == nil {
					result = fmt.Errorf("read earlier self-DM request reactions: %w", err)
				}
				break
			}
			for _, prior := range history.Messages {
				if compareTS(prior.TS, m.TS) >= 0 {
					continue
				}
				if err := p.removePriorRequestReactions(ctx, m.Thread.Channel, prior); err != nil && result == nil {
					result = err
				}
			}
			if !history.HasMore || history.NextCursor == "" {
				break
			}
			cursor = history.NextCursor
		}
	}
	return result
}

// CompleteRequest replaces the in-progress marker with a check mark.
func (p *Platform) CompleteRequest(ctx context.Context, m bot.Message) error {
	return p.replaceRequestReaction(ctx, m, "white_check_mark")
}

// FailRequest replaces the in-progress marker with a warning.
func (p *Platform) FailRequest(ctx context.Context, m bot.Message) error {
	return p.replaceRequestReaction(ctx, m, "warning")
}

func (p *Platform) removePriorRequestReactions(ctx context.Context, channel string, prior slack.Message) error {
	if prior.TS == "" || prior.User != p.id.UserID || prior.User == "" || prior.BotID != "" || prior.Subtype != "" {
		return nil
	}
	hasRequestReaction := false
	for _, reaction := range prior.Reactions {
		if selfDMRequestReaction(reaction.Name) && containsSelfDMUser(reaction.Users, p.id.UserID) {
			hasRequestReaction = true
			break
		}
	}
	if !hasRequestReaction {
		return nil
	}
	postedAt, ok := selfDMTimestamp(prior.TS)
	if !ok || postedAt.Before(time.Now().Add(-(postedTTL - postedLedgerSafetyGrace))) {
		p.log.Debug("preserving reactions on a self-DM message outside the posted-ledger safety window", "ts", prior.TS)
		return nil
	}
	posted, err := p.state.SurfacePosted(ctx, SurfaceName, prior.TS)
	if err != nil {
		p.log.Warn("cannot check whether a self-DM message was posted by this daemon; preserving its reactions",
			"ts", prior.TS, "error", err)
		return fmt.Errorf("check the self-DM posted-message ledger for %s: %w", prior.TS, err)
	}
	if posted {
		return nil
	}
	var result error
	for _, reaction := range prior.Reactions {
		if !selfDMRequestReaction(reaction.Name) || !containsSelfDMUser(reaction.Users, p.id.UserID) {
			continue
		}
		if err := p.api.RemoveReaction(ctx, channel, prior.TS, reaction.Name); err != nil && !slack.IsCode(err, "no_reaction") && result == nil {
			result = fmt.Errorf("remove stale self-DM %s reaction from %s: %w", reaction.Name, prior.TS, err)
		}
	}
	return result
}

func (p *Platform) replaceRequestReaction(ctx context.Context, m bot.Message, next string) error {
	for _, reaction := range []string{"eyes", "hourglass_flowing_sand"} {
		if err := p.api.RemoveReaction(ctx, m.Thread.Channel, m.TS, reaction); err != nil && !slack.IsCode(err, "no_reaction") {
			return fmt.Errorf("remove self-DM in-progress %s reaction: %w", reaction, err)
		}
	}
	return p.api.AddReaction(ctx, m.Thread.Channel, m.TS, next)
}

func (p *Platform) transitionRequestReaction(ctx context.Context, m bot.Message, old, next string) error {
	var result error
	if err := p.api.RemoveReaction(ctx, m.Thread.Channel, m.TS, old); err != nil && !slack.IsCode(err, "no_reaction") {
		result = fmt.Errorf("remove self-DM %s request reaction: %w", old, err)
	}
	if err := p.api.AddReaction(ctx, m.Thread.Channel, m.TS, next); err != nil && !slack.IsCode(err, "already_reacted") && result == nil {
		result = fmt.Errorf("add self-DM %s request reaction: %w", next, err)
	}
	return result
}

func selfDMTimestamp(ts string) (time.Time, bool) {
	seconds, fraction, ok := strings.Cut(ts, ".")
	if !ok || seconds == "" || fraction == "" || len(fraction) > 9 {
		return time.Time{}, false
	}
	unixSeconds, err := strconv.ParseInt(seconds, 10, 64)
	if err != nil || unixSeconds < 0 {
		return time.Time{}, false
	}
	nanoseconds, err := strconv.ParseInt(fraction+strings.Repeat("0", 9-len(fraction)), 10, 64)
	if err != nil {
		return time.Time{}, false
	}
	return time.Unix(unixSeconds, nanoseconds), true
}

func selfDMRequestReaction(name string) bool {
	switch name {
	case "eyes", "hourglass_flowing_sand", "white_check_mark", "warning", "shrug":
		return true
	default:
		return false
	}
}

func containsSelfDMUser(users []string, userID string) bool {
	for _, user := range users {
		if user == userID {
			return true
		}
	}
	return false
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
	ts, err := p.postPlainText(ctx, channel, text)
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

func (p *Platform) postPlainText(ctx context.Context, channel, text string) (string, error) {
	ts, err := p.api.PostPlainTextMessage(ctx, channel, "", text)
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
	if r.ts == "" {
		// Start may fail before a message handle exists. Retain the partial
		// answer for Finish or Fail instead of updating an empty ts.
		return nil
	}
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
	if r.ts == "" {
		ts, err := r.plat.post(ctx, r.channel, r.threadTS, pieces[0])
		if err != nil {
			return fmt.Errorf("post the final answer: %w", err)
		}
		r.ts = ts
	} else if err := r.plat.update(ctx, r.channel, r.ts, pieces[0]); err != nil {
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

	text := failureText(r.text.String(), cause)
	r.text.Reset()
	r.text.WriteString(text)
	if r.ts != "" {
		if err := r.flush(ctx); err == nil || !slack.IsCode(err, "message_not_found") {
			return err
		}
	}

	pieces := splitForSlack(r.text.String())
	ts, err := r.plat.post(ctx, r.channel, r.threadTS, pieces[0])
	if err != nil {
		return fmt.Errorf("post failure message: %w", err)
	}
	r.ts = ts
	r.lastFlush = time.Now()
	return nil
}

// failureText reserves room for the cause before trimming a partial answer to
// the one message a failed turn can update or post.
func failureText(partial string, cause error) string {
	failure := ":warning: pi-chat: " + cause.Error()
	const marker = "\n\n… _(truncated)_"
	if len(failure) > maxMessage {
		cut := maxMessage - len(marker)
		for cut > 0 && !utf8.RuneStart(failure[cut]) {
			cut--
		}
		return failure[:cut] + marker
	}

	const separator = "\n\n"
	available := maxMessage - len(separator) - len(failure)
	if available <= 0 {
		return failure
	}
	if len(partial) > available {
		cut := available
		notice := marker
		if cut >= len(notice) {
			cut -= len(notice)
		} else {
			notice = ""
		}
		for cut > 0 && !utf8.RuneStart(partial[cut]) {
			cut--
		}
		partial = partial[:cut] + notice
	}
	if partial == "" {
		return failure
	}
	return partial + separator + failure
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
