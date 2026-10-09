package bot

import (
	"context"
	"testing"
)

// unaddressed builds a plain message in the test thread: something somebody said
// without addressing the bot.
func unaddressed(eventID, text string) Message {
	m := newTestMessage(eventID, text)
	m.Mentioned = false
	return m
}

func TestAnUnaddressedReplyInAThreadWithASessionIsSilent(t *testing.T) {
	b, platform, st := newTestBot(t)
	threadWithSession(t, b, st, "C1", "1700000000.000100")
	message := unaddressed("Ev-unaddressed", "thanks, that makes sense")

	b.HandleMessage(context.Background(), message)

	if notices := platform.postedNotices(); len(notices) != 0 {
		t.Fatalf("posted %+v, want silence for an unaddressed message", notices)
	}
	if platform.startedTurns() != 0 {
		t.Error("an unaddressed message spent a turn")
	}
	if acks := platform.acknowledgedRequests(); len(acks) != 0 {
		t.Errorf("acknowledged %+v, want no reaction for an unaddressed message", acks)
	}
	if claimed, err := st.ClaimEvent(context.Background(), messageClaim(message)); err != nil || !claimed {
		t.Errorf("unaddressed message was claimed (claimed=%v, err=%v)", claimed, err)
	}
}

func TestAnUnaddressedReplyInAThreadWithNoSessionSaysNothing(t *testing.T) {
	b, platform, st := newTestBot(t)

	b.HandleMessage(context.Background(), unaddressed("Ev-unaddressed", "hi all"))

	if notices := platform.postedNotices(); len(notices) != 0 {
		t.Errorf("a notice was posted for an unaddressed message: %+v", notices)
	}
	if _, found, err := st.Thread(context.Background(), "T1:C1:1700000000.000100"); err != nil || found {
		t.Errorf("an unaddressed message wrote a thread row (found=%v, err=%v)", found, err)
	}
}
