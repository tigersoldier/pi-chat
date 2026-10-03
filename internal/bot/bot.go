// Package bot is pi-chat's platform-independent core (DESIGN.md §5, §12): it
// maps chat threads onto pi-gateway sessions, resolves commands, provisions
// workspaces, and drives turns.
//
// The dependency direction is one-way. Adapters (internal/slack) import this
// package and implement Platform; this package never imports an adapter, so a
// second chat platform means a second adapter, not a change here.
package bot

import (
	"context"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/tigersoldier/pi-chat/internal/config"
	"github.com/tigersoldier/pi-chat/internal/store"
	"github.com/tigersoldier/pi-chat/internal/workspace"
)

// Maintenance cadence. The sweep is what keeps a warm pi process from
// outliving the conversation that needed it; the prune keeps the dedupe table
// proportional to a day of traffic rather than to the bot's uptime.
const (
	maintenanceInterval = 30 * time.Second
	pruneInterval       = 6 * time.Hour
	eventTTL            = 24 * time.Hour
)

// Bot is the core: one instance per process, because Socket Mode distributes
// deliveries across connections (DESIGN.md §12).
type Bot struct {
	cfg     *config.Config
	log     *slog.Logger
	plat    Platform
	store   *store.Store
	work    *workspace.Provisioner
	gw      gateway
	life    lifecycle
	version string

	mu      sync.Mutex
	threads map[string]*thread
	closed  bool
}

// New builds a Bot. The caller keeps ownership of cfg, log, plat and st, and
// closes the store itself.
func New(cfg *config.Config, log *slog.Logger, plat Platform, st *store.Store, version string) *Bot {
	// One connector serves both halves of the gateway: reading the catalog and
	// changing sessions differ in the token they dial with, not in what they are.
	conn := &connector{cfg: cfg}
	return &Bot{
		cfg:     cfg,
		log:     log,
		plat:    plat,
		store:   st,
		version: version,
		work: workspace.New(workspace.Config{
			ProjectsRoot:   cfg.Paths.ProjectsRoot,
			ReposRoot:      cfg.Paths.ReposRoot,
			InjectedPrompt: cfg.Behavior.InjectedPrompt,
		}, log),
		gw:      conn,
		life:    conn,
		threads: make(map[string]*thread),
	}
}

