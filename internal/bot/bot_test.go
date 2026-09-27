package bot

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/tigersoldier/pi-chat/internal/config"
	"github.com/tigersoldier/pi-chat/internal/store"
)

func TestThreadIdentity(t *testing.T) {
	thread := Thread{Workspace: "T6K8Y3FRR", Channel: "C0123ABC", ThreadTS: "1721609600.123456"}

	if got, want := thread.Key(), "T6K8Y3FRR:C0123ABC:1721609600.123456"; got != want {
		t.Fatalf("Key() = %q, want %q", got, want)
	}
	// The name is derived, not random, so a restart reattaches to the same
	// session instead of creating a second one.
	if got, want := thread.SessionName(), "slack-t6k8y3frr-c0123abc-1721609600-123456"; got != want {
		t.Fatalf("SessionName() = %q, want %q", got, want)
	}
}

func TestSlugName(t *testing.T) {
	tests := []struct{ in, want string }{
		{"T6K8Y3FRR", "t6k8y3frr"},
		{"1721609600.123456", "1721609600-123456"},
		{"a_b", "a-b"},
		{"--leading and trailing--", "leading-and-trailing"},
		{"a...b", "a-b"},
		{"", ""},
	}
	for _, test := range tests {
		if got := slugName(test.in); got != test.want {
			t.Errorf("slugName(%q) = %q, want %q", test.in, got, test.want)
		}
	}
}

func TestAllowed(t *testing.T) {
	newBot := func(users, channels []string) *Bot {
		cfg := config.Defaults()
		cfg.Slack.Access.AllowedUsers = users
		cfg.Slack.Access.AllowedChannels = channels
		return &Bot{cfg: cfg, log: discardLogger()}
	}

	tests := []struct {
		name    string
		bot     *Bot
		user    string
		channel string
		want    bool
	}{
		{"listed user, no channel list", newBot([]string{"U1"}, nil), "U1", "C1", true},
		{"unlisted user", newBot([]string{"U1"}, nil), "U2", "C1", false},
		{"nobody listed", newBot(nil, nil), "U1", "C1", false},
		{"listed user in a listed channel", newBot([]string{"U1"}, []string{"C1"}), "U1", "C1", true},
		{"listed user in another channel", newBot([]string{"U1"}, []string{"C1"}), "U1", "C2", false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := test.bot.allowed(test.user, test.channel); got != test.want {
				t.Fatalf("allowed(%q, %q) = %v, want %v", test.user, test.channel, got, test.want)
			}
		})
	}
}

func TestHandleMessageRefusesAndExplains(t *testing.T) {
	b, platform, _ := newTestBot(t)
	message := newTestMessage("Ev1", "run something expensive")
	message.UserID = "U2" // not on the allowlist

	b.HandleMessage(context.Background(), message)

	notices := platform.postedNotices()
	if len(notices) != 1 {
		t.Fatalf("posted %d notices, want exactly one refusal", len(notices))
	}
	if !notices[0].Ephemeral {
		t.Error("a refusal should be visible only to the sender")
	}
	if platform.startedTurns() != 0 {
		t.Error("a refused message reached the agent")
	}
	// The refusal names the fact, not the configuration: the allowlist itself
	// is the owner's business.
	if strings.Contains(notices[0].Text, "allowed_users") {
		t.Errorf("the refusal leaks the configuration: %q", notices[0].Text)
	}
}

func TestHandleMessageRefusesAnUnlistedChannel(t *testing.T) {
	b, platform, _ := newTestBot(t)
	b.cfg.Slack.Access.AllowedChannels = []string{"C-OTHER"}

	b.HandleMessage(context.Background(), newTestMessage("Ev1", "hello"))

	notices := platform.postedNotices()
	if len(notices) != 1 || !notices[0].Ephemeral {
		t.Fatalf("notices = %+v, want one ephemeral refusal", notices)
	}
	if platform.startedTurns() != 0 {
		t.Error("a message from an unlisted channel reached the agent")
	}
}

func TestHandleMessageIgnoresADuplicateEvent(t *testing.T) {
	// The gateway has no prompt idempotency key, so a redelivery that reaches
	// the core twice would otherwise run the same prompt twice (DESIGN.md §7).
	b, platform, _ := newTestBot(t)
	message := newTestMessage("Ev1", "hello")

	b.HandleMessage(context.Background(), message)
	b.HandleMessage(context.Background(), message)

	waitFor(t, "the first turn", func() bool { return platform.startedTurns() == 1 })
	// A duplicate would start its turn from the same goroutine path the first one
	// took, so a moment of patience is what turns "no second turn yet" into "no
	// second turn". This can only make the test weaker on a slow machine, never
	// wrong.
	time.Sleep(20 * time.Millisecond)
	if got := platform.startedTurns(); got != 1 {
		t.Fatalf("started %d turns for one event, want 1", got)
	}
}

func TestHandleMessageDropsPlainTextInAThreadWeDoNotOwn(t *testing.T) {
	b, platform, st := newTestBot(t)
	message := newTestMessage("Ev1", "just chatting with someone else")
	message.Mentioned = false

	b.HandleMessage(context.Background(), message)

	// The drop happens synchronously and before anything is recorded, so this
	// needs no waiting: a stranger's thread must not leave a row behind either.
	if platform.startedTurns() != 0 {
		t.Error("plain text in a stranger's thread started a session")
	}
	if notices := platform.postedNotices(); len(notices) != 0 {
		t.Errorf("posted %+v, want silence", notices)
	}
	if rows, err := st.Threads(context.Background()); err != nil {
		t.Fatalf("list threads: %v", err)
	} else if len(rows) != 0 {
		t.Errorf("recorded %d threads for a dropped message", len(rows))
	}
}

