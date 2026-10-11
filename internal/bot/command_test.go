package bot

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/tigersoldier/pi-gateway/gwclient"

	"github.com/tigersoldier/pi-chat/internal/config"
	"github.com/tigersoldier/pi-chat/internal/store"
)

func TestSplitCommand(t *testing.T) {
	tests := []struct {
		text string
		name string
		arg  string
	}{
		{"/status", "status", ""},
		{"/skill:grill-me", "skill:grill-me", ""},
		{"/skill:grill-me be blunt", "skill:grill-me", "be blunt"},
		{"/compact  keep the plan ", "compact", "keep the plan"},
		{"/", "", ""},
		{"  /help  ", "help", ""},
		{"status", "", ""},
		{"", "", ""},
		{"fix the /thing", "", ""},
	}
	for _, test := range tests {
		name, arg := splitCommand(test.text)
		if name != test.name || arg != test.arg {
			t.Errorf("splitCommand(%q) = (%q, %q), want (%q, %q)",
				test.text, name, arg, test.name, test.arg)
		}
	}
}

func TestBareControlName(t *testing.T) {
	tests := []struct{ text, want string }{
		{"status", "status"},
		{"Status", "status"},
		{" delete ", "delete"},
		// Only a message that is exactly the command counts: these are prompts,
		// and interrupting them with a hint would be worse than running them.
		{"status report", ""},
		{"delete the old branch", ""},
		{"hello", ""},
		{"", ""},
		{"/status", ""},
	}
	for _, test := range tests {
		if got := bareControlName(test.text); got != test.want {
			t.Errorf("bareControlName(%q) = %q, want %q", test.text, got, test.want)
		}
	}
}

func TestCommandScope(t *testing.T) {
	tests := []struct {
		name     string
		inThread bool
		want     bool
	}{
		{"help", false, true},
		{"help", true, true},
		{"status", false, true},
		{"status", true, true},
		{"resume", false, true},
		{"resume", true, true},
		{"delete", false, false},
		{"delete", true, true},
	}
	for _, test := range tests {
		if got := controls()[test.name].scope.allows(test.inThread); got != test.want {
			t.Errorf("controls[%q] in thread=%v = %v, want %v", test.name, test.inThread, got, test.want)
		}
	}
}

// commandRequest builds the root or thread form of a command.
func commandRequest(threadTS, text string) Command {
	c := Command{EventID: "Ev-" + text, Channel: "C1", UserID: "U1", Workspace: "T1", Text: text}
	if threadTS != "" {
		c.Thread = &Thread{Workspace: "T1", Channel: "C1", ThreadTS: threadTS}
	}
	return c
}

func TestDispatchHelpListsTheCommandsOfItsContext(t *testing.T) {
	t.Run("root", func(t *testing.T) {
		b, platform, _ := newTestBot(t)
		b.HandleCommand(context.Background(), commandRequest("", "/help"))

		waitFor(t, "the answer", func() bool { return len(platform.postedNotices()) == 1 })
		text := platform.postedNotices()[0].Text
		if !strings.Contains(text, "/pi status") || !strings.Contains(text, "/pi resume") {
			t.Errorf("the root help does not list the root commands:\n%s", text)
		}
		// A thread command must be spelled the way it has to be typed there.
		if strings.Contains(text, "• `/pi abort`") {
			t.Errorf("the root help lists a command that cannot run there:\n%s", text)
		}
	})

	t.Run("thread", func(t *testing.T) {
		b, platform, _ := newTestBot(t)
		b.HandleCommand(context.Background(), commandRequest("1700000000.000100", "/help"))

		waitFor(t, "the answer", func() bool { return len(platform.postedNotices()) == 1 })
		text := platform.postedNotices()[0].Text
		if !strings.Contains(text, "@pi /status") || !strings.Contains(text, "@pi /delete") {
			t.Errorf("the thread help does not list the thread commands:\n%s", text)
		}
		if strings.Contains(text, "• `/pi resume`") {
			t.Errorf("the thread help lists a channel command:\n%s", text)
		}
	})
}