// HandleMessage accepts one inbound message and runs its turn in the
// background, so the caller can acknowledge the platform immediately
// (DESIGN.md §7: the ack budget is three seconds).
func (b *Bot) HandleMessage(ctx context.Context, m Message) {
	// The adapter drops messages that address neither the bot nor a thread it
	// owns, so a root without a thread timestamp here is a platform that sent
	// one anyway — and a thread key without a timestamp is not an identity.
	if m.Thread.ThreadTS == "" {
		b.log.Warn("ignoring a message that names no thread", "channel", m.Thread.Channel)
		return
	}

	// A mention starts or joins a session, and in a one-to-one DM plain text does
	// too: there the conversation *is* the address. Plain text anywhere else — a
	// channel thread, a group DM — is conversation to observe and never a turn:
	// the bot sits in busy rooms, and a reply meant for somebody else must not
	// spend one. Nothing is lost by dropping it, because the next turn reads the
	// thread back from its watermark (DESIGN.md §4, §5).
	//
	// This is checked before the allowlist *and* before the dedupe record, on
	// purpose. A message nobody addressed to the bot is not a request: there is
	// nothing to refuse, and nothing to protect by claiming it first, because a
	// duplicate of a message that does nothing is nothing. Claiming first would
	// record every message in every room the bot sits in, which is the one thing
	// the dedupe table is not for (DESIGN.md §5, §7, §10).
	if !m.Mentioned && !m.Direct {
		b.log.Debug("not addressed to the bot; the next turn will read it as context",
			"thread", m.Thread.Key(), "user", m.UserID)
		// Nothing runs — and, in a thread pi-chat has a session in, the person who
		// typed is told why. Silence there looks like a broken bot, because the bot
		// may be the one who opened the thread (DESIGN.md §5).
		b.hintUnaddressed(ctx, m)
		return
	}

	// The claim comes before the allowlist, so a refused request is recorded as
	// handled too and a redelivery of it is not refused a second time.
	if !b.claim(ctx, messageClaim(m)) {
		return
	}

	if !b.allowed(m.UserID, m.Thread.Channel) {
		b.refuse(ctx, m.UserID, m.Thread.Channel, &m.Thread, "")
		return
	}

	th, err := b.threadFor(m.Thread)
	if err != nil {
		// Without the stored state a prompt could create a second session for a
		// thread that already has one, so this is a refusal, not a risk to take.
		b.reply(ctx, Notice{Thread: &m.Thread, UserID: m.UserID,
			Text: "I cannot read this thread's state right now, so I will not start anything: " + err.Error()})
		return
	}

	// A top-level message in a DM starts a session of its own — the thread is the
	// session, so a new thread is a new session by construction — and saying so is
	// what keeps that reset from being invisible when the DM already had one
	// (DESIGN.md §4, §5).
	if m.Direct && m.TS != "" && m.TS == m.Thread.ThreadTS {
		b.noticeNewDMSession(ctx, m)
	}

	// `@pi /status` is a command, not a prompt: the vocabulary is resolved
	// before anything reaches the agent (DESIGN.md §5).
	if m.Mentioned && strings.HasPrefix(strings.TrimSpace(m.Text), "/") {
		b.dispatch(ctx, Command{
			Thread:    &m.Thread,
			Channel:   m.Thread.Channel,
			UserID:    m.UserID,
			Workspace: m.Workspace,
			TS:        m.TS,
			Text:      strings.TrimSpace(m.Text),
		})
		return
	}

	switch {
	case strings.TrimSpace(m.Text) == "" && len(m.Files) == 0:
		// A bare mention: the user introduced the bot without asking anything.
		// Answering costs nothing, and it is how the grammar is discovered from
		// inside Slack.
		b.reply(ctx, Notice{Thread: &m.Thread, UserID: m.UserID, Text: mentionHelp})
		return
	case strings.TrimSpace(m.Text) == "":
		if _, ok := b.plat.(ImageReader); !ok {
			b.reply(ctx, Notice{Thread: &m.Thread, UserID: m.UserID,
				Text: "This platform cannot read attachments — send the text instead."})
			return
		}
	}

	// A mention whose whole text is a control command's name is a near miss:
	// `@pi status` meant `@pi /status`, and answering it as a prompt would
	// spend a turn to be told the obvious (DESIGN.md §5, rule 3).
	if m.Mentioned {
		if name := bareControlName(m.Text); name != "" {
			b.reply(ctx, Notice{Thread: &m.Thread, Ephemeral: true, UserID: m.UserID,
				Text: "Did you mean `@pi /" + name + "`?"})
			return
		}
	}

	b.startTurn(ctx, th, m)
}

// HandleCommand answers a command: a slash command at a channel or DM root, or
// `@pi /<command>` inside a thread (DESIGN.md §5).
// hintUnaddressed tells somebody who typed in a thread pi-chat has a session in,
// without addressing the bot, why nothing is happening.
//
// In a channel only a mention is a request (DESIGN.md §5), and nothing outside
// says so: a bot that opened the thread looks like it is waiting for a reply. The
// note is ephemeral — it is for the person who typed, not for the channel — and it
// is asked once per person per thread, recorded in the same table that dedupes
// messages, so a side conversation running alongside a session gets one answer and
// then silence.
func (b *Bot) hintUnaddressed(ctx context.Context, m Message) {
	if _, ok := b.tracked(m.Thread); !ok {
		// No session here: nobody has ever been answered in this thread, so there is
		// nothing to explain, and asking would write a row for a thread pi-chat has
		// never worked in.
		return
	}
	if !b.claim(ctx, "hint:"+m.Thread.Key()+":"+m.UserID) {
		return
	}
	b.reply(ctx, Notice{
		Thread:    &m.Thread,
		Channel:   m.Thread.Channel,
		Ephemeral: true,
		UserID:    m.UserID,
		Text:      channelReplyHint,
	})
}

