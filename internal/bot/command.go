package bot

import (
	"context"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/tigersoldier/pi-gateway/gwclient"

	"github.com/tigersoldier/pi-chat/internal/store"
)

// The two spellings of the vocabulary (DESIGN.md §5). Slack forbids developer
// slash commands inside message threads and its slash-command payload carries
// no thread timestamp, which is why a root command and a thread command cannot
// share one form.
const (
	rootForm   = "/pi "
	threadForm = "@pi /"
)

// ActionResume is the button that adopts a session listed by /pi resume. The
// adapter round-trips it; keeping the constant here keeps the vocabulary in
// one place.
const ActionResume = "resume"

// Limits on what the bot lists, so an answer stays a message rather than a
// dump.
const (
	resumeLimit  = 5
	commandLimit = 20
)

// Texts for the situations where the bot has to say no or explain itself.
const (
	deniedUserText    = "I am not allowed to work for you: your workspace owner has to add you to my allowlist."
	deniedChannelText = "I am not allowed to work in this channel."
	// rootHintText answers a message in a channel or DM root that does not
	// address the bot. A DM user has nothing else to type, so they get the two
	// ways to start a session.
	rootHintText = "Mention me to start: `@pi <what you want done>`, and I answer in a thread. " +
		"`/pi help` lists the commands that work here."
	// rootCommandHint answers a root command that needs a session.
	rootCommandHint = "That command needs a session, and sessions live in threads: " +
		"mention me with a prompt to start one, then use `@pi /<command>` there."
)

// scope says where a command is legal.
type scope uint8

const (
	// scopeRoot is a channel or DM root, where there is no session.
	scopeRoot scope = 1 << iota
	// scopeThread is inside a thread, where there is one.
	scopeThread
)

// allows reports whether the command may run in the given context.
func (s scope) allows(inThread bool) bool {
	if inThread {
		return s&scopeThread != 0
	}
	return s&scopeRoot != 0
}

// control is one command pi-chat answers itself, as opposed to one it forwards
// to pi.
type control struct {
	scope   scope
	summary string // one line for the help text
	usage   string // argument form, "" when it takes none
	run     func(b *Bot, ctx context.Context, r request, arg string) error
	// pending marks a command the grammar reserves but this build does not
	// implement. It must be answered here rather than forwarded: pi would
	// receive "/delete" as literal prompt text.
	pending string
}

// controls returns the whole vocabulary: everything not in this table is
// either one of pi's own commands or plain text, and both go to the session
// verbatim (DESIGN.md §5, rule 2).
//
// It is built in a function rather than as a package-level map literal because
// its handlers read the vocabulary back — the help text lists the commands —
// and a literal that refers to functions referring to the literal is an
// initialization cycle.
func controls() map[string]control {
	return map[string]control{
		"help": {
			scope:   scopeRoot | scopeThread,
			summary: "this message",
			run:     (*Bot).cmdHelp,
		},
		"status": {
			scope:   scopeRoot | scopeThread,
			summary: "bot status at the root; session status in a thread",
			run:     (*Bot).cmdStatus,
		},
		"resume": {
			scope:   scopeRoot,
			summary: "adopt a session that was started outside pi-chat",
			run:     (*Bot).cmdResume,
		},
		"abort": {
			scope:   scopeThread,
			summary: "cancel the running turn; queued prompts survive",
			pending: "phase 2",
		},
		"delete": {
			scope:   scopeThread,
			summary: "delete this thread's session and its worktree",
			pending: "phase 2",
		},
		"model": {
			scope:   scopeThread,
			summary: "show or switch the session's model",
			pending: "phase 2",
		},
		"stop": {
			scope:   scopeThread,
			summary: "stop the session's pi process (it can be reopened)",
			pending: "phase 2",
		},
	}
}

// cmdHelp answers /pi help and @pi /help.
func (b *Bot) cmdHelp(ctx context.Context, r request, _ string) error {
	b.answer(ctx, r, b.helpText(ctx, r))
	return nil
}

