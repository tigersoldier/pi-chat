package bot

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/tigersoldier/pi-gateway/gwclient"

	"github.com/tigersoldier/pi-chat/internal/config"
	"github.com/tigersoldier/pi-chat/internal/store"
	"github.com/tigersoldier/pi-chat/internal/workspace"
)

// TestLiveTurnThroughGateway drives real turns through pi-gatewayd with the
// tokens pi-chat is configured to use. It is the seam the unit tests cannot
// cover: provisioning a project directory, creating a session through a
// throwaway admin connection, binding a thread connection, streaming a turn,
// closing the connection, and resuming the same session from its cursor.
//
// It needs a running pi-gatewayd and a working pi, it writes into the real
// projects root, and it spends a few tokens, so it is opt-in:
//
//	PI_CHAT_LIVE=1 go test ./internal/bot -run Live -v
func TestLiveTurnThroughGateway(t *testing.T) {
	if os.Getenv("PI_CHAT_LIVE") == "" {
		t.Skip("set PI_CHAT_LIVE=1 to run against a live pi-gatewayd")
	}

	cfg, err := config.Load("")
	if err != nil {
		t.Fatalf("load %s: %v", config.DefaultPath(), err)
	}
	// A debug logger to stderr, so a live run shows exactly what the gateway
	// sent.
	debugLog := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug}))

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	// The database lives in a temporary directory: the live test is about the
	// gateway seam, not about pi-chat's own state, and it must not inherit rows
	// from earlier runs.
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "pi-chat.db"))
	if err != nil {
		t.Fatalf("open the store: %v", err)
	}
	defer st.Close()

	platform := &fakePlatform{}
	b := New(cfg, debugLog, platform, st, "live-test")
	defer b.Close()

	// A thread of its own, named after the run so it is obvious in the gateway's
	// catalog where it came from.
	thread := Thread{
		Workspace: "T-LIVE",
		Channel:   "C-LIVE",
		ThreadTS:  strconv.FormatInt(time.Now().Unix(), 10),
	}
	th, err := b.threadFor(thread)
	if err != nil {
		t.Fatalf("track the live thread: %v", err)
	}
	t.Cleanup(func() { cleanupLiveSession(t, cfg, th, debugLog) })

	// A first turn: this is where the session is created, so it is also where
	// provisioning is checked.
	first := &recordingRenderer{signal: make(chan struct{}, 16)}
	if err := th.runTurn(ctx, liveMessage(thread, "Reply with exactly the single word PONG and nothing else."), first); err != nil {
		t.Fatalf("the first turn failed: %v", err)
	}
	if len(first.finals) != 1 {
		t.Fatalf("rendered %d final answers, want 1", len(first.finals))
	}
	t.Logf("first turn: %d delta(s), answer %q", len(first.deltas), first.finals[0])
	// "deltas streamed" is the promise the renderer makes; a turn that produced
	// no delta at all would still pass the answer check below.
	if len(first.deltas) == 0 {
		t.Error("the first turn streamed no deltas")
	}
	if !strings.Contains(first.finals[0], "PONG") {
		t.Fatalf("the answer %q does not contain PONG", first.finals[0])
	}

	row := th.snapshot()
	if row.SessionPath == "" || row.SessionName == "" {
		t.Fatalf("the session was not recorded: %+v", row)
	}
	// A session gets a project directory of its own under the projects root,
	// not the projects root itself (DESIGN.md §9).
	if !strings.HasPrefix(row.Cwd, cfg.Paths.ProjectsRoot+string(filepath.Separator)) {
		t.Fatalf("cwd %q is not a project directory under %q", row.Cwd, cfg.Paths.ProjectsRoot)
	}
	if row.Cwd != row.ProjectDir {
		t.Errorf("cwd %q and project dir %q disagree", row.Cwd, row.ProjectDir)
	}

	// Closing the connection is the idle-close path: the session stays, and the
	// next turn has to reattach to the session it left rather than start
	// another one.
	th.turnMu.Lock()
	th.close(ctx)
	th.turnMu.Unlock()
	if row := th.snapshot(); row.State != store.StateCold {
		t.Fatalf("state after closing = %q, want %q", row.State, store.StateCold)
	}

	second := &recordingRenderer{signal: make(chan struct{}, 16)}
	if err := th.runTurn(ctx, liveMessage(thread, "Reply with exactly the single word PONG2 and nothing else."), second); err != nil {
		t.Fatalf("the resumed turn failed: %v", err)
	}
	if len(second.finals) != 1 {
		t.Fatalf("rendered %d final answers, want 1", len(second.finals))
	}
	t.Logf("resumed turn: %d delta(s), answer %q, cursor seq %d",
		len(second.deltas), second.finals[0], th.snapshot().LastSeq)
	if len(second.deltas) == 0 {
		t.Error("the resumed turn streamed no deltas")
	}
	if !strings.Contains(second.finals[0], "PONG2") {
		t.Fatalf("the resumed answer %q does not contain PONG2", second.finals[0])
	}

	// Same session, not a second one: the whole point of persisting the path and
	// the cursor.
	after := th.snapshot()
	if after.SessionPath != row.SessionPath {
		t.Fatalf("the thread moved to a different session: %q then %q", row.SessionPath, after.SessionPath)
	}
	if after.State != store.StateWarm {
		t.Errorf("state after the resumed turn = %q, want %q", after.State, store.StateWarm)
	}
}

// liveMessage is one prompt in the live thread.
func liveMessage(thread Thread, text string) Message {
	return Message{
		EventID:   "live-" + strconv.FormatInt(time.Now().UnixNano(), 10),
		Thread:    thread,
		UserID:    "U-LIVE",
		Workspace: thread.Workspace,
		Text:      text,
		Mentioned: true,
	}
}

// cleanupLiveSession removes what the live test created: the session through
// the gateway, then the project directory through the same cleanup that
// `@pi /delete` will call in phase 2. It reports rather than fails: a cleanup
// problem must not mask a passing run.
func cleanupLiveSession(t *testing.T, cfg *config.Config, th *thread, log *slog.Logger) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()

	row := th.snapshot()
	if row.SessionPath != "" {
		admin, err := gwclient.Dial(ctx, gwclient.Config{
			StateDir:  cfg.Gateway.StateDir,
			TokenFile: cfg.Gateway.AdminTokenFile,
			Name:      "pi-chat live cleanup",
			Kind:      kind,
		})
		if err != nil {
			log.Warn("live cleanup: cannot open an admin connection", "error", err)
		} else {
			defer admin.Close()
			if _, err := admin.DeleteSession(ctx, row.SessionPath, true); err != nil {
				log.Warn("live cleanup: cannot delete the session", "error", err)
			}
		}
	}
	if row.ProjectDir == "" {
		return
	}
	provisioner := workspace.New(workspace.Config{
		ProjectsRoot: cfg.Paths.ProjectsRoot,
		ReposRoot:    cfg.Paths.ReposRoot,
	}, log)
	result, err := provisioner.Cleanup(ctx, row.ProjectDir)
	if err != nil {
		log.Warn("live cleanup: the project directory", "error", err)
		return
	}
	log.Info("live cleanup", "dir", row.ProjectDir, "removed", result.RemovedDir, "left", result.LeftBehind)
}
