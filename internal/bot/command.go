package bot

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/tigersoldier/pi-gateway/gwclient"

	"github.com/tigersoldier/pi-chat/internal/config"
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

// ActionResume is the button that adopts an outside session from a root
// /pi resume picker.
const ActionResume = "resume"

// ActionResumeRetired restores a session this thread previously retired with
// /new. Its path is checked against the thread's durable retired-session rows.
const ActionResumeRetired = "resume-retired"

// ActionStop is the platform's own stop control rather than a button pi-chat
// posted: Slack shows one while an agent session is `processing`, and pressing
// it arrives as the `agent_session_stopped` event. The adapter hands it to the
// button path so the allowlist, the thread lookup and the dedupe all apply
// unchanged (DESIGN.md §6).
const ActionStop = "stop"

// ActionDelete and ActionDeleteCancel are the two buttons of the confirmation
// `@pi /delete` posts (DESIGN.md §5, rule 6).
//
// Two action ids rather than one carrying a yes/no value: which button was
// pressed is what the handler switches on, and Slack refuses a message whose
// actions block repeats an id — see wireActionID in the Slack adapter, which
// exists because /pi resume's picker was rejected for exactly that.
const (
	ActionDelete       = "delete"
	ActionDeleteCancel = "delete-cancel"
)