func (b *Bot) HandleCommand(ctx context.Context, c Command) {
	if !b.claim(ctx, c.EventID) {
		return
	}
	if !b.allowed(c.UserID, c.Channel) {
		b.refuse(ctx, c.UserID, c.Channel, c.Thread, c.ReplyTo)
		return
	}
	go b.dispatch(ctx, c)
}

// HandleAction answers one button press.
func (b *Bot) HandleAction(ctx context.Context, a Action) {
	if !b.claim(ctx, a.EventID) {
		return
	}
	if !b.allowed(a.UserID, a.Channel) {
		b.refuse(ctx, a.UserID, a.Channel, a.Thread, a.ReplyTo)
		return
	}
	switch a.ActionID {
	case ActionResume:
		go b.resumeSession(ctx, a)
	case ActionStop:
		b.stopTurn(ctx, a)
	case ActionDelete:
		// A delete dials the gateway, stops pi and removes worktrees, so it answers
		// through the button instead of inside the delivery that carried the press.
		go b.deleteSession(ctx, a)
	case ActionDeleteCancel:
		b.cancelDelete(ctx, a)
	default:
		b.log.Warn("ignoring a button this build does not know", "action", a.ActionID)
	}
}

// HandleOpened answers the platform telling us the user opened the bot's own
// conversation: the moment to offer prompts built from this machine instead of a
// list hard-coded in a manifest (DESIGN.md §6).
func (b *Bot) HandleOpened(ctx context.Context, o Opened) {
	if !b.claim(ctx, o.EventID) {
		return
	}
	if !b.allowed(o.UserID, o.Channel) {
		// Nothing to refuse: the conversation is empty, and a refusal in it would
		// be noise a stranger cannot act on. The log is the record.
		b.log.Warn("someone outside the allowlist opened the conversation",
			"user", o.UserID, "channel", o.Channel)
		return
	}
	go b.suggestPrompts(ctx, o)
}

// Limits on what a platform is offered. Slack takes four prompts, and a list of
// four is already more than anyone reads.
const (
	suggestionLimit  = 4
	suggestionsTitle = "Try one of these"
)

// suggestPrompts offers work the repos root actually contains.
func (b *Bot) suggestPrompts(ctx context.Context, o Opened) {
	reporter, ok := b.plat.(PromptReporter)
	if !ok {
		return
	}
	suggestions := b.suggestions()
	if len(suggestions) == 0 {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, gatewayTimeout)
	defer cancel()
	if err := reporter.SetSuggestedPrompts(ctx, o.Channel, suggestionsTitle, suggestions); err != nil {
		b.log.Debug("cannot offer suggestions", "channel", o.Channel, "error", err)
	}
}

// suggestions names repositories that are really here, so the list is about this
// machine rather than about the product: the first two repositories get a
// question each, and an empty or unreadable repos root simply means no
// suggestions rather than a fabricated list.
func (b *Bot) suggestions() []Suggestion {
	var out []Suggestion
	for _, repo := range b.work.Repos() {
		out = append(out,
			Suggestion{
				Title:   "What changed in " + repo + "?",
				Message: "Summarize the uncommitted changes in " + repo + " and what I should look at.",
			},
			Suggestion{
				Title:   "Fix the tests in " + repo,
				Message: "Run the tests in " + repo + " and fix what fails.",
			})
		if len(out) >= suggestionLimit {
			return out[:suggestionLimit]
		}
	}
	return out
}

// Run performs the periodic maintenance the daemon needs, until ctx ends:
// closing connections that have gone idle and pruning the dedupe table.
//
// It blocks, and it is the caller's to run beside the socket, rather than a
// goroutine started at construction: a service that spawns work in a
// constructor cannot be shut down cleanly.
func (b *Bot) Run(ctx context.Context) error {
	sweep := time.NewTicker(maintenanceInterval)
	defer sweep.Stop()
	prune := time.NewTicker(pruneInterval)
	defer prune.Stop()

	for {
		select {
		case <-ctx.Done():
			b.Close()
			return ctx.Err()
		case <-sweep.C:
			b.closeIdle(ctx)
		case <-prune.C:
			b.pruneEvents(ctx)
		}
	}
}

