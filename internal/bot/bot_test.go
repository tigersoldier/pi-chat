package bot

import (
	"context"
	"testing"

	"github.com/tigersoldier/pi-chat/internal/config"
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
	message := func(user, channel string) Message {
		return Message{UserID: user, Thread: Thread{Workspace: "T1", Channel: channel, ThreadTS: "1"}}
	}

	tests := []struct {
		name    string
		bot     *Bot
		message Message
		want    bool
	}{
		{"listed user, no channel list", newBot([]string{"U1"}, nil), message("U1", "C1"), true},
		{"unlisted user", newBot([]string{"U1"}, nil), message("U2", "C1"), false},
		{"nobody listed", newBot(nil, nil), message("U1", "C1"), false},
		{"listed user in a listed channel", newBot([]string{"U1"}, []string{"C1"}), message("U1", "C1"), true},
		{"listed user in another channel", newBot([]string{"U1"}, []string{"C1"}), message("U1", "C2"), false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := test.bot.allowed(test.message); got != test.want {
				t.Fatalf("allowed = %v, want %v", got, test.want)
			}
		})
	}
}

func TestSeenSetClaimsEachEventOnce(t *testing.T) {
	seen := newSeenSet(2)

	if !seen.claim("a") {
		t.Fatal("the first claim should succeed")
	}
	if seen.claim("a") {
		t.Fatal("a repeated event must not be claimed twice")
	}
	// An empty ID has nothing to deduplicate on, so it is never a duplicate.
	if !seen.claim("") {
		t.Fatal("an empty ID should be claimable")
	}
	if !seen.claim("") {
		t.Fatal("an empty ID should never count as a duplicate")
	}

	seen.claim("b")
	seen.claim("c") // evicts "a"
	if !seen.claim("a") {
		t.Fatal("an evicted ID should be forgotten")
	}
	if seen.claim("c") {
		t.Fatal("the most recent IDs should still be remembered")
	}
}

// countingPlatform records whether a turn was started.
type countingPlatform struct{ starts int }

func (p *countingPlatform) StartTurn(context.Context, Message) (Renderer, error) {
	p.starts++
	return &recordingRenderer{}, nil
}

func TestHandleMessageRefusesDisallowedUsers(t *testing.T) {
	cfg := config.Defaults()
	cfg.Slack.Access.AllowedUsers = []string{"U1"}
	platform := &countingPlatform{}
	b := New(cfg, discardLogger(), platform)

	b.HandleMessage(context.Background(), Message{
		EventID: "Ev1",
		UserID:  "U2",
		Thread:  Thread{Workspace: "T1", Channel: "C1", ThreadTS: "1"},
		Text:    "run something expensive",
	})

	// HandleMessage returns before dispatching a refusal, so this needs no
	// synchronisation: a refusal must not reach the platform at all.
	if platform.starts != 0 {
		t.Fatalf("a refused message started %d turns, want 0", platform.starts)
	}
}