func TestDispatchStatusAtTheRoot(t *testing.T) {
	b, platform, st := newTestBot(t)
	b.gw = &fakeGateway{status: "gateway: reachable, pi 1.2.3, 4 sessions (2 live, 1 created by pi-chat)"}
	seedThread(t, st, store.ThreadRow{
		ThreadKey: "T1:C1:1", WorkspaceID: "T1", ChannelID: "C1", ThreadTS: "1",
		SessionPath: "/sessions/a.jsonl", State: store.StateWarm,
	})
	seedThread(t, st, store.ThreadRow{
		ThreadKey: "T1:C1:2", WorkspaceID: "T1", ChannelID: "C1", ThreadTS: "2",
		SessionPath: "/sessions/b.jsonl", State: store.StateCold,
	})
	seedThread(t, st, store.ThreadRow{
		ThreadKey: "T1:C9:3", WorkspaceID: "T1", ChannelID: "C9", ThreadTS: "3",
		SessionPath: "/sessions/c.jsonl", State: store.StateCold,
	})

	b.HandleCommand(context.Background(), commandRequest("", "/status"))

	waitFor(t, "the answer", func() bool { return len(platform.postedNotices()) == 1 })
	text := platform.postedNotices()[0].Text
	for _, want := range []string{"gateway: reachable", "warm sessions: 1 of", "threads in this channel: 2"} {
		if !strings.Contains(text, want) {
			t.Errorf("the status is missing %q:\n%s", want, text)
		}
	}
}

func TestDispatchStatusInAThread(t *testing.T) {
	t.Run("no session yet", func(t *testing.T) {
		b, platform, _ := newTestBot(t)
		b.HandleCommand(context.Background(), commandRequest("1700000000.000100", "/status"))

		waitFor(t, "the answer", func() bool { return len(platform.postedNotices()) == 1 })
		if text := platform.postedNotices()[0].Text; !strings.Contains(text, "no session") {
			t.Errorf("the answer does not say the thread has no session: %q", text)
		}
	})

	t.Run("a stored session", func(t *testing.T) {
		b, platform, st := newTestBot(t)
		seedThread(t, st, store.ThreadRow{
			ThreadKey: "T1:C1:1700000000.000100", WorkspaceID: "T1", ChannelID: "C1",
			ThreadTS: "1700000000.000100", SessionName: "slack-t1-c1-1700000000-000100",
			SessionID: "abc", SessionPath: "/sessions/a.jsonl", Cwd: "/work/2026-09-27-fix",
			State: store.StateCold, LastActive: time.Now().Add(-5 * time.Minute),
		})

		b.HandleCommand(context.Background(), commandRequest("1700000000.000100", "/status"))

		waitFor(t, "the answer", func() bool { return len(platform.postedNotices()) == 1 })
		text := platform.postedNotices()[0].Text
		for _, want := range []string{"slack-t1-c1-1700000000-000100", "/work/2026-09-27-fix", "cold"} {
			if !strings.Contains(text, want) {
				t.Errorf("the status is missing %q:\n%s", want, text)
			}
		}
	})
}

func TestDispatchResumeOffersOnlyAdoptableSessions(t *testing.T) {
	b, platform, st := newTestBot(t)
	// One session is already ours (a row), one was created by pi-chat (its cwd
	// is under the projects root), and one is a stranger's: only the stranger
	// is adoptable.
	seedThread(t, st, store.ThreadRow{
		ThreadKey: "T1:C1:1", WorkspaceID: "T1", ChannelID: "C1", ThreadTS: "1",
		SessionPath: "/sessions/ours.jsonl", State: store.StateCold,
	})
	if err := st.RetireSession(context.Background(), store.RetiredSession{
		SessionPath: "/sessions/retired-outside.jsonl", ThreadKey: "T1:C2:2",
		SessionName: "retired-outside", ProjectDir: "",
	}); err != nil {
		t.Fatalf("RetireSession: %v", err)
	}
	b.gw = &fakeGateway{rows: []gwclient.SessionRow{
		{Path: "/sessions/ours.jsonl", Cwd: "/somewhere"},
		{Path: "/sessions/made-by-pi-chat.jsonl", Cwd: b.cfg.Paths.ProjectsRoot + "/2026-09-27-x"},
		{Path: "/sessions/retired-outside.jsonl", Name: "retired-outside", Cwd: "/home/pi/code/old"},
		{Path: "/sessions/theirs.jsonl", Name: "their-session", Cwd: "/home/pi/code/thing"},
	}}

	b.HandleCommand(context.Background(), commandRequest("", "/resume"))

	waitFor(t, "the picker", func() bool { return len(platform.postedNotices()) == 1 })
	notice := platform.postedNotices()[0]
	if !notice.Ephemeral {
		t.Error("a picker should be visible only to whoever asked")
	}
	if len(notice.Buttons) != 1 {
		t.Fatalf("offered %d sessions, want only the adoptable one: %+v", len(notice.Buttons), notice.Buttons)
	}
	if got := notice.Buttons[0].Value; got != "/sessions/theirs.jsonl" {
		t.Errorf("button value = %q, want the session path", got)
	}
	if notice.Buttons[0].ActionID != ActionResume {
		t.Errorf("button action = %q, want %q", notice.Buttons[0].ActionID, ActionResume)
	}
}