func TestHandleMessageContinuesOurOwnThreadWithoutAMention(t *testing.T) {
	b, platform, st := newTestBot(t)
	seedThread(t, st, store.ThreadRow{
		ThreadKey: "T1:C1:1700000000.000100", WorkspaceID: "T1", ChannelID: "C1",
		ThreadTS: "1700000000.000100", SessionPath: "/sessions/x.jsonl", State: store.StateCold,
	})
	message := newTestMessage("Ev1", "carry on")
	message.Mentioned = false

	b.HandleMessage(context.Background(), message)

	waitFor(t, "the turn", func() bool { return platform.startedTurns() == 1 })
}

func TestHandleMessageRoutesAMentionedCommand(t *testing.T) {
	b, platform, _ := newTestBot(t)

	b.HandleMessage(context.Background(), newTestMessage("Ev1", "/status"))

	waitFor(t, "the answer", func() bool { return len(platform.postedNotices()) == 1 })
	if platform.startedTurns() != 0 {
		t.Fatal("`@pi /status` was sent to the agent as a prompt")
	}
}

func TestHandleMessageGuardsNearMisses(t *testing.T) {
	// `@pi status` meant `@pi /status`: answering it as a prompt would spend a
	// turn to be told the obvious.
	b, platform, _ := newTestBot(t)

	b.HandleMessage(context.Background(), newTestMessage("Ev1", "status"))

	waitFor(t, "the hint", func() bool { return len(platform.postedNotices()) == 1 })
	notices := platform.postedNotices()
	if !notices[0].Ephemeral {
		t.Error("a hint should not be a message the whole channel sees")
	}
	if !strings.Contains(notices[0].Text, "@pi /status") {
		t.Errorf("the hint does not spell out the command: %q", notices[0].Text)
	}
	if platform.startedTurns() != 0 {
		t.Error("a near miss reached the agent")
	}
}

func TestHandleMessageDoesNotGuardRealPrompts(t *testing.T) {
	// "delete the old branch" is a prompt, not a mis-typed `/delete`. The guard
	// only covers a message that is exactly a command name.
	b, platform, _ := newTestBot(t)

	b.HandleMessage(context.Background(), newTestMessage("Ev1", "delete the old branch"))

	waitFor(t, "the turn", func() bool { return platform.startedTurns() == 1 })
	if notices := platform.postedNotices(); len(notices) != 0 {
		t.Errorf("a real prompt earned a hint: %+v", notices)
	}
}

func TestHandleMessageAnswersABareMention(t *testing.T) {
	b, platform, st := newTestBot(t)

	b.HandleMessage(context.Background(), newTestMessage("Ev1", ""))

	waitFor(t, "the greeting", func() bool { return len(platform.postedNotices()) == 1 })
	if platform.startedTurns() != 0 {
		t.Fatal("a greeting created a session")
	}
	// The thread is remembered, so a later plain-text reply in it continues the
	// conversation, but no session exists yet.
	row, found, err := st.Thread(context.Background(), "T1:C1:1700000000.000100")
	if err != nil || !found {
		t.Fatalf("the thread was not recorded: found=%v err=%v", found, err)
	}
	if row.SessionPath != "" {
		t.Errorf("a greeting created a session: %q", row.SessionPath)
	}
}

func TestHandleMessageAnswersAttachmentsOnlyOnce(t *testing.T) {
	b, platform, _ := newTestBot(t)
	message := newTestMessage("Ev1", "")
	message.Files = 2

	b.HandleMessage(context.Background(), message)

	waitFor(t, "the notice", func() bool { return len(platform.postedNotices()) == 1 })
	if got := platform.postedNotices()[0].Text; !strings.Contains(got, "attachment") {
		t.Errorf("the notice does not mention attachments: %q", got)
	}
	if platform.startedTurns() != 0 {
		t.Error("an attachment-only message was sent to the agent as an empty prompt")
	}
}

func TestCloseIdleMarksStaleWarmThreadsCold(t *testing.T) {
	// A tracked thread whose connection is gone while its row still says warm is
	// what a restarted daemon leaves behind: the marker has to follow reality.
	b, _, st := newTestBot(t)
	seedThread(t, st, store.ThreadRow{
		ThreadKey: "T1:C1:1700000000.000100", WorkspaceID: "T1", ChannelID: "C1",
		ThreadTS: "1700000000.000100", SessionPath: "/sessions/x.jsonl", State: store.StateWarm,
		LastActive: time.Now(),
	})
	thread := Thread{Workspace: "T1", Channel: "C1", ThreadTS: "1700000000.000100"}
	if _, ok := b.tracked(thread); !ok {
		t.Fatal("the seeded thread did not load")
	}

	b.closeIdle(context.Background())

	row, _, err := st.Thread(context.Background(), thread.Key())
	if err != nil {
		t.Fatalf("read the row: %v", err)
	}
	if row.State != store.StateCold {
		t.Errorf("state = %q, want %q after the sweep", row.State, store.StateCold)
	}
}