// Close ends every gateway connection. Sessions are left running: they are
// pi's own state, and an idle one hibernates by itself.
func (b *Bot) Close() {
	b.mu.Lock()
	if b.closed {
		b.mu.Unlock()
		return
	}
	b.closed = true
	threads := make([]*thread, 0, len(b.threads))
	for _, th := range b.threads {
		threads = append(threads, th)
	}
	b.mu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	for _, th := range threads {
		// A turn in flight keeps its connection; the process is going away and
		// the answer is in pi's session file either way.
		if th.turnMu.TryLock() {
			th.close(ctx)
			th.turnMu.Unlock()
		}
	}
}

// closeIdle closes the connection of every thread that has been idle for
// longer than the configured window, and saves the cursor of the rest so a
// restart resumes from the right place.
//
// The session is never stopped: closing a connection is instant and cheap to
// undo, while stopping pi would throw away the process's warm state and force
// a resume (DESIGN.md §4).
func (b *Bot) closeIdle(ctx context.Context) {
	window := time.Duration(b.cfg.Concurrency.ThreadIdleCloseMinutes) * time.Minute
	now := time.Now()
	closed := 0

	for _, th := range b.allThreads() {
		row := th.snapshot()
		if row.State != store.StateWarm {
			continue
		}
		// TryLock rather than Lock: a running turn must not be interrupted by
		// maintenance, and waiting for it would stall the whole sweep.
		if !th.turnMu.TryLock() {
			continue
		}
		client := th.live()
		if client != nil {
			if now.Sub(row.LastActive) >= window {
				th.close(ctx)
				closed++
			} else {
				th.saveCursor(ctx, client)
			}
		} else if row.State == store.StateWarm {
			// The connection is gone but the row still says warm: the process
			// restarted, or the connection died without a turn noticing.
			th.update(func(row *store.ThreadRow) { row.State = store.StateCold })
		}
		th.turnMu.Unlock()
	}
	if closed > 0 {
		b.log.Info("closed idle thread connections", "count", closed, "window", window)
	}
	b.reportCap(ctx)
}

// reportCap warns when the warm set has reached the cap. Phase 1 does not evict
// yet, so the count is the whole story; phase 2 adds the queue.
func (b *Bot) reportCap(ctx context.Context) {
	// Counted from the persisted rows, which is the same source /pi status
	// reads: a marker that failed to write must not make the two disagree about
	// how close the cap is.
	rows, err := b.store.Threads(ctx)
	if err != nil {
		b.log.Warn("cannot count the warm sessions", "error", err)
		return
	}
	warm := 0
	for _, row := range rows {
		if row.State == store.StateWarm {
			warm++
		}
	}
	if warm >= b.cfg.Concurrency.MaxWarmSessions {
		b.log.Warn("the warm session cap is reached",
			"warm", warm, "cap", b.cfg.Concurrency.MaxWarmSessions)
	}
}

// pruneEvents forgets the dedupe records of events old enough that no platform
// retry can still be in flight.
func (b *Bot) pruneEvents(ctx context.Context) {
	n, err := b.store.PruneEvents(ctx, time.Now().Add(-eventTTL))
	if err != nil {
		b.log.Warn("cannot prune handled events", "error", err)
		return
	}
	if n > 0 {
		b.log.Debug("pruned handled events", "count", n)
	}
}

