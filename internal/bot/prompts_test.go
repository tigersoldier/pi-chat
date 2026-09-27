package bot

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// makeRepo creates a directory that looks like a repository: it has a `.git`
// entry, which is what a linked worktree has too.
func makeRepo(t *testing.T, root, name string) {
	t.Helper()
	dir := filepath.Join(root, name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", dir, err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".git"), []byte("gitdir: /elsewhere\n"), 0o644); err != nil {
		t.Fatalf("write .git in %s: %v", dir, err)
	}
}

// TestOpenedOffersPromptsAboutRealRepositories is the difference between a
// dynamic suggestion list and a static one: the names in it exist on this
// machine.
func TestOpenedOffersPromptsAboutRealRepositories(t *testing.T) {
	b, platform, _ := newTestBot(t)
	root := b.cfg.Paths.ReposRoot
	makeRepo(t, root, "notes")
	makeRepo(t, root, "app")
	// A directory in the repos root that is not a repository: suggesting work in
	// it would be suggesting something that cannot work.
	if err := os.MkdirAll(filepath.Join(root, "scratch"), 0o755); err != nil {
		t.Fatal(err)
	}

	b.HandleOpened(context.Background(), Opened{
		EventID: "Ev1", Channel: "D1", UserID: "U1", Workspace: "T1",
	})

	waitFor(t, "suggestions", func() bool {
		_, suggestions := platform.suggestedPrompts()
		return len(suggestions) > 0
	})
	channel, suggestions := platform.suggestedPrompts()
	if channel != "D1" {
		t.Errorf("channel = %q, want the conversation that was opened", channel)
	}
	// Slack takes four, and the core stops there rather than trimming later.
	if len(suggestions) != suggestionLimit {
		t.Errorf("got %d suggestions, want %d", len(suggestions), suggestionLimit)
	}
	// Sorted, so the list does not depend on directory order.
	if !strings.Contains(suggestions[0].Message, "app") {
		t.Errorf("first suggestion = %q, want the first repository by name", suggestions[0].Message)
	}
	if !strings.Contains(suggestions[2].Message, "notes") {
		t.Errorf("third suggestion = %q, want the second repository", suggestions[2].Message)
	}
	for _, s := range suggestions {
		if s.Title == "" || s.Message == "" {
			t.Errorf("suggestion %+v is missing a title or a message", s)
		}
		if strings.Contains(s.Message, "scratch") {
			t.Errorf("suggestion %q names a directory that is not a repository", s.Message)
		}
		// Pressing a suggestion sends its message as a plain DM message. One
		// that starts with a slash would arrive as something other than a
		// prompt, which is a confusing way to be ignored.
		if strings.HasPrefix(s.Message, "/") || strings.HasPrefix(s.Message, "@") {
			t.Errorf("suggestion %q is addressed like a command, not a prompt", s.Message)
		}
	}
}

// TestOpenedOffersNothingWhenThereAreNoRepositories: no repositories means no
// suggestions, not a fabricated list.
func TestOpenedOffersNothingWhenThereAreNoRepositories(t *testing.T) {
	b, platform, _ := newTestBot(t)

	b.HandleOpened(context.Background(), Opened{
		EventID: "Ev1", Channel: "D1", UserID: "U1", Workspace: "T1",
	})

	// Nothing to wait for, so nothing to time out on: give the goroutine its
	// chance and then check that it stayed quiet.
	if _, suggestions := platform.suggestedPrompts(); len(suggestions) != 0 {
		t.Errorf("suggestions = %+v, want none", suggestions)
	}
}

// TestOpenedOutsideTheAllowlistIsIgnored: an empty conversation says nothing
// back. The log is the record, because a refusal there is noise the stranger
// cannot act on.
func TestOpenedOutsideTheAllowlistIsIgnored(t *testing.T) {
	b, platform, _ := newTestBot(t)
	makeRepo(t, b.cfg.Paths.ReposRoot, "app")

	b.HandleOpened(context.Background(), Opened{
		EventID: "Ev1", Channel: "D1", UserID: "U2", Workspace: "T1",
	})

	if _, suggestions := platform.suggestedPrompts(); len(suggestions) != 0 {
		t.Errorf("suggestions = %+v, want none for a user outside the allowlist", suggestions)
	}
	if notices := platform.postedNotices(); len(notices) != 0 {
		t.Errorf("notices = %+v, want a silent refusal", notices)
	}
}

// TestOpenedIsClaimedOnce: Slack redelivers, and the claim is what stops the
// same event being acted on twice. The second delivery names a different
// conversation, so acting on it would leave a trace.
func TestOpenedIsClaimedOnce(t *testing.T) {
	b, platform, _ := newTestBot(t)
	makeRepo(t, b.cfg.Paths.ReposRoot, "app")
	ctx := context.Background()

	b.HandleOpened(ctx, Opened{EventID: "Ev1", Channel: "D1", UserID: "U1", Workspace: "T1"})
	waitFor(t, "suggestions", func() bool {
		_, suggestions := platform.suggestedPrompts()
		return len(suggestions) > 0
	})
	b.HandleOpened(ctx, Opened{EventID: "Ev1", Channel: "D2", UserID: "U1", Workspace: "T1"})

	channel, suggestions := platform.suggestedPrompts()
	if len(suggestions) == 0 {
		t.Fatal("the first opening offered nothing")
	}
	if channel != "D1" {
		t.Errorf("channel = %q, want D1: a redelivered event was offered prompts again", channel)
	}
}
