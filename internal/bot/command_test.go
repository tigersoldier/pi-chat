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
		{"resume", true, false},
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
	c := Command{EventID: "Ev-" + text, Channel: "C1", UserID: "U1", TeamID: "T1", Text: text}
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
	b.gw = &fakeGateway{rows: []gwclient.SessionRow{
		{Path: "/sessions/ours.jsonl", Cwd: "/somewhere"},
		{Path: "/sessions/made-by-pi-chat.jsonl", Cwd: b.cfg.Paths.ProjectsRoot + "/2026-09-27-x"},
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
		EventID: "Ev-action", Channel: "C1", UserID: "U1", TeamID: "T1",
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
		EventID: "Ev-action", Channel: "C1", UserID: "U1", TeamID: "T1",
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
	// `/delete` arrives before its phase. Forwarding it would send the literal
	// text to the agent, which is worse than saying so.
	b, platform, _ := newTestBot(t)

	b.HandleCommand(context.Background(), commandRequest("1700000000.000100", "/delete"))

	waitFor(t, "the answer", func() bool { return len(platform.postedNotices()) == 1 })
	if platform.startedTurns() != 0 {
		t.Fatal("a reserved command reached the agent")
	}
	if text := platform.postedNotices()[0].Text; !strings.Contains(text, "not in this build") {
		t.Errorf("the answer does not explain itself: %q", text)
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

	t.Run("root command in a thread", func(t *testing.T) {
		b, platform, _ := newTestBot(t)
		b.HandleCommand(context.Background(), commandRequest("1700000000.000100", "/resume"))

		waitFor(t, "the hint", func() bool { return len(platform.postedNotices()) == 1 })
		if text := platform.postedNotices()[0].Text; !strings.Contains(text, "/pi resume") {
			t.Errorf("the hint does not point at the root form: %q", text)
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

func TestPiArgsWithoutApprovals(t *testing.T) {
	b, _, _ := newTestBot(t)
	b.cfg.Behavior.Approvals = "interactive"
	b.cfg.Gateway.PiArgs = nil
	b.cfg.Behavior.InjectedPrompt = ""

	project := workspaceProject(t, b, "fix the tests")
	if args := b.piArgs(project); len(args) != 0 {
		t.Errorf("piArgs = %v, want none", args)
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