// threadFor returns the thread's agent, creating it when the thread is new.
//
// It fails when the thread's stored state cannot be read. Continuing with a
// blank row would be worse than failing: the row holds the session identity, and
// the next prompt would create a second session and orphan the first one's
// working directory.
func (b *Bot) threadFor(t Thread) (*thread, error) {
	if th, ok := b.tracked(t); ok {
		return th, nil
	}
	row, found, err := b.readRow(t.Key(), t)
	if err != nil {
		return nil, err
	}
	th, created := b.register(t, row)
	if created && !found {
		// The new row is written through the thread, under rowMu, so it cannot
		// overwrite session identity that a concurrent turn added in between.
		th.update(func(*store.ThreadRow) {})
		b.log.Debug("tracking a thread", "thread", th.key, "session", t.SessionName())
	}
	return th, nil
}

// tracked returns the thread's agent, loading its row if this process has not
// seen the thread since it started. It creates nothing: a read-only command
// must not write a row or open a session as a side effect.
func (b *Bot) tracked(t Thread) (*thread, bool) {
	key := t.Key()
	b.mu.Lock()
	th, ok := b.threads[key]
	b.mu.Unlock()
	if ok {
		return th, true
	}

	ctx, cancel := context.WithTimeout(context.Background(), storeTimeout)
	defer cancel()
	row, found, err := b.store.Thread(ctx, key)
	if err != nil {
		b.log.Error("cannot read the thread state", "thread", key, "error", err)
		return nil, false
	}
	if !found {
		return nil, false
	}
	th, _ = b.register(t, row)
	return th, true
}

// register adds this process's agent for a row, or returns the one another
// goroutine added first. The flag reports whether this call created it.
func (b *Bot) register(t Thread, row store.ThreadRow) (*thread, bool) {
	key := t.Key()
	b.mu.Lock()
	defer b.mu.Unlock()
	if th, ok := b.threads[key]; ok {
		return th, false
	}
	th := &thread{key: key, t: t, b: b, log: b.log.With("thread", key), row: row}
	b.threads[key] = th
	return th, true
}

// readRow reads a thread's row, or builds the row a new thread starts with. A
// failed read is an error rather than a blank row: writing a blank row over a
// real one would lose the session binding.
func (b *Bot) readRow(key string, t Thread) (store.ThreadRow, bool, error) {
	ctx, cancel := context.WithTimeout(context.Background(), storeTimeout)
	defer cancel()
	row, found, err := b.store.Thread(ctx, key)
	if err != nil {
		b.log.Error("cannot read the thread state", "thread", key, "error", err)
		return store.ThreadRow{}, false, fmt.Errorf("read the thread state: %w", err)
	}
	if found {
		return row, true, nil
	}
	return newRow(key, t), false, nil
}

// newRow is the row of a thread that has not been used before: the key and the
// platform identifiers are known, and no session exists yet.
func newRow(key string, t Thread) store.ThreadRow {
	now := time.Now()
	return store.ThreadRow{
		ThreadKey:   key,
		WorkspaceID: t.Workspace,
		ChannelID:   t.Channel,
		ThreadTS:    t.ThreadTS,
		State:       store.StateCold,
		CreatedAt:   now,
		LastActive:  now,
	}
}

// allThreads returns a snapshot of the thread map.
func (b *Bot) allThreads() []*thread {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := make([]*thread, 0, len(b.threads))
	for _, th := range b.threads {
		out = append(out, th)
	}
	return out
}

// allowed applies the platform's allowlist: a user must always be listed, and
// a non-empty channel list additionally restricts where the bot will work. It
// runs before any side effect (DESIGN.md §10).
func (b *Bot) allowed(userID, channel string) bool {
	if !slices.Contains(b.cfg.Slack.Access.AllowedUsers, userID) {
		return false
	}
	channels := b.cfg.Slack.Access.AllowedChannels
	return len(channels) == 0 || slices.Contains(channels, channel)
}

// claim records an inbound identity and reports whether this is the first time
// it has been seen. A failure to record blocks the turn: without the record, a
// platform retry would run the same prompt twice, and running it once late
// beats running it twice (DESIGN.md §7).
func (b *Bot) claim(ctx context.Context, key string) bool {
	claimed, err := b.store.ClaimEvent(ctx, key)
	if err != nil {
		b.log.Error("cannot record the inbound identity; refusing to act twice", "key", key, "error", err)
		return false
	}
	if !claimed {
		b.log.Debug("duplicate inbound ignored", "key", key)
	}
	return claimed
}