func TestDispatchResumeInThreadOffersOnlyThatThreadsRetiredSessions(t *testing.T) {
	b, platform, st := newTestBot(t)
	thread := Thread{Workspace: "T1", Channel: "C1", ThreadTS: "1700000000.000100"}
	if _, err := b.threadFor(thread); err != nil {
		t.Fatalf("threadFor: %v", err)
	}
	if err := st.RetireSession(context.Background(), store.RetiredSession{
		SessionPath: "/sessions/old.jsonl", ThreadKey: thread.Key(), SessionName: "old-session", ProjectDir: "/work/old",
	}); err != nil {
		t.Fatalf("RetireSession: %v", err)
	}
	if err := st.RetireSession(context.Background(), store.RetiredSession{
		SessionPath: "/sessions/other-thread.jsonl", ThreadKey: "T1:C2:other", SessionName: "other-session",
	}); err != nil {
		t.Fatalf("RetireSession: %v", err)
	}
	b.gw = &fakeGateway{rows: []gwclient.SessionRow{
		{Path: "/sessions/old.jsonl", Name: "old-session"},
		{Path: "/sessions/other-thread.jsonl", Name: "other-session"},
	}}

	b.HandleCommand(context.Background(), commandRequest(thread.ThreadTS, "/resume"))
	waitFor(t, "the retired picker", func() bool { return len(platform.postedNotices()) == 1 })
	notice := platform.postedNotices()[0]
	if len(notice.Buttons) != 1 || notice.Buttons[0].Value != "/sessions/old.jsonl" || notice.Buttons[0].ActionID != ActionResumeRetired {
		t.Fatalf("retired picker = %+v, want only this thread's old session", notice.Buttons)
	}
	if notice.Thread == nil || notice.Thread.ThreadTS != thread.ThreadTS {
		t.Errorf("retired picker was not posted in the original thread: %+v", notice.Thread)
	}
}

func TestResumeRetiredSessionRestoresOriginalThreadEvenIfGatewayAttachMustRetry(t *testing.T) {
	b, platform, st := newTestBot(t)
	thread := Thread{Workspace: "T1", Channel: "C1", ThreadTS: "1700000000.000100"}
	th, err := b.threadFor(thread)
	if err != nil {
		t.Fatalf("threadFor: %v", err)
	}
	const path = "/sessions/old.jsonl"
	if err := st.RetireSession(context.Background(), store.RetiredSession{
		SessionPath: path, ThreadKey: thread.Key(), SessionName: "old-session", ProjectDir: "/work/old",
		ObservedTS: "1700000000.000500", LastSeq: 42, LeafID: "leaf-42",
	}); err != nil {
		t.Fatalf("RetireSession: %v", err)
	}
	b.gw = &fakeGateway{rows: []gwclient.SessionRow{{Path: path, Name: "old-session", ID: "sid-old", Cwd: "/work/old/wt"}}}

	b.HandleAction(context.Background(), Action{
		EventID: "Ev-resume-retired", Channel: thread.Channel, Thread: &thread, UserID: "U1", Workspace: thread.Workspace,
		ActionID: ActionResumeRetired, Value: path, MessageTS: "1700000000.000900",
	})
	waitFor(t, "restored row", func() bool { return th.snapshot().SessionPath == path })
	row, found, err := st.Thread(context.Background(), thread.Key())
	if err != nil || !found {
		t.Fatalf("restored thread: found=%v err=%v", found, err)
	}
	if row.SessionID != "sid-old" || row.Cwd != "/work/old/wt" || row.ProjectDir != "/work/old" ||
		row.ObservedTS != "1700000000.000500" || row.LastSeq != 42 || row.LeafID != "leaf-42" {
		t.Errorf("restored thread lost its session context: %+v", row)
	}
	if retired, err := st.RetiredSessions(context.Background(), thread.Key()); err != nil || len(retired) != 0 {
		t.Errorf("retired rows after restore = %+v, %v", retired, err)
	}
	waitFor(t, "the attach retry notice", func() bool {
		for _, notice := range platform.postedNotices() {
			if strings.Contains(notice.Text, "could not attach to it yet") {
				return true
			}
		}
		return false
	})
}

