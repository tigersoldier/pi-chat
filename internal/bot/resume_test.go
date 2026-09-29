package bot

import (
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/tigersoldier/pi-gateway/gwclient"
)

func TestSessionLabelLeadsWithWhatTheSessionIsAbout(t *testing.T) {
	// A picker's button is the only thing a person picks from, so it says what the
	// session was about and which directory it works in — never a filename, and
	// never a machine name that only encodes the thread.
	row := gwclient.SessionRow{
		Name:  "slack-t6k8y3frr-c0c4tmqnr8r-1790529157-569279",
		Title: "can you see my question above?",
		Cwd:   "/home/pi/work/2026-09-27-can-you-see-my-question-above",
		Path:  "/home/pi/.pi/agent/sessions/--home-pi-work-2026-09-27--/2026-09-27T18-09-03-996Z_01a0e40e.jsonl",
	}

	label := sessionLabel(row)
	if !strings.HasPrefix(label, "can you see my question above?") {
		t.Errorf("the label does not lead with the session's own words: %q", label)
	}
	if !strings.Contains(label, "2026-09-27-can-you-see") {
		t.Errorf("the label does not say where the session works: %q", label)
	}
	for _, unwanted := range []string{"/home/pi", "slack-t6k8y3frr", ".jsonl"} {
		if strings.Contains(label, unwanted) {
			t.Errorf("the label leaks %q: %q", unwanted, label)
		}
	}
	if n := utf8.RuneCountInString(label); n > 70 {
		t.Errorf("the label is %d runes, over the platform's cap: %q", n, label)
	}
}

func TestSessionLabelCutsTheTitleNotTheDirectory(t *testing.T) {
	// Title and directory compete for the same cap. Cutting the whole label would
	// eat the directory, which is the half that tells two sessions of one
	// repository apart.
	row := gwclient.SessionRow{
		Title: strings.Repeat("a long first prompt ", 20),
		Cwd:   "/home/pi/code/pi-gchat",
		Path:  "/sessions/a.jsonl",
	}

	label := sessionLabel(row)
	if !strings.HasSuffix(label, " · pi-gchat") {
		t.Errorf("the directory did not survive the cap: %q", label)
	}
	if !strings.Contains(label, "…") {
		t.Errorf("the title was not marked as cut: %q", label)
	}
	if n := utf8.RuneCountInString(label); n > 70 {
		t.Errorf("the label is %d runes, over the platform's cap: %q", n, label)
	}
}

func TestSessionLabelFallsBackThroughNameToFile(t *testing.T) {
	named := gwclient.SessionRow{Name: "nightly-build", Cwd: "/home/pi/code/thing", Path: "/sessions/2026-01-01T00-00-00-000Z_abc.jsonl"}
	if got := sessionLabel(named); !strings.HasPrefix(got, "nightly-build") {
		t.Errorf("a named session was labelled %q, want its name", got)
	}
	// Nothing to describe it by: the file is still better than an empty button.
	bare := gwclient.SessionRow{Path: "/sessions/2026-01-01T00-00-00-000Z_abc.jsonl"}
	if got := sessionLabel(bare); !strings.HasPrefix(got, "2026-01-01T00-00-00-000Z_abc.jsonl") {
		t.Errorf("a session with nothing else was labelled %q, want its file", got)
	}
}

func TestSessionLabelSaysWhenASessionIsLive(t *testing.T) {
	// A live session already has a pi process — usually somebody's terminal — and
	// adopting it into Slack makes it a shared session. That is worth knowing
	// before the button is pressed, not after.
	row := gwclient.SessionRow{Title: "hello", Cwd: "/home/pi/code/thing", Live: true, Path: "/sessions/a.jsonl"}
	if got := sessionLabel(row); !strings.HasSuffix(got, "· live") {
		t.Errorf("a live session was labelled %q, want it marked", got)
	}
}

func TestFirstSentenceSkipsTheWrapperASkillArrivesIn(t *testing.T) {
	// pi wraps a skill or template invocation in its own element, followed by
	// boilerplate. A session that began with `/grill-me` should be labelled by the
	// question somebody asked, not by the skill that ran.
	title := "<skill name=\"grill-me\" location=\"/home/pi/.pi/agent/skills/grill-me/SKILL.md\">\n" +
		"References are relative to /home/pi/.pi/agent/skills/grill-me.\n\n" +
		"Call the Skill tool with \"grilling\".\n</skill>\n\n" +
		"I want to change the scope and foundation of this project.\n\n" +
		"This project will no longer be gchat only."
	want := "I want to change the scope and foundation of this project."
	if got := firstSentence(title); got != want {
		t.Errorf("firstSentence = %q, want %q", got, want)
	}

	for _, tc := range []struct{ in, want string }{
		{"fix the flaky test\n\nit is the retry logic", "fix the flaky test"},
		{"  \n\n  hello there  \n", "hello there"},
		{"a very long prompt " + strings.Repeat("x", 400), "a very long prompt " + strings.Repeat("x", 400)},
		{"   \n\n", ""},
		{"", ""},
		// An unclosed wrapper is not a wrapper: the text is used as it is rather
		// than thrown away.
		{"<skill name=\"x\">\nno closing tag", "<skill name=\"x\">"},
	} {
		if got := firstSentence(tc.in); got != tc.want {
			t.Errorf("firstSentence(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestResumeNoticeCarriesItsChannelAndSaysHowToContinue(t *testing.T) {
	// Two things this notice has to get right, both learned from the first live
	// adoption:
	//
	//   - it names its channel. A notice with a thread but no channel has nowhere to
	//     go, and the adapter answered channel_not_found — so the adoption, which had
	//     in fact succeeded, looked like it had not happened at all.
	//   - it says how to continue. An adopted session lives in a *channel* thread,
	//     where the grammar answers mentions only, so "keep going here" leaves
	//     somebody typing at a bot that is listening for something else.
	//
	// The attach step itself cannot run in a unit test (it dials a real daemon), so
	// what the notice is made of is checked here rather than through the flow.
	thread := Thread{Workspace: "T1", Channel: "C1", ThreadTS: "1700000000.000100"}
	row := gwclient.SessionRow{Name: "their-session", Cwd: "/home/pi/code/thing"}

	notice := resumeNotice(thread, row)
	if notice.Thread == nil || *notice.Thread != thread {
		t.Errorf("the notice is not in the adopted thread: %#v", notice.Thread)
	}
	if notice.Channel != thread.Channel {
		t.Errorf("channel = %q, want %q so the adapter has somewhere to post it",
			notice.Channel, thread.Channel)
	}
	if !strings.Contains(notice.Text, "their-session") || !strings.Contains(notice.Text, "/home/pi/code/thing") {
		t.Errorf("the notice does not say what was adopted or where it works: %q", notice.Text)
	}
	if !strings.Contains(notice.Text, "mention me") {
		t.Errorf("the notice does not say how to continue in a channel thread: %q", notice.Text)
	}
}
