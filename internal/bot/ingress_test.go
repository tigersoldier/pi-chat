package bot

import (
	"context"
	"slices"
	"testing"
	"time"
)

// slackMention is one Slack mention as Slack reports it: twice, once as
// `app_mention` and once as the `message.channels` event for the same message,
// with two different event ids. The order the two arrive in is not fixed, so
// neither is the order a caller may assume.
func slackMention(threadTS, ts, text string) (mention, fromMessage Message) {
	base := Message{
		Thread:    Thread{Workspace: "T1", Channel: "C1", ThreadTS: threadTS},
		UserID:    "U1",
		Workspace: "T1",
		TS:        ts,
		Text:      text,
		Mentioned: true,
	}
	mention = base
	mention.EventID = "EvAppMention-" + ts
	fromMessage = base
	fromMessage.EventID = "EvMessage-" + ts
	return mention, fromMessage
}

// One message is one turn, however many events carried it. Claiming by event id
// spent two turns — and posted two answers — on a single request, which is what
// this pins.
func TestOneMessageDeliveredTwiceSpendsOneTurn(t *testing.T) {
	for _, tc := range []struct {
		name     string
		threadTS string
		ts       string
		direct   bool
	}{
		{name: "a mention inside a thread", threadTS: "1700000000.000100", ts: "1700000000.000200"},
		{name: "a mention at the root of a channel", threadTS: "1700000000.000200", ts: "1700000000.000200"},
		{name: "a message in a one-to-one DM", threadTS: "1700000000.000200", ts: "1700000000.000200", direct: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b, platform, _ := newTestBot(t)
			first, second := slackMention(tc.threadTS, tc.ts, "what changed?")
			first.Direct, second.Direct = tc.direct, tc.direct

			b.HandleMessage(context.Background(), first)
			b.HandleMessage(context.Background(), second)

			waitFor(t, "the turn", func() bool { return platform.startedTurns() == 1 })
			// The second delivery is dropped synchronously, so a moment of
			// patience is what turns "no second turn yet" into "no second turn".
			time.Sleep(20 * time.Millisecond)
			turns := platform.postedTurns()
			if len(turns) != 1 {
				t.Fatalf("started %d turns for one message, want 1", len(turns))
			}
			if turns[0].Text != "what changed?" {
				t.Errorf("prompt = %q, want the message's own text", turns[0].Text)
			}
		})
	}
}

// The two deliveries of a DM message disagree about *why* they address the bot —
// one is plain text in a direct conversation, the other a mention — and they are
// still one request. The claim is the message's identity, not the reason it was
// routed.
func TestTwinDeliveriesThatDisagreeAboutBeingAddressedSpendOneTurn(t *testing.T) {
	b, platform, _ := newTestBot(t)
	plain, mention := slackMention("1700000000.000200", "1700000000.000200", "hello")
	plain.Mentioned, plain.Direct = false, true
	plain.EventID = "EvMessage"
	mention.Direct = true

	b.HandleMessage(context.Background(), plain)
	b.HandleMessage(context.Background(), mention)

	waitFor(t, "the turn", func() bool { return platform.startedTurns() == 1 })
	time.Sleep(20 * time.Millisecond)
	if got := platform.startedTurns(); got != 1 {
		t.Fatalf("started %d turns, want 1", got)
	}
}

// And two messages are two turns: the claim is the message's identity, not the
// thread's, so a conversation is not deduped into one turn per thread.
func TestTwoMessagesInOneThreadAreTwoTurns(t *testing.T) {
	b, platform, _ := newTestBot(t)
	first, _ := slackMention("1700000000.000100", "1700000000.000200", "one")
	_, second := slackMention("1700000000.000100", "1700000000.000300", "two")

	b.HandleMessage(context.Background(), first)
	b.HandleMessage(context.Background(), second)

	waitFor(t, "both turns", func() bool { return platform.startedTurns() == 2 })
	turns := platform.postedTurns()
	if len(turns) != 2 {
		t.Fatalf("turns = %+v, want one per message", turns)
	}
	// Each turn is started on its own goroutine, so what is asserted is that both
	// messages were turned into turns, not the order the platform happened to see
	// them in.
	texts := []string{turns[0].Text, turns[1].Text}
	slices.Sort(texts)
	if !slices.Equal(texts, []string{"one", "two"}) {
		t.Fatalf("prompts = %v, want one per message", texts)
	}
}