func TestDispatchResumeWithoutCandidates(t *testing.T) {
	b, platform, _ := newTestBot(t)
	b.gw = &fakeGateway{}

	b.HandleCommand(context.Background(), commandRequest("", "/resume"))

	waitFor(t, "the answer", func() bool { return len(platform.postedNotices()) == 1 })
	notice := platform.postedNotices()[0]
	if len(notice.Buttons) != 0 {
		t.Errorf("offered buttons for an empty catalog: %+v", notice.Buttons)
	}
	if !strings.Contains(notice.Text, "nothing to adopt") {
		t.Errorf("the answer does not explain the empty list: %q", notice.Text)
	}
}

func TestResumeSessionAdoptsTheChosenSession(t *testing.T) {
	b, platform, _ := newTestBot(t)
	b.gw = &fakeGateway{rows: []gwclient.SessionRow{
		{Path: "/sessions/theirs.jsonl", Name: "their-session", ID: "sid", Cwd: "/home/pi/code/thing"},
	}}

	b.HandleAction(context.Background(), Action{
		EventID: "Ev-action", Channel: "C1", UserID: "U1", Workspace: "T1",
		ActionID: ActionResume, Value: "/sessions/theirs.jsonl", MessageTS: "1700000000.000900",
	})

	waitFor(t, "the adoption", func() bool {
		row, found, err := b.store.Thread(context.Background(), "T1:C1:900.000001")
		return err == nil && found && row.SessionPath == "/sessions/theirs.jsonl"
	})

	if opened := platform.openedThreads(); len(opened) != 1 {
		t.Errorf("opened %d threads, want 1 for a root command", len(opened))
	}

	// A root command has no thread of its own, so one is opened and bound to the
	// adopted session.
	row, found, err := b.store.Thread(context.Background(), "T1:C1:900.000001")
	if err != nil || !found {
		t.Fatalf("the adopted thread was not recorded: found=%v err=%v", found, err)
	}
	if row.SessionPath != "/sessions/theirs.jsonl" || row.SessionName != "their-session" {
		t.Errorf("the row does not carry the adopted session: %+v", row)
	}
	// An adopted session's directory is not pi-chat's to delete.
	if row.ProjectDir != "" {
		t.Errorf("an adopted session got a project directory: %q", row.ProjectDir)
	}
}

func TestResumeSessionRefusesAValueThatIsNotInTheCatalog(t *testing.T) {
	// The button's value travelled through the platform, so it is input by the
	// time it comes back.
	b, platform, _ := newTestBot(t)
	b.gw = &fakeGateway{}

	b.HandleAction(context.Background(), Action{
		EventID: "Ev-action", Channel: "C1", UserID: "U1", Workspace: "T1",
		ActionID: ActionResume, Value: "/sessions/forged.jsonl",
	})

	waitFor(t, "the answer", func() bool { return len(platform.postedNotices()) == 1 })
	if opened := platform.openedThreads(); len(opened) != 0 {
		t.Errorf("opened a thread for a session that is not in the catalog: %v", opened)
	}
	if text := platform.postedNotices()[0].Text; !strings.Contains(text, "gone") {
		t.Errorf("the answer does not explain the refusal: %q", text)
	}
}

func TestDispatchAnswersReservedCommandsInsteadOfForwardingThem(t *testing.T) {
	// The grammar reserves more commands than this build implements, and
	// forwarding one of those would send its literal text to the agent: a clear
	// "not yet" is better. `/delete` used to be answered here and is not any more
	// — it deletes.
	for _, name := range []string{"/abort", "/model", "/stop"} {
		t.Run(name, func(t *testing.T) {
			b, platform, _ := newTestBot(t)

			b.HandleCommand(context.Background(), commandRequest("1700000000.000100", name))

			waitFor(t, "the answer", func() bool { return len(platform.postedNotices()) == 1 })
			if platform.startedTurns() != 0 {
				t.Fatal("a reserved command reached the agent")
			}
			if text := platform.postedNotices()[0].Text; !strings.Contains(text, "not in this build") {
				t.Errorf("the answer does not explain itself: %q", text)
			}
		})
	}
}

