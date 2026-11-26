package slack

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"github.com/tigersoldier/pi-chat/internal/bot"
	"github.com/tigersoldier/pi-chat/internal/config"
)

const (
	// placeholder is the patch-mode reply state until its first text update.
	placeholder = "_pi is working…_"
	// streamChunk is the largest piece sent in one chat.appendStream call,
	// well under Slack's per-request markdown_text limit.
	streamChunk = 3000
	// maxMessage is a conservative byte budget within Slack's 12,000-character
	// markdown_text limit, leaving room for the truncation marker.
	maxMessage = 12_000
	// Slack recommends pacing chat.update to at most one update per message per
	// three seconds. Progress state is coalesced between those writes.
	progressReportUpdateInterval = 3 * time.Second

	// streamRotateAfter is how long one Slack stream stays open. Slack
	// documents a 10-minute lifetime measured from chat.startStream, and appends
	// do not extend it; when it expires Slack finalizes the message, moves the
	// agent session to active, and rejects later appends. Rotate with a minute
	// of margin so latency cannot let Slack close the stream under the turn.
	// https://docs.slack.dev/reference/methods/chat.startStream#timeout
	streamRotateAfter = 9 * time.Minute

	// rotationContinuationTitle labels the task cards in a segment that is being
	// closed. Slack marks still-open tasks as errors when their stream stops;
	// completing them with this title keeps that error out of the thread while
	// the live cards continue in the replacement stream.
	rotationContinuationTitle = "Continued in a new message"

	// promotionContinuationTitle does the same for a card whose stream is being
	// replaced by a plan report in the same message. The report update follows
	// immediately; this title is what remains if that update fails.
	promotionContinuationTitle = "Progress moved to a plan"

	replyTooLongNotice = "Slack rejected this reply as too long. Please ask for a shorter version."
)

// Identity is who this install is on Slack, from auth.test: the workspace, the
// bot user whose mentions are stripped from messages, and the bot ID its own
// posts carry.
type Identity struct {
	TeamID string
	UserID string
	BotID  string
	URL    string // workspace URL returned by auth.test
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

	// promptsRefused is the same latch for suggestions: an older or custom app
	// that is not declared as an agent may have no surface to show them on, or
	// may not have granted the method's assistant:write scope.
	promptsRefused atomic.Bool
}

// NewPlatform builds the Slack adapter. id is the install's own identity, which
// is what a thread key starts with and what marks the bot's own messages in a
// transcript.
func NewPlatform(api *API, cfg *config.Config, id Identity, log *slog.Logger) *Platform {
	return &Platform{api: api, cfg: cfg, log: log, id: id, labels: newLabeler(api, log)}
}