// cmdStatus answers /pi status at the root and @pi /status in a thread: the
// same word, two different questions (DESIGN.md §5).
func (b *Bot) cmdStatus(ctx context.Context, r request, _ string) error {
	if !r.inThread() {
		return b.statusRoot(ctx, r)
	}
	return b.statusThread(ctx, r)
}

// cmdResume answers /pi resume: the picker of sessions started outside
// pi-chat.
func (b *Bot) cmdResume(ctx context.Context, r request, _ string) error {
	return b.pickResumable(ctx, r)
}

// request is one inbound interaction after the allowlist, normalized across
// the three entry points so the command and turn paths share one shape.
type request struct {
	thread  *Thread // nil, or without a timestamp, at a root
	channel string
	userID  string
	teamID  string
	replyTo string
	text    string // the command as written, slash included
}

// dispatch resolves one command: a control command of ours, an agent command
// of pi's, or text pi-chat does not recognize.
func (b *Bot) dispatch(ctx context.Context, c Command) {
	r := request{
		thread:  c.Thread,
		channel: c.Channel,
		userID:  c.UserID,
		teamID:  c.TeamID,
		replyTo: c.ReplyTo,
		text:    c.Text,
	}
	name, arg := splitCommand(c.Text)
	if name == "" {
		// A bare `/pi` or `@pi /`: the user is looking for the vocabulary.
		b.answer(ctx, r, b.helpText(ctx, r))
		return
	}
	table := controls()
	cmd, ok := table[name]
	if !ok {
		b.forward(ctx, r)
		return
	}
	if !cmd.scope.allows(r.inThread()) {
		b.wrongPlace(ctx, r, name)
		return
	}
	if cmd.pending != "" {
		b.answer(ctx, r, fmt.Sprintf("`%s%s` is not in this build yet (%s).", b.form(r), name, cmd.pending))
		return
	}
	if err := cmd.run(b, ctx, r, arg); err != nil {
		b.log.Warn("a command failed", "command", name, "thread", r.key(), "error", err)
		b.answer(ctx, r, "That did not work: "+err.Error())
	}
}

// forward sends a command pi-chat does not own to pi, as a prompt: an agent
// command, a prompt template, a skill — or a typo, whose error from pi is more
// useful than one from here (DESIGN.md §5, rule 2).
func (b *Bot) forward(ctx context.Context, r request) {
	if !r.inThread() {
		b.answer(ctx, r, rootCommandHint)
		return
	}
	// The text goes as written, slash and all: pi's own resolver decides
	// whether it is a command, a template or a skill.
	m := Message{
		Thread:    *r.thread,
		UserID:    r.userID,
		TeamID:    r.teamID,
		Text:      r.text,
		Mentioned: true,
	}
	b.startTurn(ctx, b.threadFor(*r.thread), m)
}

// helpText answers /pi help and @pi /help. The two contexts list what is legal
// in that context, spelled the way it has to be typed there.
func (b *Bot) helpText(ctx context.Context, r request) string {
	if !r.inThread() {
		return strings.Join(append([]string{
			fmt.Sprintf("*pi-chat %s* — I drive a pi coding agent on the machine pi-gatewayd runs on.", b.version),
			"",
			"*Start a session*: mention me with a prompt and I answer in a thread.",
			"`@pi fix the failing test`",
			"Everything typed in that thread reaches the same session, mention or not.",
			"",
			"*Commands here* (channel or DM root):",
		}, append(b.commandLines(scopeRoot, rootForm),
			"",
			"*In a thread*: `@pi /help`, `@pi /status`, and pi's own commands — `@pi /compact`, `@pi /skill:<name>`, prompt templates.",
		)...), "\n")
	}

	lines := []string{
		"*This thread is one pi session.* Everything typed here reaches it.",
		"",
		"*Commands*:",
	}
	lines = append(lines, b.commandLines(scopeThread, threadForm)...)
	if commands, ok := b.sessionCommands(ctx, r); ok {
		lines = append(lines, "", "*pi's commands in this session* (passed straight through):")
		for i, c := range commands {
			if i == commandLimit {
				lines = append(lines, fmt.Sprintf("• _…and %d more_", len(commands)-commandLimit))
				break
			}
			line := "• `" + threadForm + c.Name + "`"
			if c.Description != "" {
				line += " — " + c.Description
			}
			lines = append(lines, line)
		}
	}
	return strings.Join(lines, "\n")
}

