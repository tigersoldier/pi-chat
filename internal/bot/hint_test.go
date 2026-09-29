package bot

import (
	"context"
	"strings"
	"testing"
)

// unaddressed builds a plain message in the test thread: something somebody said
// without addressing the bot.
func unaddressed(eventID, text string) Message {
	m := newTestMessage(eventID, text)
	m.Mentioned = false
	return m
}

func TestAnUnaddressedReplyInAThreadWithASessionGetsOneHint(t *testing.T) {
	// In a channel only a mention is a request (DESIGN.md §5), and nothing outside
	// says so: the bot may be the one who opened the thread — `/pi resume` does —
	// which makes typing in it look exactly like the way to continue. So the person
	// who typed is told, privately, and without spending a turn.
	b, platform, st := newTestBot(t)
	threadWithSession(t, b, st, "C1", "1700000000.000100")

	b.HandleMessage(context.Background(), unaddressed("Ev-unaddressed", "thanks, that makes sense"))

	if n := len(platform.postedNotices()); n != 1 {
		t.Fatalf("posted %d notices, want one hint: %+v", n, platform.postedNotices())
	}
	notice := platform.postedNotices()[0]
	if !notice.Ephemeral || notice.UserID != "U1" {
		t.Errorf("the hint is not a private note to the sender: %+v", notice)
	}
	if notice.Thread == nil || notice.Thread.ThreadTS != "1700000000.000100" {
		t.Errorf("the hint was not posted in the thread: %+v", notice.Thread)
	}
	if !strings.Contains(notice.Text, "@pi") {
		t.Errorf("the hint does not say what to type: %q", notice.Text)
	}
	if platform.startedTurns() != 0 {
		t.Error("an unaddressed message spent a turn")
	}

	// Once is enough. A side conversation running alongside the session gets one
	// answer and then silence, because the note is aimed at somebody who does not
	// know the rule, not at every sentence they type.
	b.HandleMessage(context.Background(), unaddressed("Ev-unaddressed-2", "yes, agreed"))
	if n := len(platform.postedNotices()); n != 1 {
		t.Errorf("the hint was repeated: %+v", platform.postedNotices())
	}
	if platform.startedTurns() != 0 {
		t.Error("an unaddressed message spent a turn")
	}
}

func TestAnUnaddressedReplyInAThreadWithNoSessionSaysNothing(t *testing.T) {
	// Nobody has ever been answered in this thread, so there is nothing to explain
	// — and an unaddressed message must not register a row for a thread pi-chat has
	// never worked in.
	b, platform, st := newTestBot(t)

	b.HandleMessage(context.Background(), unaddressed("Ev-unaddressed", "hi all"))

	if n := len(platform.postedNotices()); n != 0 {
		t.Errorf("a hint was posted for a thread with no session: %+v", platform.postedNotices())
	}
	if _, found, err := st.Thread(context.Background(), "T1:C1:1700000000.000100"); err != nil || found {
		t.Errorf("an unaddressed message wrote a thread row (found=%v, err=%v)", found, err)
	}
}