// Limits on what the bot lists, so an answer stays a message rather than a
// dump.
const (
	// resumeLimit is how many sessions /pi resume offers. It must not exceed the
	// adapter's noticeButtonLimit (internal/slack/blocks.go), or the tail would
	// be omitted from the picker.
	resumeLimit = 5
	// commandLimit is how many of the session's own commands /help lists.
	commandLimit = 20
	// modelLimit keeps /model's picker readable when many providers are configured.
	modelLimit = 20
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
			scope:   scopeRoot | scopeThread,
			summary: "adopt an outside session at the root, or restore one retired here",
			run:     (*Bot).cmdResume,
		},
		"new": {
			scope:   scopeThread,
			summary: "start a fresh session here; the previous one stays resumable",
			run:     (*Bot).cmdNew,
		},
		"abort": {
			scope:   scopeThread,
			summary: "cancel the running turn; queued prompts survive",
			run:     (*Bot).cmdAbort,
		},
		"delete": {
			scope:   scopeThread,
			summary: "delete this thread's session and its worktree",
			run:     (*Bot).cmdDelete,
		},
		"model": {
			scope:   scopeThread,
			summary: "show or switch the session's model",
			usage:   "[provider/model]",
			run:     (*Bot).cmdModel,
		},
		"stop": {
			scope:   scopeThread,
			summary: "stop the session's pi process (it can be reopened)",
			run:     (*Bot).cmdStop,
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

// cmdResume answers /pi resume with outside sessions at the root and this
// thread's retired sessions inside a thread.
func (b *Bot) cmdResume(ctx context.Context, r request, _ string) error {
	if r.inThread() {
		return b.pickRetired(ctx, r)
	}
	return b.pickResumable(ctx, r)
}

// cmdNew answers @pi /new: replace this thread's session with a fresh one.
//
// The text after the command, when there is any, is the new session's first
// prompt — otherwise the next message here is. The session being replaced stays
// resumable, and the answer says so, because somebody who wanted their work gone
// would have asked for `/pi delete` instead (DESIGN.md §4).
func (b *Bot) cmdNew(ctx context.Context, r request, arg string) error {
	th, err := b.threadFor(*r.thread)
	if err != nil {
		return err
	}
	// A turn holds turnMu for its whole life, so waiting for one would mean this
	// command answering minutes later. Refusing is immediate and honest.
	if !th.turnMu.TryLock() {
		b.answer(ctx, r, "A turn is running here. Stop it, or wait for it to finish, and try again.")
		return nil
	}
	th.sessionChanging.Store(true)
	if th.busy() {
		th.sessionChanging.Store(false)
		th.turnMu.Unlock()
		b.answer(ctx, r, "A turn is running here. Stop it, or wait for it to finish, and try again.")
		return nil
	}
	retired, err := th.retire(ctx)
	th.sessionChanging.Store(false)
	th.turnMu.Unlock()
	if err != nil {
		return err
	}
	if !retired {
		b.answer(ctx, r, "There is no session here yet — send a message and I will start one.")
		return nil
	}

	// The notice answers the command, so it is ephemeral like every other command
	// answer; the fresh session's own answer is a normal message.
	b.answer(ctx, r, "Starting a new session. The previous one stays available in `/pi resume`.")
	if text := strings.TrimSpace(arg); text != "" {
		b.startTurn(ctx, th, Message{
			Thread:    *r.thread,
			UserID:    r.userID,
			Workspace: r.workspace,
			TS:        r.ts,
			Text:      text,
			Mentioned: true,
		})
	}
	return nil
}

// cmdAbort cancels the active turn without disturbing prompts already queued
// by pi-gatewayd (DESIGN.md §5).
func (b *Bot) cmdAbort(ctx context.Context, r request, _ string) error {
	th, ok := b.tracked(*r.thread)
	if !ok || (th.snapshot().SessionPath == "" && !th.busy()) {
		b.answer(ctx, r, "This thread has no session to abort yet.")
		return nil
	}
	stopped, err := th.abort(ctx)
	if err != nil {
		return fmt.Errorf("abort the running turn: %w", err)
	}
	if !stopped {
		b.answer(ctx, r, "No turn is running in this thread.")
		return nil
	}
	b.answer(ctx, r, "The current turn was stopped. Queued prompts will still run.")
	return nil
}

// cmdModel lists the session's configured models, or switches the model when
// given an exact provider/model reference. It only attaches to an existing
// session and refuses to inspect or change model state during a turn.
func (b *Bot) cmdModel(ctx context.Context, r request, arg string) error {
	th, ok := b.tracked(*r.thread)
	if !ok || th.snapshot().SessionPath == "" {
		b.answer(ctx, r, "This thread has no session yet — send me a prompt and it gets one.")
		return nil
	}
	if !th.turnMu.TryLock() {
		b.answer(ctx, r, "A turn is running here. Wait for it to finish before changing the model.")
		return nil
	}
	defer th.turnMu.Unlock()

	ctx, cancel := context.WithTimeout(ctx, gatewayTimeout)
	defer cancel()
	client, err := th.attachExisting(ctx)
	if err != nil {
		return fmt.Errorf("attach to the session: %w", err)
	}
	if client.TurnRunning() {
		b.answer(ctx, r, "A turn is running here. Wait for it to finish before changing the model.")
		return nil
	}
	models, err := client.GetAvailableModels(ctx)
	if err != nil {
		return fmt.Errorf("list available models: %w", err)
	}
	if arg != "" {
		model, found := findAvailableModel(models, arg)
		if !found {
			b.answer(ctx, r, fmt.Sprintf("No configured model matches that reference. Use `%smodel` to see the available models.", threadForm))
			return nil
		}
		if _, err := client.SetModel(ctx, model.Provider, model.ID); err != nil {
			return fmt.Errorf("switch to model %s: %w", modelReference(model), err)
		}
		b.answer(ctx, r, "Switched this session to `"+modelReference(model)+"`.")
		return nil
	}

	state, err := client.GetState(ctx)
	if err != nil {
		return fmt.Errorf("read the current model: %w", err)
	}
	current := modelReference(gwclient.Model{Provider: state.Model.Provider, ID: state.Model.ID})
	lines := []string{"*Current model* `" + current + "`", "", "*Available models*"}
	if len(models) == 0 {
		lines = append(lines, "No models are configured.")
	} else {
		sort.Slice(models, func(i, j int) bool { return modelReference(models[i]) < modelReference(models[j]) })
		for i, model := range models {
			if i == modelLimit {
				lines = append(lines, fmt.Sprintf("_…and %d more_", len(models)-modelLimit))
				break
			}
			ref := modelReference(model)
			line := fmt.Sprintf("• `%s`", ref)
			if ref == current {
				line = fmt.Sprintf("• `%s` — current", ref)
			}
			lines = append(lines, line)
		}
	}
	b.answer(ctx, r, strings.Join(lines, "\n"))
	return nil
}

// stopAfterDetach retries only the gateway's attached-client refusal. gw_bye
// has no acknowledgement, so the just-closed thread connection can briefly
// remain registered when the separate admin stop arrives. A persistent refusal
// still fails safely; this never changes the stop's force=false behavior.
func (b *Bot) stopAfterDetach(ctx context.Context, path string) error {
	for attempt := range 5 {
		err := b.life.StopSession(ctx, path)
		if !isSessionAttached(err) || attempt == 4 {
			return err
		}
		timer := time.NewTimer(time.Duration(25<<attempt) * time.Millisecond)
		select {
		case <-ctx.Done():
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			return ctx.Err()
		case <-timer.C:
		}
	}
	return nil
}

// cmdStop gracefully stops the session's pi process but keeps its session file
// and workspace. It never forces a stop; a busy or shared session is refused by
// the gateway rather than interrupted (DESIGN.md §5, §8).
func (b *Bot) cmdStop(ctx context.Context, r request, _ string) error {
	th, ok := b.tracked(*r.thread)
	if !ok || th.snapshot().SessionPath == "" {
		b.answer(ctx, r, "This thread has no session to stop.")
		return nil
	}
	if !th.turnMu.TryLock() {
		b.answer(ctx, r, "A turn is running here. Stop it with `/abort`, or wait for it to finish.")
		return nil
	}
	defer th.turnMu.Unlock()

	ctx, cancel := context.WithTimeout(ctx, stopTimeout)
	defer cancel()
	path := th.snapshot().SessionPath
	// Stop requires the admin capability and refuses attached clients. Release
	// this thread's binding first, then let the gateway decide whether any other
	// client or active turn still makes a non-forced stop unsafe.
	th.close(ctx)
	th.update(func(row *store.ThreadRow) {
		if row.SessionPath == path {
			row.State = store.StateCold
		}
	})
	if err := b.stopAfterDetach(ctx, path); err != nil {
		if isGone(err) {
			th.forgetSessionForPath(ctx, path)
		}
		return fmt.Errorf("stop the session without force: %w", err)
	}
	th.log.Info("stopped the session's pi process", "path", path)
	b.answer(ctx, r, "Stopped pi. The session and its worktree are kept; the next prompt will reopen it.")
	return nil
}

func modelReference(model gwclient.Model) string {
	if model.Provider == "" {
		return model.ID
	}
	return model.Provider + "/" + model.ID
}

func findAvailableModel(models []gwclient.Model, reference string) (gwclient.Model, bool) {
	for _, model := range models {
		if modelReference(model) == reference {
			return model, true
		}
	}
	return gwclient.Model{}, false
}

// cmdDelete answers @pi /delete. It asks before it deletes: the session file is
// what makes the thread resumable, and the worktree is where the work is
// (DESIGN.md §5, rule 6).
//
// The confirmation names the session it would delete and its buttons carry that
// session's path, so a confirmation that outlives the session it described — a
// `/new` in between — cannot delete its successor.
func (b *Bot) cmdDelete(ctx context.Context, r request, _ string) error {
	th, ok := b.tracked(*r.thread)
	row := store.ThreadRow{}
	if ok {
		row = th.snapshot()
	}
	if row.SessionPath == "" {
		b.answer(ctx, r, "There is nothing to delete here: this thread has no session.")
		return nil
	}

	lines := []string{fmt.Sprintf("*Delete this thread's session?* `%s`", row.SessionName)}
	if row.ProjectDir != "" {
		lines = append(lines,
			fmt.Sprintf("Worktree `%s`.", row.Cwd),
			"The session file is removed, so the session cannot be resumed. Worktrees pi-chat made under that project directory go too, with the `pi/` branches they were on. Anything else the agent wrote there is left in place.")
	} else {
		lines = append(lines,
			"This session was started outside pi-chat, so its working directory is not pi-chat's to remove: only the session itself goes.")
	}
	if n, err := b.store.RetiredCount(ctx, th.key); err == nil && n > 0 {
		lines = append(lines, fmt.Sprintf("Sessions this thread retired earlier are not touched — they stay in `%sresume`.", rootForm))
	}

	return b.plat.Post(ctx, Notice{
		Thread:    r.thread,
		Channel:   r.channel,
		UserID:    r.userID,
		Ephemeral: true,
		ReplyTo:   r.replyTo,
		Text:      strings.Join(lines, "\n"),
		Buttons: []Button{
			{ActionID: ActionDelete, Text: "Delete session", Value: row.SessionPath, Style: "danger"},
			{ActionID: ActionDeleteCancel, Text: "Cancel", Value: row.SessionPath},
		},
	})
}

// threadForAction resolves the thread a button press belongs to.
//
// A platform may not report the thread a button was pressed in — Slack's payload
// for a button on an *ephemeral* message carries no thread timestamp, which is
// how /pi delete's confirmation came back with nothing to act on and did nothing
// at all — so a press is also resolved through the session it named: whichever
// thread owns that session is the thread the button belonged to.
func (b *Bot) threadForAction(ctx context.Context, a Action) (*thread, bool) {
	if a.Thread != nil && a.Thread.ThreadTS != "" {
		return b.tracked(*a.Thread)
	}
	if a.Value == "" {
		return nil, false
	}
	rows, err := b.store.Threads(ctx)
	if err != nil {
		b.log.Warn("cannot look up the session a button named", "error", err)
		return nil, false
	}
	for _, row := range rows {
		if row.SessionPath != a.Value || row.State == store.StateDeleted {
			continue
		}
		t := Thread{Workspace: row.WorkspaceID, Channel: row.ChannelID, ThreadTS: row.ThreadTS}
		if th, ok := b.tracked(t); ok {
			return th, true
		}
	}
	return nil, false
}

// deleteSession carries out a confirmed deletion: the session in the gateway,
// then what pi-chat can prove it put on disk, then the row.
//
// It runs in its own goroutine and answers through the button, replacing the
// confirmation as it goes: a delete stops pi and removes worktrees, which takes
// longer than the platform's idea of an immediate answer.
func (b *Bot) deleteSession(ctx context.Context, a Action) {
	r := request{channel: a.Channel, userID: a.UserID, workspace: a.Workspace, replyTo: a.ReplyTo}
	th, ok := b.threadForAction(ctx, a)
	if !ok {
		// The session the confirmation named belongs to no thread any more: it was
		// deleted, or the thread retired it. Say so where the button was pressed — a
		// press that answers nothing is indistinguishable from a broken bot.
		r.thread = a.Thread
		b.answer(ctx, r, "That session is not one of mine any more — nothing was deleted.")
		return
	}
	r.thread = &th.t
	row := th.snapshot()
	switch {
	case row.SessionPath == "":
		b.answer(ctx, r, "There is nothing to delete here: this thread has no session.")
		return
	case a.Value != "" && a.Value != row.SessionPath:
		// The button named a session this thread no longer owns, so the confirmation
		// no longer describes what would be deleted. Deleting the successor of the
		// session somebody agreed to delete is exactly how a stale button destroys
		// work, so this refuses instead.
		b.answer(ctx, r, fmt.Sprintf(
			"That confirmation was for another session of this thread, which is not the one here now. Ask again with `%sdelete`.", threadForm))
		return
	}
	if !th.turnMu.TryLock() {
		b.answer(ctx, r, "A turn is running here. Stop it, or wait for it to finish, then ask again.")
		return
	}
	th.sessionChanging.Store(true)
	if th.busy() {
		th.sessionChanging.Store(false)
		th.turnMu.Unlock()
		b.answer(ctx, r, "A turn is running here. Stop it, or wait for it to finish, then ask again.")
		return
	}
	defer func() {
		th.sessionChanging.Store(false)
		th.turnMu.Unlock()
	}()

	// Say what is happening before doing it: the answer is a message the user is
	// already looking at, and the work below is slower than that.
	b.reply(ctx, Notice{Thread: &th.t, Channel: th.t.Channel, UserID: a.UserID, Ephemeral: true,
		ReplyTo: a.ReplyTo, Update: a.MessageTS, ReplaceOriginal: true,
		Text: fmt.Sprintf("Deleting `%s`…", row.SessionName)})

	report, err := th.destroy(ctx)
	// Closed either way: the session is gone, and a status left at `processing`
	// would be a spinner in a thread that has nothing behind it.
	b.setStatus(ctx, th, StatusClosed)
	if err != nil {
		b.log.Warn("cannot delete the session", "thread", th.key, "error", err)
		b.reply(ctx, Notice{Thread: &th.t, Channel: th.t.Channel, UserID: a.UserID, Ephemeral: true,
			ReplyTo: a.ReplyTo, Update: a.MessageTS, ReplaceOriginal: true,
			Text: "Nothing was deleted: " + err.Error()})
		return
	}

	b.log.Info("deleted the thread's session", "thread", th.key, "user", a.UserID,
		"session", row.SessionName, "path", row.SessionPath, "dir", row.ProjectDir,
		"worktrees", len(report.Cleanup.RemovedWorktrees),
		"branches", len(report.Cleanup.PrunedBranches),
		"left", len(report.Cleanup.LeftBehind))
	b.reply(ctx, Notice{Thread: &th.t, Channel: th.t.Channel, UserID: a.UserID, Ephemeral: true,
		ReplyTo: a.ReplyTo, Update: a.MessageTS, ReplaceOriginal: true,
		Text: deleteAnswer(row, report)})
}

// cancelDelete answers the Cancel button: the confirmation is replaced and
// nothing else happens.
func (b *Bot) cancelDelete(ctx context.Context, a Action) {
	b.reply(ctx, Notice{Thread: a.Thread, Channel: a.Channel, UserID: a.UserID, Ephemeral: true,
		ReplyTo: a.ReplyTo, Update: a.MessageTS, ReplaceOriginal: true,
		Text: "Nothing was deleted."})
}

// deleteAnswer says what a completed deletion did. It names the session from the
// row as it was before the delete, because by now the row no longer has one.
func deleteAnswer(row store.ThreadRow, report deleteReport) string {
	var b strings.Builder
	if report.AlreadyGone {
		b.WriteString(fmt.Sprintf("*Deleted.* The gateway had already forgotten `%s`, so I removed what was left.",
			row.SessionName))
	} else {
		b.WriteString(fmt.Sprintf("*Deleted* `%s`.", row.SessionName))
	}
	if n := len(report.Cleanup.RemovedWorktrees); n > 0 {
		b.WriteString(fmt.Sprintf(" Removed %s and pruned %s.",
			plural(n, "worktree", "worktrees"),
			plural(len(report.Cleanup.PrunedBranches), "branch", "branches")))
	}
	if report.Cleanup.RemovedDir {
		b.WriteString(" Its project directory was empty and is gone.")
	}
	if len(report.Cleanup.LeftBehind) > 0 {
		b.WriteByte('\n')
		b.WriteString(leftBehindText(report.Cleanup.LeftBehind))
	}
	if report.CleanupErr != nil {
		b.WriteByte('\n')
		b.WriteString("The cleanup did not finish: ")
		b.WriteString(report.CleanupErr.Error())
	}
	b.WriteString("\nThe next message here starts a fresh session.")
	return b.String()
}

// leftBehindText names what a cleanup deliberately did not touch. The paths are
// the point: "something was left behind" is not actionable, and these are the
// files somebody might still want (DESIGN.md §9).
func leftBehindText(paths []string) string {
	const shown = 3
	quoted := make([]string, 0, shown)
	for i, path := range paths {
		if i == shown {
			break
		}
		quoted = append(quoted, "`"+path+"`")
	}
	text := "Not pi-chat's to remove, left in place: " + strings.Join(quoted, ", ")
	if rest := len(paths) - len(quoted); rest > 0 {
		text += fmt.Sprintf(" and %d more", rest)
	}
	return text + "."
}

// plural renders a count with its noun. The plural is given rather than derived,
// because not every noun this reports takes an "s".
func plural(n int, one, many string) string {
	if n == 1 {
		return "1 " + one
	}
	return fmt.Sprintf("%d %s", n, many)
}

// threadStates renders a channel's thread counts, naming only the states it has:
// a line that always ends in "0 deleted" is a line nobody reads.
func threadStates(warm, cold, deleted int) string {
	parts := []string{plural(warm, "warm", "warm"), plural(cold, "cold", "cold")}
	if deleted > 0 {
		parts = append(parts, plural(deleted, "deleted", "deleted"))
	}
	return strings.Join(parts, ", ")
}

// request is one inbound interaction after the allowlist, normalized across
// the three entry points so the command and turn paths share one shape.
type request struct {
	thread    *Thread // nil, or without a timestamp, at a root
	channel   string
	userID    string
	workspace string
	replyTo   string
	text      string // the command as written, slash included
	ts        string // the message it arrived in, "" for a slash command
}

// dispatch resolves one command: a control command of ours, an agent command
// of pi's, or text pi-chat does not recognize.
func (b *Bot) dispatch(ctx context.Context, c Command) {
	r := request{
		thread:    c.Thread,
		channel:   c.Channel,
		userID:    c.UserID,
		workspace: c.Workspace,
		replyTo:   c.ReplyTo,
		text:      c.Text,
		ts:        c.TS,
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
	// A command must not fall back to a blank thread: the row that could not be
	// read is what names the session, and starting one anyway is how a thread
	// ends up with two.
	th, err := b.threadFor(*r.thread)
	if err != nil {
		b.answer(ctx, r, "I cannot read this thread's state right now: "+err.Error())
		return
	}
	// The text goes as written, slash and all: pi's own resolver decides
	// whether it is a command, a template or a skill.
	m := Message{
		Thread:    *r.thread,
		UserID:    r.userID,
		Workspace: r.workspace,
		TS:        r.ts,
		Text:      r.text,
		Mentioned: true,
	}
	b.startTurn(ctx, th, m)
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
		// get_state is read only between turns: it also pokes the client's own
		// turn latch, and gwclient wakes AwaitSettled when that latch says the
		// turn stopped. Asking mid-turn could therefore end the turn early, and
		// the answer would be whatever the previous turn left behind.
		if client.TurnRunning() || th.busy() {
			lines = append(lines, "a turn is running")
		} else if state, err := client.GetState(ctx); err == nil {
			lines = append(lines, fmt.Sprintf("model: %s/%s, thinking: %s",
				state.Model.Provider, state.Model.ID, state.ThinkingLevel))
		} else {
			b.log.Debug("cannot read the session state", "thread", r.key(), "error", err)
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
	var warm, cold, deleted, here, hereWarm, hereCold, hereDeleted int
	for _, row := range rows {
		switch row.State {
		case store.StateWarm:
			warm++
		case store.StateCold:
			cold++
		case store.StateDeleted:
			deleted++
		}
		if row.ChannelID == r.channel {
			here++
			switch row.State {
			case store.StateWarm:
				hereWarm++
			case store.StateCold:
				hereCold++
			case store.StateDeleted:
				hereDeleted++
			}
		}
	}

	lines := []string{
		fmt.Sprintf("*pi-chat %s*", b.version),
		b.gw.Status(ctx),
		fmt.Sprintf("warm sessions: %d of %d allowed (idle close after %dm; cold poll %s for first %s, then %s until %s); %s",
			warm, b.cfg.Concurrency.MaxWarmSessions, b.cfg.Concurrency.ThreadIdleCloseMinutes,
			config.ColdPollFastInterval, config.ColdPollFastWindow,
			config.ColdPollSlowInterval, config.ColdPollWindow,
			threadStates(warm, cold, deleted)),
		fmt.Sprintf("threads in this channel: %d (%s)", here, threadStates(hereWarm, hereCold, hereDeleted)),
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

type retiredChoice struct {
	Version     int    `json:"v"`
	Thread      Thread `json:"t"`
	SessionPath string `json:"p"`
}

func encodeRetiredChoice(thread Thread, sessionPath string) string {
	thread.Surface = thread.SurfaceName()
	value, _ := json.Marshal(retiredChoice{Version: 1, Thread: thread, SessionPath: sessionPath})
	return string(value)
}

func decodeRetiredChoice(value string) (retiredChoice, bool) {
	var choice retiredChoice
	if err := json.Unmarshal([]byte(value), &choice); err != nil || choice.Version != 1 ||
		choice.SessionPath == "" || choice.Thread.Channel == "" || choice.Thread.ThreadTS == "" {
		return retiredChoice{}, false
	}
	switch choice.Thread.SurfaceName() {
	case SurfaceApp, SurfaceSelfDM:
		choice.Thread.Surface = choice.Thread.SurfaceName()
	default:
		return retiredChoice{}, false
	}
	return choice, true
}

// pickRetired answers @pi /resume with the sessions this thread replaced with
// /new. Unlike root adoption, each choice returns to the same chat identity.
func (b *Bot) pickRetired(ctx context.Context, r request) error {
	th, ok := b.tracked(*r.thread)
	if !ok {
		b.answer(ctx, r, "There are no sessions retired from this thread. Root `/pi resume` can adopt a session started outside pi-chat.")
		return nil
	}
	if th.snapshot().SessionPath != "" {
		b.answer(ctx, r, "This thread already has a session. Use `@pi /new` to retire it before restoring an older one.")
		return nil
	}
	retired, err := b.store.RetiredSessions(ctx, th.key)
	if err != nil {
		return fmt.Errorf("read this thread's retired sessions: %w", err)
	}
	if len(retired) == 0 {
		b.answer(ctx, r, "There are no sessions retired from this thread. Root `/pi resume` adopts sessions started outside pi-chat.")
		return nil
	}
	catalog, err := b.gw.Sessions(ctx)
	if err != nil {
		return fmt.Errorf("read the session catalog: %w", err)
	}
	byPath := make(map[string]gwclient.SessionRow, len(catalog))
	for _, row := range catalog {
		byPath[row.Path] = row
	}
	n := Notice{
		Thread:    r.thread,
		Channel:   r.channel,
		UserID:    r.userID,
		Ephemeral: true,
		ReplyTo:   r.replyTo,
		Text:      "*Sessions retired from this thread* — pick one to restore it here:",
	}
	for _, old := range retired {
		row, ok := byPath[old.SessionPath]
		if !ok {
			continue // gateway already forgot it; do not offer a stale button
		}
		n.Buttons = append(n.Buttons, Button{
			ActionID: ActionResumeRetired,
			Text:     sessionLabel(row),
			Value:    encodeRetiredChoice(th.t, old.SessionPath),
		})
		if len(n.Buttons) == resumeLimit {
			break
		}
	}
	if len(n.Buttons) == 0 {
		b.answer(ctx, r, "None of this thread's retired sessions are in the gateway catalog any more.")
		return nil
	}
	return b.plat.Post(ctx, n)
}

// resumeSession adopts the session a /pi resume button named.
func (b *Bot) resumeSession(ctx context.Context, a Action) {
	r := request{channel: a.Channel, userID: a.UserID, workspace: a.Workspace, replyTo: a.ReplyTo}
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

	// Read the previous answer before opening the thread so it can be part of
	// the root message. This read is only context for the new thread: failure or
	// an empty session must not prevent adoption.
	lastResponse, err := b.gw.LastAssistantText(ctx, chosen.Path)
	if err != nil {
		b.log.Debug("cannot read the last assistant response while adopting a session", "error", err)
		lastResponse = ""
	}

	// A session lives in a thread and this command was not in one, so the
	// thread has to be opened before it can be bound (DESIGN.md §5: picking a
	// session opens a thread and binds it).
	thread, err := b.plat.OpenThread(ctx, a.Channel, resumeOpeningMessage(*chosen, lastResponse))
	if err != nil {
		b.answer(ctx, r, "I cannot open a thread for it: "+err.Error())
		return
	}

	th, err := b.threadFor(thread)
	if err != nil {
		b.answer(ctx, r, "I found that session but cannot record it: "+err.Error())
		return
	}
	th.forgetAssistant()
	th.update(func(row *store.ThreadRow) {
		row.SessionName, row.SessionPath, row.SessionID = chosen.Name, chosen.Path, chosen.ID
		row.Cwd = chosen.Cwd
		// An adopted session's directory is not pi-chat's to delete: /delete
		// and the startup sweep leave ProjectDir empty alone (DESIGN.md §9).
		row.ProjectDir = ""
	})
	client, err := th.ensure(ctx, "")
	if err != nil {
		b.reply(ctx, Notice{Thread: &thread, Text: "I found that session but could not attach to it: " + err.Error()})
		return
	}
	th.saveCursorAfterActivity(ctx, client)
	b.reply(ctx, b.resumeNoticeForSession(ctx, thread, *chosen, a.Direct))
	// The picker is replaced rather than left behind, so the same session
	// cannot be adopted twice by an old button.
	if a.MessageTS != "" || a.ReplyTo != "" {
		b.reply(ctx, Notice{Channel: a.Channel, UserID: a.UserID, Ephemeral: true,
			ReplyTo: a.ReplyTo, Update: a.MessageTS, ReplaceOriginal: true,
			Text: fmt.Sprintf("Adopted `%s`. Continue in the new thread.", sessionName(*chosen))})
	}
}

// resumeRetiredSession restores a choice made by an in-thread /resume picker.
// The path is input from the adapter, so it is rechecked against both the
// thread's retired records and the gateway catalog before moving its binding.
func (b *Bot) resumeRetiredSession(ctx context.Context, a Action) {
	choice, ok := decodeRetiredChoice(a.Value)
	if !ok {
		r := request{channel: a.Channel, userID: a.UserID, workspace: a.Workspace, replyTo: a.ReplyTo, thread: a.Thread}
		b.answer(ctx, r, "That retired-session choice no longer identifies a thread. Ask for `@pi /resume` again.")
		return
	}
	thread := choice.Thread
	if thread.Channel != a.Channel || (a.Workspace != "" && thread.Workspace != a.Workspace) {
		r := request{channel: a.Channel, userID: a.UserID, workspace: a.Workspace, replyTo: a.ReplyTo, thread: a.Thread}
		b.answer(ctx, r, "That retired-session choice came from another conversation. Ask for `@pi /resume` again.")
		return
	}
	if a.Thread != nil && (a.Thread.Channel != thread.Channel || a.Thread.ThreadTS != thread.ThreadTS ||
		(a.Thread.Workspace != "" && a.Thread.Workspace != thread.Workspace)) {
		r := request{channel: a.Channel, userID: a.UserID, workspace: a.Workspace, replyTo: a.ReplyTo, thread: a.Thread}
		b.answer(ctx, r, "That retired-session choice no longer identifies this thread. Ask for `@pi /resume` again.")
		return
	}
	r := request{channel: a.Channel, userID: a.UserID, workspace: thread.Workspace, replyTo: a.ReplyTo, thread: &thread}
	th, ok := b.tracked(thread)
	if !ok {
		b.answer(ctx, r, "I cannot find this thread's state, so I did not restore anything.")
		return
	}
	r.thread = &th.t
	if !th.turnMu.TryLock() {
		b.answer(ctx, r, "A turn is running here. Wait for it to finish before restoring an older session.")
		return
	}
	defer th.turnMu.Unlock()
	if th.snapshot().SessionPath != "" {
		b.answer(ctx, r, "This thread already has a session. Use `@pi /new` before restoring an older one.")
		return
	}
	retired, err := b.store.RetiredSessions(ctx, th.key)
	if err != nil {
		b.answer(ctx, r, "I cannot read this thread's retired sessions: "+err.Error())
		return
	}
	var selected *store.RetiredSession
	for i := range retired {
		if retired[i].SessionPath == choice.SessionPath {
			selected = &retired[i]
			break
		}
	}
	if selected == nil {
		b.answer(ctx, r, "That session is not retired from this thread any more. Ask for `@pi /resume` again.")
		return
	}
	catalog, err := b.gw.Sessions(ctx)
	if err != nil {
		b.answer(ctx, r, "I cannot read the session catalog: "+err.Error())
		return
	}
	var chosen *gwclient.SessionRow
	for i := range catalog {
		if catalog[i].Path == selected.SessionPath {
			chosen = &catalog[i]
			break
		}
	}
	if chosen == nil {
		b.answer(ctx, r, "That session is no longer in the gateway catalog. It was not restored.")
		return
	}
	row, restored, err := b.store.RestoreRetiredSession(ctx, th.key, selected.SessionPath, chosen.ID, chosen.Cwd)
	if err != nil {
		b.answer(ctx, r, "I could not restore the session: "+err.Error())
		return
	}
	if !restored {
		b.answer(ctx, r, "That session or thread changed before I could restore it. Ask for `@pi /resume` again.")
		return
	}
	th.rowMu.Lock()
	th.row = row
	th.rowMu.Unlock()
	if _, err := th.ensure(ctx, ""); err != nil {
		b.answer(ctx, r, "I restored the session to this thread, but could not attach to it yet: "+err.Error())
		return
	}
	message := fmt.Sprintf("Restored `%s` in this thread. Continue the conversation here.", sessionName(*chosen))
	if note := b.chatToolsUnavailableNotice(ctx, chosen.Path); note != "" {
		message += "\n" + note
	}
	b.reply(ctx, Notice{Thread: &th.t, Channel: a.Channel, UserID: a.UserID, Ephemeral: true,
		ReplyTo: a.ReplyTo, Update: a.MessageTS, ReplaceOriginal: true, Text: message})
}

// How to keep going in a thread the bot just opened. The grammar differs by
// context — a channel thread answers mentions, a DM thread answers anything — so
// the notice says the one that is true where it is posted. Telling somebody in a
// DM to mention the bot, or somebody in a channel to just type, leaves them
// talking to a bot that is listening for something else (DESIGN.md §5).
const (
	continueInChannel     = "Continue in this thread by mentioning me: `@pi <what to do>`."
	continueInDM          = "This is a direct message: just type here to continue."
	resumeOpeningMaxBytes = 38000 // Slack's practical text limit with room to spare.
	resumeResponseOmitted = "The last agent response was omitted because it contained session identifiers."
)

// resumeNotice is what an adopted thread says. It carries the thread's channel as
// well as the thread, because a notice that names only its thread leaves the
// adapter with nowhere to post it — which is how the first live adoption ended in
// `channel_not_found` and silence.
func (b *Bot) resumeNoticeForSession(ctx context.Context, t Thread, row gwclient.SessionRow, direct bool) Notice {
	notice := resumeNotice(t, row, direct)
	if note := b.chatToolsUnavailableNotice(ctx, row.Path); note != "" {
		notice.Text += "\n" + note
	}
	return notice
}

// resumeOpeningMessage is the adopted session's new thread root. The picker
// label is useful here, but its last-resort filename/name fallbacks are not: an
// adoption message must not turn an opaque session identifier into chat text.
func resumeOpeningMessage(row gwclient.SessionRow, response string) string {
	label := resumeSessionLabel(row)
	opening := "Resuming a session."
	if label != "" {
		opening = "Resuming from session " + label + "."
	}
	if response = strings.TrimSpace(response); response != "" {
		if exposesSessionIdentifier(response, row) {
			return opening + "\n" + resumeResponseOmitted
		}
		prefix := "\nHere is the last agent response in that session:\n\n"
		opening += prefix + truncateResumeResponse(response, resumeOpeningMaxBytes-len(opening)-len(prefix))
	}
	return opening
}

func truncateResumeResponse(response string, maxBytes int) string {
	response = strings.ToValidUTF8(response, "\uFFFD")
	if len(response) <= maxBytes {
		return response
	}
	const ellipsis = "…"
	budget := maxBytes - len(ellipsis)
	if budget < 0 {
		return ""
	}
	for budget > 0 && !utf8.RuneStart(response[budget]) {
		budget--
	}
	return response[:budget] + ellipsis
}

func resumeSessionLabel(row gwclient.SessionRow) string {
	// sessionLabel is also used for picker buttons. Only use its title/name
	// fallback when that fallback is human-readable; never use its final file
	// fallback as the identity of an adopted session.
	title := firstSentence(row.Title)
	if title == "" {
		title = strings.TrimSpace(row.Name)
	}
	if title == "" || exposesSessionIdentifier(title, row) {
		return ""
	}
	label := sessionLabel(row)
	if exposesSessionIdentifier(label, row) {
		return ""
	}
	return label
}

func exposesSessionIdentifier(text string, row gwclient.SessionRow) bool {
	text = normalizeSlackMrkdwn(text)
	if strings.Contains(strings.ToLower(text), ".jsonl") || containsUUID(text) {
		return true
	}
	if id := normalizeSlackMrkdwn(row.ID); id != "" && containsIdentifierToken(text, id) {
		return true
	}
	for _, identifier := range []string{row.Path, filepath.Base(row.Path)} {
		identifier = normalizeSlackMrkdwn(identifier)
		if identifier != "" && identifier != "." && strings.Contains(text, identifier) {
			return true
		}
	}
	return false
}

func normalizeSlackMrkdwn(text string) string {
	return strings.Map(func(r rune) rune {
		switch r {
		case '*', '_', '~', '`', '<', '>', '|', '\\':
			return -1
		default:
			return r
		}
	}, text)
}

func containsIdentifierToken(text, identifier string) bool {
	for offset := 0; offset <= len(text)-len(identifier); {
		index := strings.Index(text[offset:], identifier)
		if index < 0 {
			return false
		}
		start := offset + index
		end := start + len(identifier)
		if (start == 0 || !isIdentifierByte(text[start-1])) &&
			(end == len(text) || !isIdentifierByte(text[end])) {
			return true
		}
		offset = start + 1
	}
	return false
}

func isIdentifierByte(c byte) bool {
	return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' ||
		c >= '0' && c <= '9' || c == '_' || c == '-'
}

func containsUUID(s string) bool {
	for start := 0; start+36 <= len(s); start++ {
		end := start + 36
		if start > 0 && isHexDigit(s[start-1]) || end < len(s) && isHexDigit(s[end]) {
			continue
		}
		valid := true
		for i := start; i < end; i++ {
			switch i - start {
			case 8, 13, 18, 23:
				valid = valid && s[i] == '-'
			default:
				valid = valid && isHexDigit(s[i])
			}
		}
		if valid {
			return true
		}
	}
	return false
}

func isHexDigit(c byte) bool {
	return c >= '0' && c <= '9' || c >= 'a' && c <= 'f' || c >= 'A' && c <= 'F'
}

func (b *Bot) chatToolsUnavailableNotice(ctx context.Context, sessionPath string) string {
	if !b.cfg.ChatTools.Enabled {
		return ""
	}
	binding, found, err := b.store.SessionToolBinding(ctx, sessionPath)
	if err != nil {
		return "I could not verify chat-tool access for this session; treat chat tools as unavailable. `@pi /new` starts a session with them enabled."
	}
	if found && binding.State == store.ToolBindingActive {
		return ""
	}
	return "Chat tools are not available in this session. `@pi /new` starts a session with them enabled."
}

func resumeNotice(t Thread, row gwclient.SessionRow, direct bool) Notice {
	howToContinue := continueInChannel
	if direct {
		howToContinue = continueInDM
	}
	return Notice{
		Thread:  &t,
		Channel: t.Channel,
		Text:    fmt.Sprintf("Attached to `%s` in `%s`. %s", sessionName(row), row.Cwd, howToContinue),
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

// sessionLabel is one button's text: what the session is about, and where it
// works — the two things that tell two sessions apart. It is the label a person
// picks from, so it is never a filename: the catalog carries a title (the first
// thing asked in the session) and an explicit name, and the directory is shown
// as its last element rather than as a path.
//
// Platforms cap a button label, so the title gives way first: cutting the whole
// label at the cap would eat the directory, which is the half that says where
// the work is.
func sessionLabel(row gwclient.SessionRow) string {
	var suffix string
	if dir := filepath.Base(row.Cwd); dir != "" && dir != "." && dir != string(filepath.Separator) {
		suffix += " · " + truncateText(dir, 24)
	}
	if row.Live {
		// pi is running for this session right now, which usually means somebody is
		// using it from a terminal.
		suffix += " · live"
	}
	budget := 70 - utf8.RuneCountInString(suffix)
	if budget < 12 {
		budget = 12
	}
	return truncateText(sessionTitle(row), budget) + suffix
}

// sessionTitle says what a session is in the fewest words it can: the title the
// catalog derived from the session's first prompt, or — when there is none — the
// session's own name, or its file as the last resort.
//
// A name is deliberately second: the only sessions that have one are pi-chat's
// own, named after the thread they came from (`slack-t6k8y3frr-…`), which says
// nothing about the work. A title does.
func sessionTitle(row gwclient.SessionRow) string {
	if title := firstSentence(row.Title); title != "" {
		return title
	}
	if row.Name != "" {
		return row.Name
	}
	return filepath.Base(row.Path)
}

// firstSentence is the first line that says something, with the wrapper pi puts
// around a skill or template invocation cut away — including its boilerplate
// ("References are relative to …"), which is machinery rather than a request.
// A session that began with `/grill-me` should be labelled by the question
// somebody asked, not by the skill that ran.
func firstSentence(title string) string {
	for {
		text := strings.TrimSpace(title)
		if !strings.HasPrefix(text, "<") {
			break
		}
		gt := strings.Index(text, ">")
		if gt < 0 {
			break
		}
		fields := strings.Fields(text[1:gt])
		if len(fields) == 0 {
			break
		}
		closing := "</" + strings.TrimSuffix(fields[0], "/") + ">"
		end := strings.Index(text, closing)
		if end < 0 {
			break
		}
		title = text[end+len(closing):]
	}
	for _, line := range strings.Split(title, "\n") {
		if line = strings.Join(strings.Fields(line), " "); line != "" {
			return line
		}
	}
	return ""
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