func TestDispatchExplainsWrongPlaces(t *testing.T) {
	t.Run("thread command at the root", func(t *testing.T) {
		b, platform, _ := newTestBot(t)
		b.HandleCommand(context.Background(), commandRequest("", "/delete"))

		waitFor(t, "the hint", func() bool { return len(platform.postedNotices()) == 1 })
		if text := platform.postedNotices()[0].Text; !strings.Contains(text, "@pi /delete") {
			t.Errorf("the hint does not point at the thread form: %q", text)
		}
	})

	t.Run("resume in a thread", func(t *testing.T) {
		b, platform, _ := newTestBot(t)
		b.HandleCommand(context.Background(), commandRequest("1700000000.000100", "/resume"))

		waitFor(t, "the answer", func() bool { return len(platform.postedNotices()) == 1 })
		if text := platform.postedNotices()[0].Text; !strings.Contains(text, "no sessions retired") {
			t.Errorf("the thread picker did not explain the empty result: %q", text)
		}
	})
}

func TestDispatchForwardsUnknownCommandsToTheAgent(t *testing.T) {
	// pi's own vocabulary — skills, templates, extensions — is pi's to resolve,
	// and its error is more useful than ours (DESIGN.md §5, rule 2).
	b, platform, _ := newTestBot(t)

	b.HandleCommand(context.Background(), commandRequest("1700000000.000100", "/skill:grill-me be harsh"))

	waitFor(t, "the turn", func() bool { return platform.startedTurns() == 1 })
	if got := platform.turns[0].Text; got != "/skill:grill-me be harsh" {
		t.Errorf("the agent received %q, want the command verbatim", got)
	}
}

func TestDispatchAsksARootUnknownCommandForAThread(t *testing.T) {
	b, platform, _ := newTestBot(t)

	b.HandleCommand(context.Background(), commandRequest("", "/compact"))

	waitFor(t, "the answer", func() bool { return len(platform.postedNotices()) == 1 })
	if platform.startedTurns() != 0 {
		t.Fatal("a root command started a session")
	}
	if text := platform.postedNotices()[0].Text; !strings.Contains(text, "@pi /") {
		t.Errorf("the hint does not point at the thread form: %q", text)
	}
}

func TestDispatchBareSlashShowsHelp(t *testing.T) {
	b, platform, _ := newTestBot(t)

	b.HandleCommand(context.Background(), commandRequest("", "/"))

	waitFor(t, "the help", func() bool { return len(platform.postedNotices()) == 1 })
	if text := platform.postedNotices()[0].Text; !strings.Contains(text, "pi-chat") {
		t.Errorf("the answer is not the help text: %q", text)
	}
}

func TestDispatchRefusesOutsideTheAllowlist(t *testing.T) {
	b, platform, _ := newTestBot(t)
	command := commandRequest("", "/status")
	command.UserID = "U2"

	b.HandleCommand(context.Background(), command)

	waitFor(t, "the refusal", func() bool { return len(platform.postedNotices()) == 1 })
	if !platform.postedNotices()[0].Ephemeral {
		t.Error("a refusal should be visible only to the sender")
	}
}

func TestPiArgsAddsApproveWhenApprovalsAreAutomatic(t *testing.T) {
	// The live config already passes --approve by hand; the setting must not add
	// a second one, and the injected prompt must follow it.
	b, _, _ := newTestBotWith(t, func(cfg *config.Config) {
		cfg.Gateway.PiArgs = []string{"--approve"}
		cfg.Behavior.InjectedPrompt = "work in {projectsRoot} as {slug}"
	})

	project := workspaceProject(t, b, "fix the tests")
	args := b.piArgs(project)
	approvals := 0
	for _, arg := range args {
		if arg == "--approve" {
			approvals++
		}
	}
	if approvals != 1 {
		t.Errorf("--approve appears %d times in %v", approvals, args)
	}
	if !slices.Contains(args, "--append-system-prompt") {
		t.Errorf("the injected prompt is missing from %v", args)
	}
}