// sessionCommands lists the agent commands of a thread that is already warm.
// A help request must not dial, create or wake anything, so a cold thread
// simply gets no list.
func (b *Bot) sessionCommands(ctx context.Context, r request) ([]gwclient.Command, bool) {
	th, ok := b.tracked(*r.thread)
	if !ok {
		return nil, false
	}
	client := th.live()
	if client == nil {
		return nil, false
	}
	ctx, cancel := context.WithTimeout(ctx, gatewayTimeout)
	defer cancel()
	commands, err := client.GetCommands(ctx)
	if err != nil {
		b.log.Debug("cannot list the session's commands", "thread", r.key(), "error", err)
		return nil, false
	}
	return commands, len(commands) > 0
}

// statusThread answers @pi /status: what this thread's session is.
func (b *Bot) statusThread(ctx context.Context, r request) error {
	th, ok := b.tracked(*r.thread)
	if !ok {
		b.answer(ctx, r, "This thread has no session yet — send me a prompt and it gets one.")
		return nil
	}
	row := th.snapshot()

	lines := []string{fmt.Sprintf("*Session* `%s`", row.SessionName)}
	if row.SessionID != "" {
		lines = append(lines, fmt.Sprintf("id `%s`", row.SessionID))
	}
	lines = append(lines, fmt.Sprintf("worktree `%s`", row.Cwd))
	switch {
	case row.State == store.StateWarm:
		lines = append(lines, "state: warm (pi is running for this thread)")
	case row.State == store.StateDeleted:
		lines = append(lines, "state: deleted")
	default:
		lines = append(lines, "state: cold (the next message reopens it)")
	}
	if client := th.live(); client != nil {
		if state, err := client.GetState(ctx); err == nil {
			lines = append(lines, fmt.Sprintf("model: %s/%s, thinking: %s",
				state.Model.Provider, state.Model.ID, state.ThinkingLevel))
		} else {
			b.log.Debug("cannot read the session state", "thread", r.key(), "error", err)
		}
		if client.TurnRunning() {
			lines = append(lines, "a turn is running")
		}
	}
	lines = append(lines, "last active: "+humanSince(row.LastActive))
	b.answer(ctx, r, strings.Join(lines, "\n"))
	return nil
}

// statusRoot answers /pi status: the bot, the gateway, and this channel.
func (b *Bot) statusRoot(ctx context.Context, r request) error {
	rows, err := b.store.Threads(ctx)
	if err != nil {
		return err
	}
	var warm, cold, here, hereWarm int
	for _, row := range rows {
		switch row.State {
		case store.StateWarm:
			warm++
		case store.StateCold:
			cold++
		}
		if row.ChannelID == r.channel {
			here++
			if row.State == store.StateWarm {
				hereWarm++
			}
		}
	}

	lines := []string{
		fmt.Sprintf("*pi-chat %s*", b.version),
		b.gw.Status(ctx),
		fmt.Sprintf("warm sessions: %d of %d allowed (idle close after %dm)",
			warm, b.cfg.Concurrency.MaxWarmSessions, b.cfg.Concurrency.ThreadIdleCloseMinutes),
		fmt.Sprintf("threads in this channel: %d (%d warm, %d cold)", here, hereWarm, here-hereWarm),
		fmt.Sprintf("render: %s, approvals: %s", b.cfg.Render.Mode, b.cfg.Behavior.Approvals),
	}
	b.answer(ctx, r, strings.Join(lines, "\n"))
	return nil
}

