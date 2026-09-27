package bot

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/tigersoldier/pi-gateway/gwclient"
)

// kind identifies pi-chat connections in the gateway's client roster and
// session catalog.
const kind = "pi-chat"

// thread is one chat thread's agent: a gateway connection bound to the session
// that thread owns.
type thread struct {
	key string
	t   Thread
	b   *Bot
	log *slog.Logger

	// turnMu serializes turns within a thread. A second message waits rather
	// than interleaving; M4 replaces this with admission control and a queue
	// (DESIGN.md §8).
	turnMu sync.Mutex

	// mu guards the connection and session path, which only change while
	// (re)connecting.
	mu     sync.Mutex
	client *gwclient.Client
	path   string

	// cur is the turn the connection's events belong to, if any. It is
	// atomically swapped because the gateway's read goroutine writes to it.
	cur atomic.Pointer[turnState]
}

// runTurn prompts the thread's session and renders the answer.
func (th *thread) runTurn(ctx context.Context, m Message, r Renderer) error {
	th.turnMu.Lock()
	defer th.turnMu.Unlock()

	client, err := th.ensure(ctx)
	if err != nil {
		return err
	}
	if err := r.Start(ctx); err != nil {
		return fmt.Errorf("start the reply: %w", err)
	}
	if strings.TrimSpace(m.Text) == "" {
		// A bare mention is a greeting, not a prompt: answering it beats
		// sending pi an empty message and reporting whatever it says about that.
		return r.Finish(ctx, helpText)
	}

	// GetLastAssistantText returns the session's last answer, which is not
	// necessarily an answer to *this* prompt, so remember it and refuse to
	// present a stale answer as new.
	before, err := client.GetLastAssistantText(ctx)
	if err != nil {
		th.log.Debug("cannot read the previous answer", "error", err)
	}

	st := newTurnState()
	th.cur.Store(st)
	defer th.cur.Store(nil)

	stopFlush := th.flush(ctx, st, r)
	defer stopFlush()

	if _, err := client.Prompt(ctx, trimPrompt(m.Text)); err != nil {
		return fmt.Errorf("prompt: %w", err)
	}
	if err := st.waitStarted(ctx, startGrace); err != nil {
		return err
	}
	if err := client.AwaitSettled(ctx); err != nil {
		return fmt.Errorf("wait for the turn: %w", err)
	}

	final, err := client.GetLastAssistantText(ctx)
	if err != nil {
		th.log.Warn("cannot read the answer", "error", err)
	}
	if final == "" {
		// The streamed deltas are the fallback when the answer cannot be read
		// back (for example if the session died mid-turn).
		final = st.text()
	}
	if final == "" || (final == before && !st.sawWork()) {
		return errors.New("the agent produced no answer")
	}
	stopFlush()
	if err := r.Finish(ctx, final); err != nil {
		return fmt.Errorf("finish the reply: %w", err)
	}
	return nil
}

// ensure returns a connection bound to the thread's session, creating the
// session on first use and reconnecting if the connection died.
func (th *thread) ensure(ctx context.Context) (*gwclient.Client, error) {
	th.mu.Lock()
	defer th.mu.Unlock()

	if th.client != nil {
		if err := th.client.Err(); err == nil {
			return th.client, nil
		}
		th.log.Warn("gateway connection is gone; reconnecting", "error", th.client.Err())
		_ = th.client.Close()
		th.client = nil
	}

	path := th.path
	if path == "" {
		created, err := th.create(ctx)
		if err != nil {
			return nil, err
		}
		path, th.path = created, created
	}

	client, err := th.dial(ctx)
	if err != nil {
		return nil, err
	}
	if _, err := client.SwitchSession(ctx, path); err != nil {
		_ = client.Close()
		// The session may have been deleted outside pi-chat; M4 recreates it
		// and tells the thread. Phase 0 reports the failure.
		return nil, fmt.Errorf("bind session %s: %w", path, err)
	}
	th.client = client
	return client, nil
}

// dial opens the thread's long-lived connection. Events arrive on the
// connection's read goroutine through onEvent, which must never block.
func (th *thread) dial(ctx context.Context) (*gwclient.Client, error) {
	client, err := gwclient.Dial(ctx, gwclient.Config{
		StateDir:  th.b.cfg.Gateway.StateDir,
		TokenFile: th.b.cfg.Gateway.ThreadTokenFile,
		Name:      "pi-chat " + th.t.SessionName(),
		Kind:      kind,
		Tags:      map[string]string{"platform": "slack", "thread": th.key},
		// Attach at the head: history is pi's to own, and replaying a finished
		// turn into a thread would duplicate what Slack already shows.
		LiveOnly: true,
		OnEvent:  th.onEvent,
	})
	if err != nil {
		return nil, fmt.Errorf("open the thread connection: %w", err)
	}
	return client, nil
}

// create makes the thread's session through a throwaway admin connection.
//
// gw_new_session binds the connection that issues it and there is no unbind
// command, so a pooled admin connection would stay attached to every session it
// ever created and make them unevictable (DESIGN.md §10). Dial, create, close.
func (th *thread) create(ctx context.Context) (string, error) {
	cwd := th.b.cfg.Paths.ProjectsRoot
	if err := os.MkdirAll(cwd, 0o755); err != nil {
		return "", fmt.Errorf("create %s: %w", cwd, err)
	}

	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	admin, err := gwclient.Dial(ctx, gwclient.Config{
		StateDir:  th.b.cfg.Gateway.StateDir,
		TokenFile: th.b.cfg.Gateway.AdminTokenFile,
		Name:      "pi-chat create " + th.t.SessionName(),
		Kind:      kind,
	})
	if err != nil {
		return "", fmt.Errorf("open the admin connection: %w", err)
	}
	defer admin.Close()

	ref, err := admin.NewSession(ctx, gwclient.NewSessionRequest{
		Name:   th.t.SessionName(),
		Cwd:    cwd,
		PiArgs: th.b.cfg.Gateway.PiArgs,
		Tags:   map[string]string{"platform": "slack", "thread": th.key},
	})
	if err != nil {
		return "", fmt.Errorf("create the session: %w", err)
	}
	if ref == nil || ref.Path == "" {
		return "", errors.New("the daemon created a session without a path")
	}
	th.log.Info("created session", "session", ref.Name, "path", ref.Path, "cwd", cwd)
	return ref.Path, nil
}

// close ends the thread's connection, leaving its session alone.
func (th *thread) close() {
	th.mu.Lock()
	client := th.client
	th.client = nil
	th.mu.Unlock()
	if client == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = client.Bye(ctx)
	_ = client.Close()
}