// messageClaim is the identity a message is claimed under, which is not the
// identity of the event that carried it.
//
// A platform can report one message as several events: a mention in a Slack
// channel arrives both as `app_mention` and as `message.channels`, with the same
// timestamp and two different event ids. Those are two deliveries of one
// request, and claiming by event id spends two turns — and posts two answers —
// on it. The message's own identity is the same in both, so that is what is
// claimed (DESIGN.md §7).
//
// A message a platform gives no timestamp for falls back to its event id: there
// is nothing better to dedupe by, and a platform that names no message is a
// platform whose events are its messages.
func messageClaim(m Message) string {
	if m.TS == "" {
		return m.EventID
	}
	return "msg:" + m.Thread.Workspace + ":" + m.Thread.Channel + ":" + m.TS
}

// startTurn builds a renderer and runs the turn in the background.
func (b *Bot) startTurn(ctx context.Context, th *thread, m Message) {
	th.touch()
	go func() {
		r, err := b.plat.StartTurn(ctx, m)
		if err != nil {
			b.log.Error("cannot start the reply", "thread", th.key, "error", err)
			return
		}
		// The status is the platform's loading indicator, and it is the only part
		// of this that a user sees before the first byte arrives. It goes up
		// here rather than inside the turn so that every ending — an answer, a
		// failure, a stop — takes it down again.
		b.setStatus(ctx, th, StatusBusy)
		defer b.setStatus(context.WithoutCancel(ctx), th, StatusIdle)
		if err := th.runTurn(ctx, m, r); err != nil {
			b.log.Warn("turn failed", "thread", th.key, "error", err)
			if ferr := r.Fail(ctx, err); ferr != nil {
				b.log.Error("cannot report the failure", "thread", th.key, "error", ferr)
			}
		}
	}()
}

// setStatus shows a thread session's lifecycle state, on platforms that can
// display one. It never fails a turn: a missing indicator is a cosmetic loss,
// and the adapter is the one that decides whether to try again.
func (b *Bot) setStatus(ctx context.Context, th *thread, s Status) {
	reporter, ok := b.plat.(StatusReporter)
	if !ok {
		return
	}
	if err := reporter.SetStatus(ctx, th.t, s); err != nil {
		b.log.Debug("cannot show the session status", "thread", th.key, "status", s, "error", err)
	}
}

// stopTurn handles the platform's own stop control: Slack's agent stop button,
// which arrives as an action because that is the path where the allowlist, the
// thread lookup and the dedupe already live.
//
// By the time it arrives Slack has already stopped the reply's stream, so the
// only thing left is to stop pi.
func (b *Bot) stopTurn(ctx context.Context, a Action) {
	if a.Thread == nil {
		b.log.Warn("a stop arrived without a thread to stop", "channel", a.Channel)
		return
	}
	// tracked, not threadFor: a stop is not a conversation, so it must not
	// register a row for a thread nothing has ever prompted in.
	th, ok := b.tracked(*a.Thread)
	if !ok {
		b.log.Debug("a stop arrived for a thread with no session", "thread", a.Thread.Key())
		return
	}
	if th.abort(ctx) {
		// The turn clears the status itself, wherever it ends.
		b.log.Info("stopped the turn", "thread", th.key, "user", a.UserID)
		return
	}
	// Nothing was running, so the indicator was stale: clear it rather than
	// leaving a spinner that will never stop.
	b.setStatus(ctx, th, StatusIdle)
}

// reply posts a notice, logging a failure instead of returning it: nothing the
// core posts is worth failing a turn over, and the log is where an operator
// will look anyway.
func (b *Bot) reply(ctx context.Context, n Notice) {
	if err := b.plat.Post(ctx, n); err != nil {
		b.log.Warn("cannot post a message", "channel", n.Channel, "error", err)
	}
}

