package slack

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// These are the calls a stub cannot vouch for. Slack reads arguments from a form
// body, and for some methods it ignores a JSON body entirely — which is how
// conversations.replies answered `invalid_arguments` and users.info answered
// `user_not_found` for users that exist, while the requests themselves looked
// well formed. Only the real API can say whether a request is shaped the way it
// is read, so this is deliberately opt-in:
//
//	PI_CHAT_LIVE=1 PI_CHAT_LIVE_CHANNEL=C… PI_CHAT_LIVE_THREAD_TS=… \
//	  PI_CHAT_LIVE_USER=U… go test ./internal/slack -run Live -v
func TestLiveSlackReadsAThreadAndNames(t *testing.T) {
	api, channel, threadTS, user := liveSlack(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	replies, err := api.Replies(ctx, channel, threadTS, "")
	if err != nil {
		t.Fatalf("Replies: %v", err)
	}
	if len(replies) == 0 {
		t.Fatalf("thread %s in %s came back empty", threadTS, channel)
	}
	t.Logf("read %d messages from %s", len(replies), channel)

	name, err := api.UserInfo(ctx, user)
	if err != nil {
		t.Fatalf("UserInfo: %v", err)
	}
	if strings.TrimSpace(name) == "" {
		t.Errorf("users.info resolved %s to no name at all", user)
	}
	t.Logf("resolved %s to %q", user, name)
}

// A suggestion is a write whose parameter is structured: `prompts` travels as a
// JSON string inside the form body, and a live call is the only thing that shows
// Slack accepts that shape.
func TestLiveSlackSetsSuggestedPrompts(t *testing.T) {
	api, _, _, _ := liveSlack(t)
	dm := os.Getenv("PI_CHAT_LIVE_DM")
	if dm == "" {
		t.Skip("set PI_CHAT_LIVE_DM to the bot's own conversation to write suggestions into it")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	prompts := []suggestion{{Title: "What changed?", Message: "Summarize the uncommitted changes."}}
	err := api.SetSuggestedPrompts(ctx, dm, "Try one of these", prompts)
	// A capability answer is not a shape problem: this workspace's app is not an
	// agent app, so the call cannot be made here at all. Only a call that goes
	// through shows whether Slack read the structured parameter out of the form
	// body, which is what this test is for.
	if IsCode(err, "not_agent_app") || IsCode(err, "feature_disabled") {
		t.Skipf("this install cannot show suggestions: %v", err)
	}
	if err != nil {
		t.Fatalf("SetSuggestedPrompts: %v", err)
	}
}

// liveSlack builds an API for the real Slack, from the token the daemon uses.
func liveSlack(t *testing.T) (api *API, channel, threadTS, user string) {
	t.Helper()
	if os.Getenv("PI_CHAT_LIVE") == "" {
		t.Skip("set PI_CHAT_LIVE=1 to call the real Slack API")
	}
	path := os.Getenv("PI_CHAT_LIVE_TOKEN_FILE")
	if path == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			t.Fatalf("home directory: %v", err)
		}
		path = filepath.Join(home, ".config", "pi-chat", "slack-bot-token")
	}
	token, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	channel, threadTS, user = os.Getenv("PI_CHAT_LIVE_CHANNEL"), os.Getenv("PI_CHAT_LIVE_THREAD_TS"), os.Getenv("PI_CHAT_LIVE_USER")
	if channel == "" || threadTS == "" || user == "" {
		t.Skip("set PI_CHAT_LIVE_CHANNEL, PI_CHAT_LIVE_THREAD_TS and PI_CHAT_LIVE_USER to read a real thread")
	}
	return NewAPI(strings.TrimSpace(string(token)), discardLogger()), channel, threadTS, user
}
