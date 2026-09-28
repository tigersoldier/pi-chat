package bot

import (
	"context"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/tigersoldier/pi-gateway/gwclient"

	"github.com/tigersoldier/pi-chat/internal/config"
	"github.com/tigersoldier/pi-chat/internal/store"
	"github.com/tigersoldier/pi-chat/internal/workspace"
)

// workspaceProject provisions a project directory for a test.
func workspaceProject(t *testing.T, b *Bot, prompt string) workspace.Project {
	t.Helper()
	project, err := b.work.Provision(prompt)
	if err != nil {
		t.Fatalf("provision: %v", err)
	}
	return project
}

// fakePlatform records what the core asks a platform to do.
type fakePlatform struct {
	mu       sync.Mutex
	turns    []Message
	notices  []Notice
	opened   []string // the text of each thread the core asked for
	thread   Thread   // what OpenThread returns; zero means a stand-in
	renderer *recordingRenderer
	statuses []Status // lifecycle states, in the order the core asked for them

	prompts        []Suggestion
	promptsChannel string

	// said is what the fake reports for a thread's conversation, and observeErr
	// what it fails with; the zero value is "nobody said anything". Being an
	// Observer is what makes the observation path testable without a gateway.
	said       []Said
	observeErr error
	oldest     string // the watermark the core asked from
	fetches    int
}

// SetSuggestedPrompts records suggestions, making the fake a PromptReporter.
func (p *fakePlatform) SetSuggestedPrompts(_ context.Context, channel, _ string, suggestions []Suggestion) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.promptsChannel = channel
	p.prompts = append([]Suggestion(nil), suggestions...)
	return nil
}

// suggestedPrompts returns what the core offered, and to which channel.
func (p *fakePlatform) suggestedPrompts() (string, []Suggestion) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.promptsChannel, append([]Suggestion(nil), p.prompts...)
}

func (p *fakePlatform) StartTurn(_ context.Context, m Message) (Renderer, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.turns = append(p.turns, m)
	if p.renderer == nil {
		p.renderer = &recordingRenderer{}
	}
	return p.renderer, nil
}

func (p *fakePlatform) Post(_ context.Context, n Notice) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.notices = append(p.notices, n)
	return nil
}

func (p *fakePlatform) OpenThread(_ context.Context, channel, text string) (Thread, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.opened = append(p.opened, text)
	thread := p.thread
	if thread.ThreadTS == "" {
		thread = Thread{Workspace: "T1", Channel: channel, ThreadTS: "900.000001"}
	}
	return thread, nil
}

// startedTurns is how many turns the core has begun.
func (p *fakePlatform) startedTurns() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.turns)
}

// Conversation reports what a test put in the thread, making the fake an
// Observer. It records the watermark it was asked from, which is what a test
// about observation is usually asserting.
func (p *fakePlatform) Conversation(_ context.Context, _ Thread, oldest string) ([]Said, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.oldest = oldest
	p.fetches++
	if p.observeErr != nil {
		return nil, p.observeErr
	}
	return append([]Said(nil), p.said...), nil
}

// conversationCall reports the watermark the core last read from, and how many
// times it read.
func (p *fakePlatform) conversationCall() (string, int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.oldest, p.fetches
}

// SetStatus records a lifecycle state, making the fake a StatusReporter.
func (p *fakePlatform) SetStatus(_ context.Context, _ Thread, s Status) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.statuses = append(p.statuses, s)
	return nil
}

// reportedStatuses returns the states the core asked for, in order.
func (p *fakePlatform) reportedStatuses() []Status {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]Status(nil), p.statuses...)
}

// postedNotices returns the notices posted so far.
func (p *fakePlatform) postedNotices() []Notice {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]Notice(nil), p.notices...)
}

// openedThreads returns the texts of the threads the core asked to open.
func (p *fakePlatform) openedThreads() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.opened...)
}

// fakeGateway serves a canned session catalog.
type fakeGateway struct {
	rows   []gwclient.SessionRow
	status string
	err    error
}

func (g *fakeGateway) Sessions(context.Context) ([]gwclient.SessionRow, error) {
	return g.rows, g.err
}

func (g *fakeGateway) Status(context.Context) string { return g.status }

// newTestBot builds a core whose state lives in temporary directories, with
// the platform and the gateway replaced by recorders.
func newTestBot(t *testing.T) (*Bot, *fakePlatform, *store.Store) {
	return newTestBotWith(t, nil)
}

// newTestBotWith builds a test core, letting the caller adjust the
// configuration before the core reads it: the provisioner and the renderer take
// their settings at construction, exactly as they do in the daemon.
func newTestBotWith(t *testing.T, adjust func(*config.Config)) (*Bot, *fakePlatform, *store.Store) {
	t.Helper()
	dir := t.TempDir()
	cfg := config.Defaults()
	cfg.Slack.Access.AllowedUsers = []string{"U1"}
	cfg.Paths.ProjectsRoot = filepath.Join(dir, "work")
	cfg.Paths.ReposRoot = filepath.Join(dir, "code")
	cfg.Paths.DBPath = filepath.Join(dir, "state", "pi-chat.db")
	// The gateway is pinned into the temporary directory too. Defaults() leaves
	// these unexpanded (a literal `~`), so a test cannot reach a real daemon by
	// luck; saying so here keeps it that way when someone expands the default.
	cfg.Gateway.StateDir = filepath.Join(dir, "gateway")
	cfg.Gateway.AdminTokenFile = filepath.Join(dir, "gateway", "admin.token")
	cfg.Gateway.ThreadTokenFile = filepath.Join(dir, "gateway", "thread.token")
	if adjust != nil {
		adjust(cfg)
	}

	st, err := store.Open(context.Background(), cfg.Paths.DBPath)
	if err != nil {
		t.Fatalf("open the store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	platform := &fakePlatform{}
	b := New(cfg, discardLogger(), platform, st, "test")
	b.gw = &fakeGateway{}
	return b, platform, st
}

// newTestMessage is a mention in a channel thread.
func newTestMessage(eventID, text string) Message {
	return Message{
		EventID:   eventID,
		Thread:    Thread{Workspace: "T1", Channel: "C1", ThreadTS: "1700000000.000100"},
		UserID:    "U1",
		Workspace: "T1",
		Text:      text,
		Mentioned: true,
	}
}

// seedThread writes a thread row, as a previous run would have left it.
func seedThread(t *testing.T, st *store.Store, row store.ThreadRow) {
	t.Helper()
	if row.ThreadKey == "" {
		row.ThreadKey = "T1:C1:1700000000.000100"
	}
	if row.CreatedAt.IsZero() {
		row.CreatedAt = time.Now()
	}
	if row.LastActive.IsZero() {
		row.LastActive = row.CreatedAt
	}
	if err := st.PutThread(context.Background(), row); err != nil {
		t.Fatalf("seed the thread: %v", err)
	}
}

// waitFor polls until cond holds. The core does its work in goroutines, so a
// test has to wait for a state it can observe rather than for a signal that
// does not exist yet.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}