// refuse answers a sender who is not on the allowlist, and records who they
// were. The reply names the fact, not the configuration: the list is the
// owner's business.
func (b *Bot) refuse(ctx context.Context, userID, channel string, thread *Thread, replyTo string) {
	// Whichever rule actually failed decides the text: "your workspace owner has
	// to add you" is a useless thing to tell a listed user who happened to type
	// in a channel that is not on the list.
	reason, text := "user is not on the allowlist", deniedUserText
	if slices.Contains(b.cfg.Slack.Access.AllowedUsers, userID) {
		reason, text = "channel is not on the allowlist", deniedChannelText
	}
	b.log.Warn("refused a request from outside the allowlist",
		"user", userID, "channel", channel, "reason", reason)

	n := Notice{Channel: channel, Ephemeral: true, UserID: userID, ReplyTo: replyTo, Text: text}
	if thread != nil && thread.ThreadTS != "" {
		n.Thread = thread
	}
	b.reply(ctx, n)
}

// piArgs is the argument list a new session starts with: the user's own
// gateway.pi_args, --approve when approvals are automatic, the project's
// injected prompt, and the instruction that explains how a Slack thread reads
// (DESIGN.md §4).
func (b *Bot) piArgs(project workspace.Project) []string {
	args := slices.Clone(b.cfg.Gateway.PiArgs)
	// The setting is what makes the policy explicit; pi_args stays the place
	// for everything else, and a hand-written --approve is not duplicated.
	if b.cfg.Behavior.Approvals == "auto" && !slices.Contains(args, "--approve") {
		args = append(args, "--approve")
	}
	args = append(args, b.work.PiArgs(project)...)
	return appendSystemPrompt(args, instructionText)
}

// SweepWorkspaces removes project directories that no thread accounts for, and
// returns the ones it removed. It is the startup GC of DESIGN.md §9: the
// remains of a crash, or of a delete that was interrupted, have no row and
// would otherwise sit in projects_root forever.
//
// A thread with a row is kept whatever its state, cold ones included: a cold
// thread is still resumable, and its directory holds the worktree the session
// is working in.
func (b *Bot) SweepWorkspaces(ctx context.Context) ([]string, error) {
	rows, err := b.store.Threads(ctx)
	if err != nil {
		return nil, err
	}
	keep := make(map[string]bool, len(rows))
	for _, row := range rows {
		if row.ProjectDir != "" {
			keep[row.ProjectDir] = true
		}
		// A thread whose session was never created still owns the directory it
		// was provisioned, which is exactly the case a crash leaves behind.
		if b.ownProject(row.Cwd) {
			keep[row.Cwd] = true
		}
	}
	// Retired sessions are still resumable, so their directories stay too: a
	// directory no row accounts for is the remains of a crash, but one a retired
	// session points at is somebody's work (DESIGN.md §4, §9).
	retired, err := b.store.RetiredProjects(ctx)
	if err != nil {
		return nil, err
	}
	for _, dir := range retired {
		keep[dir] = true
	}
	return b.work.Sweep(ctx, keep)
}

// noticeNewDMSession tells the user that this message starts a session of its
// own, when the DM already had one.
//
// It is a posted message rather than an ephemeral one: it is about the
// conversation, not about one interaction, and it is the only sign that the
// earlier session is still there (DESIGN.md §5).
func (b *Bot) noticeNewDMSession(ctx context.Context, m Message) {
	ctx, cancel := context.WithTimeout(ctx, storeTimeout)
	defer cancel()
	had, err := b.store.ChannelHasSession(ctx, m.Workspace, m.Thread.Channel, m.Thread.Key())
	if err != nil {
		b.log.Warn("cannot tell whether this DM already had a session", "error", err)
		return
	}
	if !had {
		return
	}
	b.reply(ctx, Notice{Thread: &m.Thread, UserID: m.UserID, Text: newSessionNotice})
}

// newSessionNotice is the one line that keeps a new DM session from being a
// silent reset. The prefix is the same word the command uses, so the two ways to
// start fresh look alike.
const newSessionNotice = "New session. The earlier one stays available in `/pi resume`."