// A message nobody addressed leaves no trace at all: no row, no notice, and —
// because it is not work — no dedupe record either. That is what keeps the table
// about requests while the bot sits in busy rooms (DESIGN.md §5, §7).
func TestOnlyAddressedMessagesAreClaimed(t *testing.T) {
	b, platform, st := newTestBot(t)
	ctx := context.Background()

	unaddressed := newTestMessage("Ev1", "just talking to a colleague")
	unaddressed.Mentioned = false
	b.HandleMessage(ctx, unaddressed)

	if claimed, err := st.ClaimEvent(ctx, messageClaim(unaddressed)); err != nil {
		t.Fatalf("ClaimEvent: %v", err)
	} else if !claimed {
		t.Error("an unaddressed message was recorded, so ambient chatter would fill the dedupe table")
	}
	if platform.startedTurns() != 0 || len(platform.postedNotices()) != 0 {
		t.Fatalf("an unaddressed message did something: turns=%d notices=%d",
			platform.startedTurns(), len(platform.postedNotices()))
	}

	// The claim still happens for a message that is a request, and it happens
	// before the turn, so a redelivery cannot run it twice.
	addressed := newTestMessage("Ev2", "what changed?")
	b.HandleMessage(ctx, addressed)
	if claimed, err := st.ClaimEvent(ctx, messageClaim(addressed)); err != nil {
		t.Fatalf("ClaimEvent: %v", err)
	} else if claimed {
		t.Error("an answered message was not claimed")
	}
}

func TestTaggedMessageIsAcknowledgedBeforeAuthorizationOutcome(t *testing.T) {
	for _, tc := range []struct {
		name    string
		userID  string
		allowed bool
	}{
		{name: "allowlisted", userID: "U1", allowed: true},
		{name: "not allowlisted", userID: "U2"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b, platform, _ := newTestBot(t)
			message := newTestMessage("Ev1", "please help")
			message.UserID, message.TS = tc.userID, "1700000000.000200"

			b.HandleMessage(context.Background(), message)

			acks := platform.acknowledgedRequests()
			if len(acks) != 1 || acks[0].allowed != tc.allowed || acks[0].message.TS != message.TS {
				t.Fatalf("acknowledgements = %+v, want one for the tagged message with allowed=%v", acks, tc.allowed)
			}
			if tc.allowed {
				waitFor(t, "the turn", func() bool { return platform.startedTurns() == 1 })
				if len(platform.postedNotices()) != 0 {
					t.Errorf("allowlisted request produced a refusal: %+v", platform.postedNotices())
				}
				return
			}

			if platform.startedTurns() != 0 {
				t.Error("a request from outside the allowlist started a turn")
			}
			notices := platform.postedNotices()
			if len(notices) != 1 || notices[0].Text != deniedUserText || !notices[0].Ephemeral || notices[0].UserID != tc.userID {
				t.Errorf("notices = %+v, want a private refusal to the sender", notices)
			}
		})
	}
}

// A reply nobody addressed to the bot is not a request, so there is nothing to
// refuse — and an unlisted person answering their colleague in a thread the bot
// happens to be in must not draw a visible reply about somebody's allowlist.
func TestAStrangersReplyIsNotRefused(t *testing.T) {
	b, platform, st := newTestBot(t)
	message := newTestMessage("Ev1", "thanks, I will take a look")
	message.Mentioned = false
	message.UserID = "U2" // not on the allowlist

	b.HandleMessage(context.Background(), message)

	if notices := platform.postedNotices(); len(notices) != 0 {
		t.Fatalf("posted %+v, want silence for a message that was not a request", notices)
	}
	if got := platform.startedTurns(); got != 0 {
		t.Errorf("started %d turns for a stranger's reply", got)
	}
	// The drop happens before anything is recorded, so the thread is not even
	// registered by it.
	if rows, err := st.Threads(context.Background()); err != nil {
		t.Fatalf("list threads: %v", err)
	} else if len(rows) != 0 {
		t.Errorf("recorded %d threads for a dropped reply", len(rows))
	}
}

// A DM is addressed to the bot by construction, so an unlisted person messaging
// it privately *is* told why nothing happened.
func TestAStrangersDMIsRefused(t *testing.T) {
	b, platform, _ := newTestBot(t)
	message := newTestMessage("Ev1", "hello")
	message.Mentioned, message.Direct, message.UserID = false, true, "U2"

	b.HandleMessage(context.Background(), message)

	notices := platform.postedNotices()
	if len(notices) != 1 || notices[0].Text != deniedUserText {
		t.Fatalf("notices = %+v, want the allowlist refusal", notices)
	}
	if !notices[0].Ephemeral {
		t.Error("a refusal belongs to the person it refuses, not to the room")
	}
	if got := platform.startedTurns(); got != 0 {
		t.Errorf("started %d turns for a refused message", got)
	}
}