// pickResumable answers /pi resume with the sessions it could adopt, one
// button each.
func (b *Bot) pickResumable(ctx context.Context, r request) error {
	rows, err := b.adoptable(ctx)
	if err != nil {
		return err
	}
	if len(rows) == 0 {
		b.answer(ctx, r, "There is nothing to adopt: every session in the gateway's catalog is either already reachable in Slack or was started by pi-chat.")
		return nil
	}
	n := Notice{
		Channel:   r.channel,
		UserID:    r.userID,
		Ephemeral: true,
		ReplyTo:   r.replyTo,
		Text:      "*Sessions I can adopt* — pick one and I continue it in a new thread:",
	}
	for _, row := range rows {
		n.Buttons = append(n.Buttons, Button{
			ActionID: ActionResume,
			Text:     sessionLabel(row),
			Value:    row.Path,
		})
	}
	return b.plat.Post(ctx, n)
}

// resumeSession adopts the session a /pi resume button named.
func (b *Bot) resumeSession(ctx context.Context, a Action) {
	r := request{channel: a.Channel, userID: a.UserID, teamID: a.TeamID, replyTo: a.ReplyTo}
	// The button's value travelled through the platform, so it comes back as
	// input rather than as fact: it is checked against a fresh catalog read
	// before anything is adopted.
	rows, err := b.adoptable(ctx)
	if err != nil {
		b.answer(ctx, r, "I cannot read the session catalog: "+err.Error())
		return
	}
	var chosen *gwclient.SessionRow
	for i := range rows {
		if rows[i].Path == a.Value {
			chosen = &rows[i]
			break
		}
	}
	if chosen == nil {
		b.answer(ctx, r, "That session is gone, or a thread already owns it — `/pi resume` lists what is available.")
		return
	}

	// A session lives in a thread and this command was not in one, so the
	// thread has to be opened before it can be bound (DESIGN.md §5: picking a
	// session opens a thread and binds it).
	thread, err := b.plat.OpenThread(ctx, a.Channel, "Adopting the session `"+sessionName(*chosen)+"`")
	if err != nil {
		b.answer(ctx, r, "I cannot open a thread for it: "+err.Error())
		return
	}

	th := b.threadFor(thread)
	th.update(func(row *store.ThreadRow) {
		row.SessionName, row.SessionPath, row.SessionID = chosen.Name, chosen.Path, chosen.ID
		row.Cwd = chosen.Cwd
		// An adopted session's directory is not pi-chat's to delete: /delete
		// and the startup sweep leave ProjectDir empty alone (DESIGN.md §9).
		row.ProjectDir = ""
	})
	if _, err := th.ensure(ctx, ""); err != nil {
		b.reply(ctx, Notice{Thread: &thread, Text: "I found that session but could not attach to it: " + err.Error()})
		return
	}
	b.reply(ctx, Notice{Thread: &thread,
		Text: fmt.Sprintf("Attached to `%s` in `%s`. Keep going here.", sessionName(*chosen), chosen.Cwd)})
	// The picker is replaced rather than left behind, so the same session
	// cannot be adopted twice by an old button.
	if a.MessageTS != "" || a.ReplyTo != "" {
		b.reply(ctx, Notice{Channel: a.Channel, UserID: a.UserID, Ephemeral: true,
			ReplyTo: a.ReplyTo, Update: a.MessageTS,
			Text: fmt.Sprintf("Adopted `%s`. Continue in the new thread.", sessionName(*chosen))})
	}
}

// answer posts a command's answer to whoever asked. Answers are ephemeral:
// they are about the session, not part of the conversation, and a thread full
// of status messages buries the answers.
func (b *Bot) answer(ctx context.Context, r request, text string) {
	n := Notice{Channel: r.channel, UserID: r.userID, Ephemeral: true, ReplyTo: r.replyTo, Text: text}
	if r.inThread() {
		n.Thread = r.thread
	}
	b.reply(ctx, n)
}