// ChatSurface identifies the installed-app credential surface.
func (p *Platform) ChatSurface() string { return bot.SurfaceApp }

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
		said := bot.Said{
			TS:     r.TS,
			UserID: r.User,
			Text:   r.Text,
			Files:  attachments(r.Files),
			// Only a human message can be a prompt: dispatch rejects every bot post,
			// so another app's mention of the bot is not a handled command.
			Mentioned: r.BotID == "" && stripMention(r.Text, p.id.UserID) != r.Text,
		}
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
// core calls Start only after the first user-visible text arrives.
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
	replaceOriginal := n.ReplaceOriginal || n.Update != ""
	switch {
	case n.ReplyTo != "":
		err := p.api.Respond(ctx, n.ReplyTo, n.Text, blocks, replaceOriginal)
		if err == nil {
			return nil
		}
		// An answer the platform refuses must not become an answer nobody ever sees
		// — whether it was the first post or the replacement of a confirmation. A
		// replace that fails leaves the original in place, which is stale but
		// readable; silence is not.
		if replaceOriginal {
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

// AcknowledgeRequest reacts to the user's original message before work starts.
// Slack's reactions.add expects the emoji name without colons.
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
// reactions from prior user messages. Thread replies are paged through the
// current request, and direct-message root history is paged through all earlier
// roots. Both scans process one Slack page at a time; the caller's bounded
// context is the only scan limit.
func (p *Platform) StartRequest(ctx context.Context, m bot.Message, queued bool) error {
	var result error
	if queued {
		result = p.transitionRequestReaction(ctx, m, "hourglass_flowing_sand", "eyes")
	} else if err := p.api.AddReaction(ctx, m.Thread.Channel, m.TS, "eyes"); err != nil && !IsCode(err, "already_reacted") {
		result = fmt.Errorf("add the eyes request reaction: %w", err)
	}
	if p.id.UserID == "" {
		return result
	}

	found, err := p.api.WalkRepliesUntil(ctx, m.Thread.Channel, m.Thread.ThreadTS, m.TS, func(prior Message) error {
		if !timestampBefore(prior.TS, m.TS) {
			return nil
		}
		if err := p.removePriorRequestReaction(ctx, m.Thread.Channel, prior); err != nil && result == nil {
			result = err
		}
		return nil // reaction failures must not stop the scan
	})
	if err != nil && result == nil {
		result = fmt.Errorf("read the thread's request reactions: %w", err)
	} else if err == nil && !found && result == nil {
		result = fmt.Errorf("request %s was not in the thread reply scan", m.TS)
	}

	if m.Direct && m.TS == m.Thread.ThreadTS {
		cursor := ""
		for {
			history, err := p.api.HistoryCursorPage(ctx, m.Thread.Channel, m.TS, cursor, historyPageSize)
			if err != nil {
				if result == nil {
					result = fmt.Errorf("read earlier direct-message request reactions: %w", err)
				}
				break
			}
			if err := p.removePriorRequestReactions(ctx, m.Thread.Channel, history.Messages, m.TS); err != nil && result == nil {
				result = err
			}
			if !history.HasMore || history.NextCursor == "" {
				break
			}
			cursor = history.NextCursor
		}
	}
	return result
}

func (p *Platform) removePriorRequestReactions(ctx context.Context, channel string, messages []Message, before string) error {
	var result error
	for _, prior := range messages {
		if !timestampBefore(prior.TS, before) {
			continue
		}
		if err := p.removePriorRequestReaction(ctx, channel, prior); err != nil && result == nil {
			result = err
		}
	}
	return result
}

func (p *Platform) removePriorRequestReaction(ctx context.Context, channel string, prior Message) error {
	if !p.isUserMessage(prior) {
		return nil
	}
	var result error
	for _, reaction := range prior.Reactions {
		if !isRequestReaction(reaction.Name) || !containsUser(reaction.Users, p.id.UserID) {
			continue
		}
		if err := p.api.RemoveReaction(ctx, channel, prior.TS, reaction.Name); err != nil && !IsCode(err, "no_reaction") && result == nil {
			result = fmt.Errorf("remove stale %s reaction from %s: %w", reaction.Name, prior.TS, err)
		}
	}
	return result
}

// CompleteRequest replaces an in-progress reaction with a check mark.
func (p *Platform) CompleteRequest(ctx context.Context, m bot.Message) error {
	return p.replaceRequestReaction(ctx, m, "white_check_mark")
}

// FailRequest replaces an in-progress reaction with a warning reaction.
func (p *Platform) FailRequest(ctx context.Context, m bot.Message) error {
	return p.replaceRequestReaction(ctx, m, "warning")
}

// replaceRequestReaction moves an accepted request out of either in-progress state.
func (p *Platform) replaceRequestReaction(ctx context.Context, m bot.Message, next string) error {
	for _, reaction := range []string{"eyes", "hourglass_flowing_sand"} {
		if err := p.api.RemoveReaction(ctx, m.Thread.Channel, m.TS, reaction); err != nil && !IsCode(err, "no_reaction") {
			return fmt.Errorf("remove the in-progress %s reaction: %w", reaction, err)
		}
	}
	return p.api.AddReaction(ctx, m.Thread.Channel, m.TS, next)
}

// transitionRequestReaction moves between request states. It attempts the new
// marker even if removing the old one failed, since reactions are best-effort.
func (p *Platform) transitionRequestReaction(ctx context.Context, m bot.Message, old, next string) error {
	var result error
	if err := p.api.RemoveReaction(ctx, m.Thread.Channel, m.TS, old); err != nil && !IsCode(err, "no_reaction") {
		result = fmt.Errorf("remove the %s request reaction: %w", old, err)
	}
	if err := p.api.AddReaction(ctx, m.Thread.Channel, m.TS, next); err != nil && !IsCode(err, "already_reacted") && result == nil {
		result = fmt.Errorf("add the %s request reaction: %w", next, err)
	}
	return result
}

func (p *Platform) isUserMessage(m Message) bool {
	return m.User != "" && m.User != p.id.UserID && m.BotID == ""
}

func containsUser(users []string, userID string) bool {
	for _, user := range users {
		if user == userID {
			return true
		}
	}
	return false
}

func isRequestReaction(name string) bool {
	switch name {
	case "eyes", "hourglass_flowing_sand", "white_check_mark", "warning", "shrug":
		return true
	default:
		return false
	}
}

func timestampBefore(a, b string) bool {
	aSeconds, aFraction, aOK := strings.Cut(a, ".")
	bSeconds, bFraction, bOK := strings.Cut(b, ".")
	if !aOK || !bOK {
		return a < b
	}
	if len(aSeconds) != len(bSeconds) {
		return len(aSeconds) < len(bSeconds)
	}
	if aSeconds != bSeconds {
		return aSeconds < bSeconds
	}
	width := max(len(aFraction), len(bFraction))
	aFraction += strings.Repeat("0", width-len(aFraction))
	bFraction += strings.Repeat("0", width-len(bFraction))
	return aFraction < bFraction
}

// OpenThread posts a top-level message and returns the thread it starts: the
// new message's own timestamp is the thread root. The core needs this when a
// root command has no message of its own to answer under.
func (p *Platform) OpenThread(ctx context.Context, channel, text string) (bot.Thread, error) {
	ts, err := p.api.PostPlainTextMessage(ctx, channel, "", text)
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

// StatusRefreshInterval renews Slack's one-hour agent-session status lease with
// enough margin for a delayed or rate-limited request.
func (p *Platform) StatusRefreshInterval() time.Duration { return 45 * time.Minute }

// RecoverInterruptedProgress closes a stream left by a previous process and
// replaces its task UI with an explicit interruption notice. The message handle
// is durable; task content remains solely in Slack.
//
// Only a stream that is still open is an interrupted reply. Slack answers
// message_not_in_streaming_state for a message it already finalized, and a
// stored handle can outlive its turn (an earlier build did not clear it, and a
// crash between delivery and the clear has the same shape). Replacing that
// message would erase a delivered answer, so a closed stream is left alone.
func (p *Platform) RecoverInterruptedProgress(ctx context.Context, t bot.Thread, ts string) error {
	err := p.api.StopStream(ctx, t.Channel, ts, "active")
	switch {
	case err == nil:
		// The stream was still open, so this reply really was interrupted.
	case streamAlreadyStopped(err):
		if p.log != nil {
			p.log.Debug("leaving a message whose stream is already closed alone",
				"channel", t.Channel, "message_ts", ts, "error", err)
		}
		return nil
	default:
		return fmt.Errorf("stop the interrupted Slack stream: %w", err)
	}
	if err := p.api.UpdateBlocks(ctx, t.Channel, ts,
		"pi-chat restarted while this reply was in progress.", nil); err != nil {
		return fmt.Errorf("mark the Slack reply interrupted: %w", err)
	}
	if err := p.SetStatus(ctx, t, bot.StatusIdle); err != nil && p.log != nil {
		p.log.Debug("cannot clear recovered Slack session status", "error", err)
	}
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

// streamState records whether Slack may still have the timestamped stream open.
type streamState uint8

const (
	streamNone streamState = iota
	streamActive
	streamUnknown
	streamClosed
)

func streamStateName(state streamState) string {
	switch state {
	case streamNone:
		return "none"
	case streamActive:
		return "active"
	case streamUnknown:
		return "unknown"
	case streamClosed:
		return "closed"
	default:
		return "invalid"
	}
}

// setStreamState is called while r.mu is held and logs only actual transitions.
func (r *renderer) setStreamState(next streamState, reason string) {
	previous := r.streamState
	if previous == next {
		return
	}
	r.streamState = next
	if r.log != nil {
		r.log.Debug("Slack reply stream state changed",
			"message_ts", r.ts,
			"from", streamStateName(previous),
			"to", streamStateName(next),
			"reason", reason)
	}
}

// renderer writes one turn into one thread. Streaming is preferred for short
// work, with a Block Kit task/plan report for multi-step or stream-closed work;
// patching remains the fallback for ordinary answers (DESIGN.md §6).
type renderer struct {
	platform *Platform
	api      *API
	log      *slog.Logger

	channel       string
	threadTS      string
	recipientUser string
	recipientTeam string
	wantStream    bool

	mu                       sync.Mutex
	ts                       string
	streamState              streamState
	streamAttempted          bool
	streamStartedAt          time.Time
	streamTextIncomplete     bool
	progressTasks            map[string]bot.TaskProgress
	progressTaskOrder        []string
	progressSummary          bot.TaskProgress
	hasProgressSummary       bool
	progressReport           bool
	progressReportDirty      bool
	progressReportVersion    int
	lastProgressReportUpdate time.Time
	text                     strings.Builder

	// tasks keeps the latest state of every task card so a rotated stream can
	// start with the timeline the previous message was showing instead of an
	// empty one.
	tasks     map[string]streamPayloadChunk
	taskOrder []string
}

func (r *renderer) Start(ctx context.Context) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	if r.ts != "" {
		return nil // A task update may already have opened the stream.
	}
	if r.wantStream && !r.streamAttempted {
		r.streamAttempted = true
		ts, err := r.startStream(ctx, nil)
		if err == nil {
			r.ts = ts
			r.streamStartedAt = time.Now()
			r.setStreamState(streamActive, "start_stream")
			return nil
		}
		r.log.Warn("cannot stream into this thread; patching one message instead", "error", err)
	}
	ts, err := r.api.PostMarkdownMessage(ctx, r.channel, r.threadTS, placeholder)
	if err != nil {
		return err
	}
	r.ts = ts
	return nil
}

func (r *renderer) startStream(ctx context.Context, chunks []streamPayloadChunk) (string, error) {
	// Slack documents the recipient as required "when streaming to channels",
	// and pi-chat always streams into a thread. Send it when we have it, and
	// retry without it rather than giving up over a field Slack may not want.
	withRecipient := r.recipientUser != "" && !r.platform.recipientRefused.Load()
	recipientUser, recipientTeam := r.recipientFields(withRecipient)
	ts, err := r.api.StartStream(ctx, r.channel, r.threadTS, recipientUser, recipientTeam, chunks)
	if err != nil && withRecipient {
		r.log.Warn("streaming with a recipient was refused; retrying without one", "error", err)
		ts, err = r.api.StartStream(ctx, r.channel, r.threadTS, "", "", chunks)
		if err == nil {
			r.platform.recipientRefused.Store(true)
		}
	}
	return ts, err
}

// UpdateTasks sends simple progress through Slack's native task timeline and
// promotes observed multi-step work to a Block Kit report. A task may start the
// stream before visible text, and multi-step work may post a report without an
// answer placeholder. Batching and the report's update cadence coalesce fast
// tool activity; progress failures never prevent the ordinary final answer.
func (r *renderer) UpdateTasks(ctx context.Context, updates []bot.TaskProgress) error {
	if len(updates) == 0 {
		return nil
	}
	for _, update := range updates {
		if _, err := taskUpdateChunk(update); err != nil {
			return err
		}
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	if r.progressTasks == nil {
		r.progressTasks = make(map[string]bot.TaskProgress)
	}
	changed := false
	for _, update := range updates {
		if update.ID == botProgressSummaryID {
			if !r.hasProgressSummary || r.progressSummary != update {
				changed = true
			}
			r.progressSummary = update
			r.hasProgressSummary = true
			continue
		}
		previous, exists := r.progressTasks[update.ID]
		if !exists {
			r.progressTaskOrder = append(r.progressTaskOrder, update.ID)
			changed = true
		} else if previous != update {
			changed = true
		}
		r.progressTasks[update.ID] = update
	}

	if r.progressReport {
		r.progressReportDirty = r.progressReportDirty || changed
		return r.updateProgressReportLocked(ctx, false)
	}
	if len(r.progressTasks) >= 2 {
		return r.promoteProgressReportLocked(ctx)
	}
	if r.streamState == streamClosed || r.streamState == streamUnknown {
		return r.promoteProgressReportLocked(ctx)
	}
	if !r.wantStream {
		return nil
	}

	var timeline []bot.TaskProgress
	for _, update := range updates {
		if update.ID == botProgressSummaryID {
			timeline = append(timeline, update)
		}
	}
	if !r.hasProgressSummary && len(r.progressTasks) == 1 {
		for _, update := range updates {
			if update.ID != botProgressSummaryID {
				timeline = append(timeline, update)
			}
		}
	}
	if len(timeline) == 0 {
		return nil
	}
	chunks := make([]streamPayloadChunk, 0, len(timeline))
	for _, update := range timeline {
		chunk, err := taskUpdateChunk(update)
		if err != nil {
			return err
		}
		chunks = append(chunks, chunk)
	}
	if r.ts == "" {
		if r.streamAttempted {
			return nil
		}
		r.streamAttempted = true
		ts, err := r.startStream(ctx, chunks)
		if err != nil {
			return fmt.Errorf("start task stream: %w", err)
		}
		r.ts = ts
		r.streamStartedAt = time.Now()
		r.rememberTasks(chunks)
		r.setStreamState(streamActive, "task_start_stream")
		return nil
	}
	if r.rotationDue(time.Now()) {
		if err := r.rotate(ctx); err != nil {
			// Progress is best-effort: the text path still patches the reply,
			// and a failed rotation must not fail the turn.
			return nil
		}
	}
	if r.streamState != streamActive {
		return nil
	}
	if err := r.api.AppendStream(ctx, r.channel, r.ts, chunks); err != nil {
		r.noteAppendFailure(err)
		if r.log != nil {
			r.log.Warn("Slack task progress append failed",
				"message_ts", r.ts,
				"task_update_chunks", len(chunks),
				"task_update_text_bytes", taskUpdateTextBytes(chunks),
				"error", err)
		}
		if r.streamState == streamClosed {
			return r.promoteProgressReportLocked(ctx)
		}
		return fmt.Errorf("append task updates: %w", err)
	}
	r.rememberTasks(chunks)
	return nil
}

// rotationDue reports whether the open stream is old enough to rotate before
// Slack's 10-minute stream timeout. A renderer that never streamed (or that
// already fell back to patching) has nothing to rotate.
func (r *renderer) rotationDue(now time.Time) bool {
	if !r.wantStream || r.ts == "" || r.streamStartedAt.IsZero() {
		return false
	}
	return !now.Before(r.streamStartedAt.Add(streamRotateAfter))
}

// Tick implements bot.TurnTicker. The core calls it on the render cadence. It
// rotates a native stream before its platform closes it, and flushes a report
// held back by the update interval so a throttled change cannot leave the plan
// showing an old state until the next tool event.
func (r *renderer) Tick(ctx context.Context) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.rotationDue(time.Now()) {
		if err := r.rotate(ctx); err != nil {
			return err
		}
	}
	if r.progressReport && r.progressReportDirty {
		return r.updateProgressReportLocked(ctx, false)
	}
	return nil
}

// rotate closes the current stream and opens a new one in the same thread.
// Slack finalizes a stream 10 minutes after chat.startStream (appends do not
// extend it), moves the agent session to active, and rejects later appends
// with message_not_in_streaming_state. A turn that runs longer would otherwise
// lose its progress timeline and stop control while the agent keeps working.
//
// The previous message keeps what it streamed. The new message starts with the
// current task cards, receives only text produced after this point, and becomes
// the target Finish corrects with the authoritative answer.
func (r *renderer) rotate(ctx context.Context) error {
	if r.ts == "" {
		return nil
	}
	previous := r.ts
	expired := false
	if r.streamMayBeActive() {
		r.closeOldTasks(ctx, rotationContinuationTitle)
		err := r.api.StopStream(ctx, r.channel, previous, "processing")
		switch {
		case err == nil:
			r.setStreamState(streamClosed, "rotated")
		case streamAlreadyStopped(err):
			// Slack finalized the stream itself and set the session active.
			expired = true
			r.setStreamState(streamClosed, "rotation_found_expired")
		default:
			// Closure is unconfirmed. The next stream is still the only way to
			// keep progress flowing, but keep the uncertainty in the state.
			r.setStreamState(streamUnknown, "rotation_stop_failed")
			if r.log != nil {
				r.log.Warn("cannot close the previous Slack reply stream before rotating",
					"message_ts", previous, "error", err)
			}
		}
	} else {
		expired = true
	}

	chunks := r.taskChunks()
	ts, err := r.startStream(ctx, chunks)
	if err != nil {
		// The finalized message still carries the answer. Progress updates
		// stop, but a rotation failure must not fail the turn.
		r.wantStream = false
		if r.log != nil {
			r.log.Warn("cannot open another Slack stream; patching the reply instead",
				"message_ts", previous, "error", err)
		}
		return fmt.Errorf("open the next Slack stream: %w", err)
	}
	r.ts = ts
	r.streamStartedAt = time.Now()
	r.streamTextIncomplete = false
	r.text.Reset()
	r.setStreamState(streamActive, "rotated_stream")
	if expired {
		// Slack's timeout drops the session to active. Put the running state
		// back so the new stream keeps its loading indicator and stop button.
		thread := bot.Thread{Channel: r.channel, ThreadTS: r.threadTS}
		if err := r.platform.SetStatus(ctx, thread, bot.StatusBusy); err != nil && r.log != nil {
			r.log.Warn("cannot restore the running status after a stream timeout",
				"message_ts", ts, "error", err)
		}
	}
	if r.log != nil {
		r.log.Info("rotated the Slack reply stream to keep a long turn live",
			"previous_message_ts", previous,
			"message_ts", ts,
			"task_chunks", len(chunks),
			"expired", expired)
	}
	return nil
}

const botProgressSummaryID = "turn-progress"

func (r *renderer) progressReportTasksLocked() []bot.TaskProgress {
	if len(r.progressTaskOrder) == 0 {
		if r.hasProgressSummary {
			return []bot.TaskProgress{r.progressSummary}
		}
		return nil
	}
	tasks := make([]bot.TaskProgress, 0, len(r.progressTaskOrder))
	for _, id := range r.progressTaskOrder {
		if task, ok := r.progressTasks[id]; ok {
			tasks = append(tasks, task)
		}
	}
	return tasks
}

func (r *renderer) progressReportTitleLocked() string {
	if r.hasProgressSummary && r.progressSummary.Status != bot.TaskInProgress && r.progressSummary.Title != "" {
		return r.progressSummary.Title
	}
	return "Work in progress"
}

func (r *renderer) progressReportFallbackLocked() string {
	return r.progressReportTitleLocked()
}

// promoteProgressReport stops streaming into the reply and replaces it with a
// durable task card or plan. The final answer is posted separately so the
// report remains visible after the work finishes.
func (r *renderer) promoteProgressReportLocked(ctx context.Context) error {
	if r.progressReport {
		return nil
	}
	tasks := r.progressReportTasksLocked()
	if len(tasks) == 0 {
		return nil
	}
	blockID := fmt.Sprintf("pi-progress-%d", r.progressReportVersion+1)
	blocks := progressBlocks(blockID, r.progressReportTitleLocked(), tasks)
	fallback := r.progressReportFallbackLocked()

	if r.ts == "" {
		ts, err := r.api.PostBlocks(ctx, r.channel, r.threadTS, fallback, blocks)
		if err != nil {
			return fmt.Errorf("post progress report: %w", err)
		}
		r.ts = ts
	} else {
		if r.streamMayBeActive() {
			// Slack marks open cards as errors when their stream stops. The plan
			// update below normally replaces them immediately; close them first so
			// a failed update does not leave an error behind.
			r.closeOldTasks(ctx, promotionContinuationTitle)
			if err := r.stopStreamWithStatus(ctx, "processing"); err != nil {
				return fmt.Errorf("stop the reply stream before promoting progress: %w", err)
			}
		}
		if err := r.api.UpdateBlocks(ctx, r.channel, r.ts, fallback, blocks); err != nil {
			return fmt.Errorf("promote the reply to a progress report: %w", err)
		}
	}
	// The reply is no longer a stream: it is a Block Kit message updated in
	// place. Disable rotation so Tick does not try to rotate it.
	r.wantStream = false
	r.progressReport = true
	r.progressReportDirty = false
	r.progressReportVersion++
	r.lastProgressReportUpdate = time.Now()
	if err := r.platform.SetStatus(ctx, bot.Thread{Channel: r.channel, ThreadTS: r.threadTS}, bot.StatusBusy); err != nil && r.log != nil {
		r.log.Debug("cannot keep the promoted progress report processing", "error", err)
	}
	return nil
}

// closeOldTasks marks the current segment's open task cards complete before
// its stream is stopped, using title to say where the work continues. The task
// cache is deliberately left unchanged: a replacement stream replays the live
// in-progress cards from taskChunks().
func (r *renderer) closeOldTasks(ctx context.Context, title string) {
	updates := make([]streamPayloadChunk, 0, len(r.taskOrder))
	for _, chunk := range r.taskChunks() {
		if chunk.Status != string(bot.TaskInProgress) {
			continue
		}
		chunk.Status = string(bot.TaskComplete)
		chunk.Title = title
		chunk.Details = ""
		updates = append(updates, chunk)
	}
	if len(updates) == 0 {
		return
	}
	if err := r.api.AppendStream(ctx, r.channel, r.ts, updates); err != nil {
		if r.log != nil {
			r.log.Warn("cannot close the previous stream's task cards",
				"message_ts", r.ts, "task_update_chunks", len(updates), "error", err)
		}
		return
	}
	if r.log != nil {
		r.log.Debug("closed the previous stream's task cards",
			"message_ts", r.ts, "task_update_chunks", len(updates))
	}
}

// rememberTasks keeps the latest card state for each task ID so a rotated
// stream can reproduce the timeline.
func (r *renderer) rememberTasks(chunks []streamPayloadChunk) {
	if len(chunks) == 0 {
		return
	}
	if r.tasks == nil {
		r.tasks = make(map[string]streamPayloadChunk, len(chunks))
	}
	for _, chunk := range chunks {
		if _, exists := r.tasks[chunk.ID]; !exists {
			r.taskOrder = append(r.taskOrder, chunk.ID)
		}
		r.tasks[chunk.ID] = chunk
	}
}

// taskChunks returns the last known state of every task in the order it first
// appeared.
func (r *renderer) taskChunks() []streamPayloadChunk {
	out := make([]streamPayloadChunk, 0, len(r.taskOrder))
	for _, id := range r.taskOrder {
		if chunk, ok := r.tasks[id]; ok {
			out = append(out, chunk)
		}
	}
	return out
}

// updateProgressReportLocked coalesces task churn and honors Slack's per-message
// chat.update pacing. A forced terminal update waits only for the remaining
// interval; callers may still deliver the final answer if that update fails.
func (r *renderer) updateProgressReportLocked(ctx context.Context, force bool) error {
	if !r.progressReport || !r.progressReportDirty {
		return nil
	}
	if !r.lastProgressReportUpdate.IsZero() {
		wait := progressReportUpdateInterval - time.Since(r.lastProgressReportUpdate)
		if wait > 0 {
			if !force {
				return nil
			}
			timer := time.NewTimer(wait)
			defer timer.Stop()
			select {
			case <-timer.C:
			case <-ctx.Done():
				return ctx.Err()
			}
		}
	}
	blocks := progressBlocks(
		fmt.Sprintf("pi-progress-%d", r.progressReportVersion+1),
		r.progressReportTitleLocked(), r.progressReportTasksLocked())
	if err := r.api.UpdateBlocks(ctx, r.channel, r.ts, r.progressReportFallbackLocked(), blocks); err != nil {
		return fmt.Errorf("update progress report: %w", err)
	}
	r.progressReportVersion++
	r.lastProgressReportUpdate = time.Now()
	r.progressReportDirty = false
	return nil
}

func taskUpdateChunk(update bot.TaskProgress) (streamPayloadChunk, error) {
	if update.ID == "" || update.Title == "" {
		return streamPayloadChunk{}, errors.New("task update requires an ID and title")
	}
	switch update.Status {
	case bot.TaskInProgress, bot.TaskComplete, bot.TaskError:
	default:
		return streamPayloadChunk{}, fmt.Errorf("unsupported task status %q", update.Status)
	}
	return streamPayloadChunk{Type: "task_update",
		ID:      update.ID,
		Title:   update.Title,
		Status:  string(update.Status),
		Details: update.Details}, nil
}

func taskUpdateTextBytes(chunks []streamPayloadChunk) int {
	total := 0
	for _, chunk := range chunks {
		total = total + len(chunk.ID) + len(chunk.Title) + len(chunk.Status) + len(chunk.Details)
	}
	return total
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

	// The new text belongs to the next segment when the stream is due to
	// rotate; rotating first keeps the old segment's text out of the new
	// message (the final answer is patched into the newest message).
	if r.streamState == streamActive && r.rotationDue(time.Now()) {
		if err := r.rotate(ctx); err != nil {
			// rotate falls back to patching; the switch below carries on with
			// the finalized message.
		}
	}
	r.text.WriteString(text)
	if r.progressReport {
		return nil // The report stays stable; the authoritative answer is posted separately.
	}
	if r.ts == "" {
		// Start may fail before a message handle exists. Retain the partial
		// answer for Finish or Fail without attempting to patch an empty ts.
		return nil
	}
	switch r.streamState {
	case streamActive:
		if r.streamTextIncomplete {
			return nil
		}
		if r.text.Len() > maxMessage {
			r.streamTextIncomplete = true
			if r.log != nil {
				r.log.Warn("Slack reply exceeded the streaming text budget; deferring completion until settlement",
					"message_ts", r.ts,
					"stream_text_bytes", r.text.Len(),
					"stream_text_chars", utf8.RuneCountInString(r.text.String()),
					"stream_text_byte_budget", maxMessage)
			}
			return nil
		}
		if err := r.appendStream(ctx, text); err == nil {
			return nil
		} else {
			attrs := []any{
				"message_ts", r.ts,
				"stream_state", streamStateName(r.streamState),
				"append_text_bytes", len(text),
				"append_text_chars", utf8.RuneCountInString(text),
				"stream_text_bytes", r.text.Len(),
				"stream_text_chars", utf8.RuneCountInString(r.text.String()),
				"error", err,
			}
			if appendErr, ok := errors.AsType[*streamAppendError](err); ok {
				attrs = append(attrs,
					"append_chunk_index", appendErr.chunkIndex,
					"append_chunk_count", appendErr.chunkCount,
					"append_chunk_bytes", appendErr.chunkBytes)
			}
			if r.log != nil {
				r.log.Warn("streaming failed; deferring message fallback until closure", attrs...)
			}
			r.streamTextIncomplete = true
			r.noteAppendFailure(err)
			if r.streamState != streamClosed {
				return nil
			}
		}
	case streamUnknown:
		// Keep the original stream handle. It may still be active, so only
		// settlement may stop it and make message updates safe.
		r.streamTextIncomplete = true
		return nil
	case streamClosed:
		// Slack or a prior terminal operation closed the stream. The original
		// message remains the fallback target.
		r.streamTextIncomplete = true
	}
	return r.patch(ctx)
}

// Finish presents the authoritative answer. In timeline mode the streamed
// message is corrected when necessary; after promotion to a report, the answer
// is posted separately so the task/plan remains visible.
func (r *renderer) Finish(ctx context.Context, final string) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	streamed := r.text.String()
	if final == "" {
		final = streamed
	}
	if r.progressReport {
		if err := r.updateProgressReportLocked(ctx, true); err != nil && r.log != nil {
			r.log.Warn("cannot finalize Slack progress report", "message_ts", r.ts, "error", err)
		}
		if final == "" {
			return nil
		}
		_, err := r.api.PostMarkdownMessage(ctx, r.channel, r.threadTS, truncate(final, maxMessage))
		if err != nil {
			return fmt.Errorf("post the final answer after progress report: %w", err)
		}
		r.text.Reset()
		r.text.WriteString(final)
		return nil
	}
	if r.ts == "" {
		if final == "" {
			return nil
		}
		ts, err := r.api.PostMarkdownMessage(ctx, r.channel, r.threadTS, truncate(final, maxMessage))
		if err != nil {
			return fmt.Errorf("post the final answer: %w", err)
		}
		r.ts = ts
		r.text.Reset()
		r.text.WriteString(final)
		return nil
	}
	if r.log != nil {
		r.log.Debug("finishing Slack reply",
			"message_ts", r.ts,
			"stream_state", streamStateName(r.streamState),
			"final_matches_streamed", final == streamed,
			"final_text_bytes", len(final),
			"final_text_chars", utf8.RuneCountInString(final),
			"stream_text_bytes", len(streamed),
			"stream_text_chars", utf8.RuneCountInString(streamed),
			"stream_text_incomplete", r.streamTextIncomplete)
	}
	if r.streamMayBeActive() {
		if err := r.stopStream(ctx); err != nil {
			return fmt.Errorf("stop the stream before finishing the reply: %w", err)
		}
	}
	if r.streamState == streamClosed && final == streamed && !r.streamTextIncomplete {
		return nil
	}
	r.text.Reset()
	r.text.WriteString(final)
	patchText := truncate(final, maxMessage)
	if err := r.patch(ctx); err != nil {
		if IsCode(err, "msg_too_long") && !r.streamMayBeActive() {
			if noticeErr := r.api.UpdateMessage(ctx, r.channel, r.ts, replyTooLongNotice); noticeErr == nil {
				r.text.Reset()
				r.text.WriteString(replyTooLongNotice)
				if r.log != nil {
					r.log.Warn("Slack reply replaced with a size-limit notice",
						"message_ts", r.ts,
						"final_text_bytes", len(final),
						"patch_text_bytes", len(patchText),
						"error", err)
				}
				return nil
			} else {
				err = errors.Join(err, fmt.Errorf("update size-limit notice: %w", noticeErr))
			}
		}
		if r.log != nil {
			r.log.Warn("Slack reply patch failed",
				"message_ts", r.ts,
				"stream_state", streamStateName(r.streamState),
				"final_text_bytes", len(final),
				"patch_text_bytes", len(patchText),
				"final_text_truncated", len(final) > maxMessage,
				"error", err)
		}
		return err
	}
	if r.streamTextIncomplete && r.log != nil {
		r.log.Info("repaired Slack reply after incomplete stream",
			"message_ts", r.ts,
			"final_text_bytes", len(final),
			"patch_text_bytes", len(patchText),
			"final_text_truncated", len(final) > maxMessage)
	}
	return nil
}

// Fail reports a failed turn. Timeline mode keeps partial text in the same
// message; report mode preserves the task report and posts the failure separately.
func (r *renderer) Fail(ctx context.Context, cause error) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	text := failureText(r.text.String(), cause)
	if r.log != nil {
		r.log.Debug("reporting Slack reply failure",
			"message_ts", r.ts,
			"stream_state", streamStateName(r.streamState),
			"has_partial_text", r.text.Len() > 0)
	}
	if r.progressReport {
		if err := r.updateProgressReportLocked(ctx, true); err != nil && r.log != nil {
			r.log.Warn("cannot finalize Slack progress report after failure", "message_ts", r.ts, "error", err)
		}
		_, err := r.api.PostMarkdownMessage(ctx, r.channel, r.threadTS, truncate(text, maxMessage))
		if err != nil {
			return fmt.Errorf("post failure after progress report: %w", err)
		}
		r.text.Reset()
		r.text.WriteString(text)
		return nil
	}

	if r.streamMayBeActive() {
		if err := r.stopStream(ctx); err != nil {
			return fmt.Errorf("stop the stream before reporting the failure: %w", err)
		}
	}
	r.text.Reset()
	r.text.WriteString(text)
	if r.ts != "" {
		if err := r.patch(ctx); err == nil || !IsCode(err, "message_not_found") {
			return err
		}
	}

	ts, err := r.api.PostMarkdownMessage(ctx, r.channel, r.threadTS, truncate(r.text.String(), maxMessage))
	if err != nil {
		return fmt.Errorf("post failure message: %w", err)
	}
	r.ts = ts
	return nil
}

// failureText reserves room for the cause before truncating a partial answer.
func failureText(partial string, cause error) string {
	failure := ":warning: pi-chat: " + bot.PublicMessage(cause)
	if len(failure) > maxMessage {
		return truncate(failure, maxMessage)
	}

	const separator = "\n\n"
	available := maxMessage - len(separator) - len(failure)
	if available <= 0 {
		return failure
	}
	if len(partial) > available {
		const marker = "\n\n… _(truncated)_"
		cut := available
		if cut >= len(marker) {
			cut = cutAtRune(partial, cut-len(marker))
			partial = partial[:cut] + marker
		} else {
			cut = cutAtRune(partial, cut)
			partial = partial[:cut]
		}
	}
	if partial == "" {
		return failure
	}
	return partial + separator + failure
}

type streamAppendError struct {
	chunkIndex int
	chunkCount int
	chunkBytes int
	err        error
}

func (e *streamAppendError) Error() string {
	return fmt.Sprintf("append markdown chunk %d/%d (%d bytes): %v", e.chunkIndex, e.chunkCount, e.chunkBytes, e.err)
}

func (e *streamAppendError) Unwrap() error { return e.err }

// appendStream sends text in pieces Slack will accept, so one long delta
// cannot fail as a whole.
func (r *renderer) appendStream(ctx context.Context, text string) error {
	pieces := chunk(text, streamChunk)
	for i, piece := range pieces {
		chunk := streamPayloadChunk{Type: "markdown_text", Text: piece}
		if err := r.api.AppendStream(ctx, r.channel, r.ts, []streamPayloadChunk{chunk}); err != nil {
			return &streamAppendError{chunkIndex: i + 1, chunkCount: len(pieces), chunkBytes: len(piece), err: err}
		}
	}
	return nil
}

// noteAppendFailure distinguishes a known-closed stream from a failure whose
// effect on Slack's stream state is unknown. Neither case discards the ts.
func (r *renderer) noteAppendFailure(err error) {
	if streamAlreadyStopped(err) {
		r.setStreamState(streamClosed, "append_reported_already_stopped")
		return
	}
	r.setStreamState(streamUnknown, "append_outcome_unknown")
}

func (r *renderer) streamMayBeActive() bool {
	return r.streamState == streamActive || r.streamState == streamUnknown
}

// streamAlreadyStopped is Slack's explicit response for an operation on a
// message that is no longer in streaming state (including the agent stop
// button having already closed it).
func streamAlreadyStopped(err error) bool {
	return IsCode(err, "message_not_in_streaming_state")
}

// stopStream records closure only after Slack confirms it, either through a
// successful stop or its explicit already-stopped response.
func (r *renderer) stopStream(ctx context.Context) error {
	return r.stopStreamWithStatus(ctx, "")
}

func (r *renderer) stopStreamWithStatus(ctx context.Context, sessionStatus string) error {
	if r.ts == "" || !r.streamMayBeActive() {
		return nil
	}
	if r.log != nil {
		r.log.Debug("stopping Slack reply stream",
			"message_ts", r.ts, "stream_state", streamStateName(r.streamState))
	}
	if err := r.api.StopStream(ctx, r.channel, r.ts, sessionStatus); err != nil {
		if streamAlreadyStopped(err) {
			r.setStreamState(streamClosed, "stop_reported_already_stopped")
			return nil
		}
		if r.log != nil {
			r.log.Warn("cannot confirm Slack reply stream closure",
				"message_ts", r.ts, "stream_state", streamStateName(r.streamState), "error", err)
		}
		return err
	}
	r.setStreamState(streamClosed, "stop_succeeded")
	return nil
}

// patch replaces the whole message with what has been rendered so far. If
// Slack reports a live stream, stop it and retry once before falling back.
func (r *renderer) patch(ctx context.Context) error {
	text := truncate(r.text.String(), maxMessage)
	err := r.api.UpdateMarkdownMessage(ctx, r.channel, r.ts, text)
	if err == nil {
		return nil
	}
	if !IsCode(err, "streaming_state_conflict") {
		return err
	}

	// The update response is authoritative evidence that the message is still
	// streaming. Never post a second reply or retry the update until closure is
	// confirmed.
	previousState := r.streamState
	r.setStreamState(streamUnknown, "update_conflict")
	if r.log != nil {
		r.log.Warn("Slack message update conflicted with an active stream",
			"message_ts", r.ts,
			"stream_state_before", streamStateName(previousState),
			"stream_state", streamStateName(r.streamState),
			"error", err)
	}
	if stopErr := r.stopStream(ctx); stopErr != nil {
		return fmt.Errorf("update conflicted with an active stream (%v); stop it before retrying: %w", err, stopErr)
	}
	if err := r.api.UpdateMarkdownMessage(ctx, r.channel, r.ts, text); err != nil {
		if IsCode(err, "streaming_state_conflict") {
			previousState := r.streamState
			r.setStreamState(streamUnknown, "retry_update_conflict")
			if r.log != nil {
				r.log.Warn("Slack message update still conflicted after stopping its stream",
					"message_ts", r.ts,
					"stream_state_before", streamStateName(previousState),
					"stream_state", streamStateName(r.streamState),
					"error", err)
			}
		}
		return fmt.Errorf("update the message after stopping its stream: %w", err)
	}
	return nil
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
	if max <= 0 {
		return ""
	}
	if len(s) <= max {
		return s
	}
	const marker = "\n\n… _(truncated)_"
	if max <= len(marker) {
		return s[:cutAtRune(s, max)]
	}
	return s[:cutAtRune(s, max-len(marker))] + marker
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
