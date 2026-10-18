package bot

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/tigersoldier/pi-gateway/gwclient"

	"github.com/tigersoldier/pi-chat/internal/config"
	"github.com/tigersoldier/pi-chat/internal/store"
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

func TestResumeNoticeExplainsWhenChatToolsAreUnavailable(t *testing.T) {
	b, _, st := newTestBotWith(t, func(cfg *config.Config) { cfg.ChatTools.Enabled = true })
	defer st.Close()
	ctx := context.Background()
	thread := Thread{Workspace: "T1", Channel: "C1", ThreadTS: "1700000000.000100"}
	row := gwclient.SessionRow{Name: "external-session", Path: "/sessions/external.jsonl", Cwd: "/home/pi/code/thing"}

	notice := b.resumeNoticeForSession(ctx, thread, row, false)
	if !strings.Contains(notice.Text, "Chat tools are not available") || !strings.Contains(notice.Text, "@pi /new") {
		t.Fatalf("an adopted session without a binding was not explained: %q", notice.Text)
	}

	if err := st.BindSessionToolBinding(ctx, store.SessionToolBinding{
		SessionPath: row.Path, Provider: "slack", Surface: SurfaceApp,
		WorkspaceID: thread.Workspace, ChannelID: thread.Channel, ThreadTS: thread.ThreadTS,
		Capabilities: []string{"read_thread"}, ExtensionVersion: "v1", State: store.ToolBindingActive,
	}, false); err != nil {
		t.Fatalf("bind the session for the active-binding case: %v", err)
	}
	active := b.resumeNoticeForSession(ctx, thread, row, false)
	if strings.Contains(active.Text, "Chat tools are not available") {
		t.Fatalf("an active session binding was reported unavailable: %q", active.Text)
	}
}

func TestResumeOpeningMessageUsesTheHumanCandidateLabelAndLastResponse(t *testing.T) {
	row := gwclient.SessionRow{
		Name: "opaque-session-name", Title: "What broke in the test?",
		ID:   "550e8400-e29b-41d4-a716-446655440000",
		Path: "/sessions/550e8400-e29b-41d4-a716-446655440000.jsonl",
		Cwd:  "/home/pi/code/thing",
	}
	response := "The test failed because the resumed session did not load its state."

	got := resumeOpeningMessage(row, response)
	want := "Resuming from session " + sessionLabel(row) + ".\n" +
		"Here is the last agent response in that session:\n\n" + response
	if got != want {
		t.Fatalf("resume opening = %q, want %q", got, want)
	}
	for _, unwanted := range []string{row.ID, row.Path, filepath.Base(row.Path), row.Name, row.Cwd} {
		if strings.Contains(got, unwanted) {
			t.Errorf("resume opening leaked %q: %q", unwanted, got)
		}
	}
}

func TestResumeOpeningMessagePreservesMrkdwnAsPlainTextContent(t *testing.T) {
	row := gwclient.SessionRow{Title: "Please use <!channel> *bold*", Cwd: "/work/repo"}
	response := "Keep <!channel>, *bold*, `code`, and <https://example.test|link> as written."
	got := resumeOpeningMessage(row, response)
	for _, dynamic := range []string{row.Title, response} {
		if !strings.Contains(got, dynamic) {
			t.Errorf("opening changed dynamic plain text %q: %q", dynamic, got)
		}
	}
}

func TestResumeOpeningMessageOmitsResponsesWithSessionIdentifiers(t *testing.T) {
	row := gwclient.SessionRow{
		Title: "Fix the queue", ID: "opaque-session-id",
		Path: "/sessions/opaque-session-id.jsonl", Cwd: "/work/queue",
	}
	for _, response := range []string{
		"The session id is `opaque-session-id`.",
		"The session id is opaque-*session*-id.",
		"The source file is /sessions/opaque-session-id.jsonl.",
		"The source file is /sessions/opaque-*session*-id.jsonl.",
		"The agent found 550e8400-e29b-41d4-a716-446655440000 in the log.",
		"The agent referenced another transcript.jsonl file.",
	} {
		t.Run(response, func(t *testing.T) {
			got := resumeOpeningMessage(row, response)
			if !strings.Contains(got, resumeResponseOmitted) {
				t.Fatalf("unsafe response was not explained: %q", got)
			}
			if strings.Contains(got, response) {
				t.Errorf("unsafe response was included: %q", got)
			}
			for _, unwanted := range []string{row.ID, row.Path, filepath.Base(row.Path), ".jsonl", "550e8400-e29b-41d4-a716-446655440000"} {
				if strings.Contains(got, unwanted) {
					t.Errorf("opening leaked %q: %q", unwanted, got)
				}
			}
		})
	}
}

func TestResumeOpeningMessageBoundsOversizedResponseWithoutSplittingUTF8(t *testing.T) {
	row := gwclient.SessionRow{Title: "Fix the queue", Cwd: "/work/queue"}
	response := strings.Repeat("🧪", 12000)
	got := resumeOpeningMessage(row, response)
	if len(got) > resumeOpeningMaxBytes {
		t.Fatalf("resume opening is %d bytes, max %d", len(got), resumeOpeningMaxBytes)
	}
	if !utf8.ValidString(got) {
		t.Fatal("resume opening is not valid UTF-8 after truncation")
	}
	if !strings.HasSuffix(got, "…") {
		t.Fatalf("oversized response was not marked as truncated: %q", got[len(got)-12:])
	}
	if !strings.Contains(got, "Here is the last agent response in that session:\n\n") {
		t.Fatalf("opening lost its response introduction: %q", got[:100])
	}
}

func TestResumeOpeningMessageDegradesWithoutHumanLabelOrResponse(t *testing.T) {
	row := gwclient.SessionRow{
		ID:   "550e8400-e29b-41d4-a716-446655440000",
		Path: "/sessions/550e8400-e29b-41d4-a716-446655440000.jsonl",
	}
	got := resumeOpeningMessage(row, " \n ")
	if got != "Resuming a session." {
		t.Fatalf("resume opening = %q, want a generic opening without a response", got)
	}
	for _, unwanted := range []string{row.ID, row.Path, filepath.Base(row.Path), ".jsonl"} {
		if strings.Contains(got, unwanted) {
			t.Errorf("generic resume opening leaked %q: %q", unwanted, got)
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

	notice := resumeNotice(thread, row, false)
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
	if !strings.Contains(notice.Text, "mentioning me") {
		t.Errorf("the notice does not say how to continue in a channel thread: %q", notice.Text)
	}

	// The same notice in a DM says the opposite thing, because it is the opposite
	// rule: asking somebody in a DM to mention the bot would be teaching them a
	// habit that does nothing.
	dm := resumeNotice(thread, row, true)
	if strings.Contains(dm.Text, "mentioning me") {
		t.Errorf("a direct message is told to mention the bot: %q", dm.Text)
	}
	if !strings.Contains(dm.Text, "just type") {
		t.Errorf("the DM notice does not say what to do: %q", dm.Text)
	}
}