// wrongPlace explains that a command exists, but not here.
func (b *Bot) wrongPlace(ctx context.Context, r request, name string) {
	if r.inThread() {
		b.answer(ctx, r, fmt.Sprintf("`%s%s` is a channel command: it works where sessions are started, not inside one. Type `%s%s` at the channel root.",
			threadForm, name, rootForm, name))
		return
	}
	b.answer(ctx, r, fmt.Sprintf("`%s%s` needs a session, and sessions live in threads. Use `%s%s` inside the thread instead.",
		rootForm, name, threadForm, name))
}

// form is the spelling of a command in this context, for messages that name
// one.
func (b *Bot) form(r request) string {
	if r.inThread() {
		return threadForm
	}
	return rootForm
}

// commandLines renders the commands legal in one context, spelled the way they
// have to be typed there.
func (b *Bot) commandLines(s scope, form string) []string {
	table := controls()
	var names []string
	for name, cmd := range table {
		if cmd.scope.allows(s == scopeThread) {
			names = append(names, name)
		}
	}
	sort.Strings(names)

	lines := make([]string, 0, len(names))
	for _, name := range names {
		cmd := table[name]
		usage := name
		if cmd.usage != "" {
			usage += " " + cmd.usage
		}
		line := "• `" + form + usage + "` — " + cmd.summary
		if cmd.pending != "" {
			line += " _(not in this build yet)_"
		}
		lines = append(lines, line)
	}
	return lines
}

// splitCommand separates a command word from its argument: "/skill:x a b" is
// ("skill:x", "a b"). Text that is not written as a command yields no name, so
// the caller can tell `/status` from a sentence that begins with a slash.
func splitCommand(text string) (name, arg string) {
	text = strings.TrimSpace(text)
	if !strings.HasPrefix(text, "/") {
		return "", ""
	}
	word, rest, _ := strings.Cut(strings.TrimPrefix(text, "/"), " ")
	return strings.ToLower(strings.TrimSpace(word)), strings.TrimSpace(rest)
}

// bareControlName reports whether a message is exactly a control command's
// name with the slash missing — the near miss `@pi status` for `@pi /status`.
//
// Only the whole message counts. "delete the old branch" is a prompt, and
// interrupting it with a hint about `/delete` would be worse than running it,
// so the guard covers the shape a user actually types when they mean the
// command (DESIGN.md §5, rule 3).
func bareControlName(text string) string {
	word := strings.TrimSpace(text)
	if word == "" || strings.ContainsAny(word, " \t\n") {
		return ""
	}
	name := strings.ToLower(word)
	if _, ok := controls()[name]; !ok {
		return ""
	}
	return name
}

// inThread reports whether the request carries a session's thread.
func (r request) inThread() bool { return r.thread != nil && r.thread.ThreadTS != "" }

// key names the thread for logs.
func (r request) key() string {
	if r.thread == nil {
		return ""
	}
	return r.thread.Key()
}

// sessionName is a session's human name, falling back to the file when the
// session has none.
func sessionName(row gwclient.SessionRow) string {
	if row.Name != "" {
		return row.Name
	}
	return filepath.Base(row.Path)
}

// sessionLabel is one button's text. Platforms cap button labels, and the
// directory is what tells two sessions of one repository apart.
func sessionLabel(row gwclient.SessionRow) string {
	label := sessionName(row)
	if row.Cwd != "" {
		label += " · " + row.Cwd
	}
	return truncateText(label, 70)
}

// truncateText cuts s to at most n runes, marking that it was cut.
func truncateText(s string, n int) string {
	runes := []rune(s)
	if len(runes) <= n {
		return s
	}
	return string(runes[:n-1]) + "…"
}

// humanSince renders how long ago something happened, coarsely: a status
// message is read to notice that something is stale, not to count seconds.
func humanSince(t time.Time) string {
	switch d := time.Since(t); {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd ago", int(d.Hours()/24))
	}
}