func TestPiArgsAddsChatToolsExtensionOnlyWhenEnabled(t *testing.T) {
	path := "/opt/pi-chat/chat-tools.ts"
	b, _, _ := newTestBotWith(t, func(cfg *config.Config) {
		cfg.ChatTools.Enabled = true
		cfg.ChatTools.ExtensionPath = path
		cfg.Gateway.PiArgs = []string{"--extension", path}
	})
	args := b.piArgs(workspaceProject(t, b, "read the chat"))
	if got := countFlag(args, "--extension"); got != 1 {
		t.Errorf("chat-tools extension appears %d times in %v, want once", got, args)
	}
	if !slices.Contains(args, path) {
		t.Errorf("extension path %q is missing from %v", path, args)
	}

	b.cfg.ChatTools.Enabled = false
	b.cfg.Gateway.PiArgs = nil
	args = b.piArgs(workspaceProject(t, b, "read without tools"))
	if slices.Contains(args, "--extension") {
		t.Errorf("disabled chat tools added an extension: %v", args)
	}
}

func TestPiArgsWithoutApprovals(t *testing.T) {
	b, _, _ := newTestBot(t)
	b.cfg.Behavior.Approvals = "interactive"
	b.cfg.Gateway.PiArgs = nil
	b.cfg.Behavior.InjectedPrompt = ""

	project := workspaceProject(t, b, "fix the tests")
	args := b.piArgs(project)
	if slices.Contains(args, "--approve") {
		t.Errorf("approvals are interactive, so --approve should not be there: %v", args)
	}
	// The instruction that explains a Slack thread is not configuration: every
	// session pi-chat creates carries it, so the arguments are never empty even
	// when nothing else is set.
	if n := countFlag(args, "--append-system-prompt"); n != 1 {
		t.Errorf("--append-system-prompt appears %d times in %v, want once", n, args)
	}
	if value := flagValue(t, args, "--append-system-prompt"); !strings.Contains(value, instructionMarker) {
		t.Errorf("the instruction is missing from %v", args)
	}
	if len(args) != 2 {
		t.Errorf("piArgs = %v, want just the instruction", args)
	}
}

func TestHandleActionRefusesOutsideTheAllowlist(t *testing.T) {
	b, platform, _ := newTestBot(t)

	b.HandleAction(context.Background(), Action{
		EventID: "Ev1", Channel: "C1", UserID: "U2", Workspace: "T1",
		ActionID: ActionResume, Value: "/sessions/a.jsonl",
	})

	waitFor(t, "the refusal", func() bool { return len(platform.postedNotices()) == 1 })
	if !platform.postedNotices()[0].Ephemeral {
		t.Error("a refusal should be visible only to the sender")
	}
	if opened := platform.openedThreads(); len(opened) != 0 {
		t.Errorf("a refused button opened a thread: %v", opened)
	}
}

func TestHandleActionIgnoresAnUnknownButton(t *testing.T) {
	b, platform, _ := newTestBot(t)

	b.HandleAction(context.Background(), Action{
		EventID: "Ev1", Channel: "C1", UserID: "U1", Workspace: "T1",
		ActionID: "not-a-button-this-build-knows", Value: "x",
	})

	// Nothing to send, nothing to open: an unknown button is old furniture from
	// a previous build, and answering it would be noise.
	time.Sleep(20 * time.Millisecond)
	if n := len(platform.postedNotices()); n != 0 {
		t.Errorf("posted %d notices for an unknown button", n)
	}
	if opened := platform.openedThreads(); len(opened) != 0 {
		t.Errorf("an unknown button opened a thread: %v", opened)
	}
}

func TestHumanSince(t *testing.T) {
	now := time.Now()
	for _, test := range []struct {
		when time.Time
		want string
	}{
		{now, "just now"},
		{now.Add(-5 * time.Minute), "5m ago"},
		{now.Add(-3 * time.Hour), "3h ago"},
		{now.Add(-49 * time.Hour), "2d ago"},
	} {
		if got := humanSince(test.when); got != test.want {
			t.Errorf("humanSince(%v) = %q, want %q", test.when, got, test.want)
		}
	}
}

func TestTruncateText(t *testing.T) {
	if got := truncateText("short", 10); got != "short" {
		t.Errorf("truncateText = %q, want it unchanged", got)
	}
	if got := truncateText("abcdefghij", 5); got != "abcd…" {
		t.Errorf("truncateText = %q, want it cut and marked", got)
	}
	// Runes, not bytes: a button label must not be cut mid-character.
	if got := truncateText("æøåü", 3); got != "æø…" {
		t.Errorf("truncateText = %q, want rune-safe cutting", got)
	}
}
