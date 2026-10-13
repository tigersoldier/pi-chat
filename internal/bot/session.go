package bot

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"github.com/tigersoldier/pi-gateway/gwclient"
	"github.com/tigersoldier/pi-gateway/protocol"

	"github.com/tigersoldier/pi-chat/internal/chattools"
	"github.com/tigersoldier/pi-chat/internal/store"
	"github.com/tigersoldier/pi-chat/internal/workspace"
)

// kind identifies pi-chat connections in the gateway's client roster and in
// the creator tags of the sessions it makes.
const kind = "pi-chat"

// Timeouts for the calls that talk to the gateway and the database. They are
// deliberately short: every one of them is on the path between a user's
// message and the beginning of an answer.
const (
	sessionCreateTimeout = 30 * time.Second
	cursorTimeout        = 5 * time.Second
	storeTimeout         = 5 * time.Second
	createAttemptTag     = "pi-chat-create-attempt"
)

// thread is one chat thread's agent: a connection bound to the session that
// thread owns, plus the durable row describing both.
type thread struct {
	key string
	t   Thread
	b   *Bot
	log *slog.Logger

	// turnMu serializes turns within a thread: a second prompt waits rather
	// than interleaving with the first. It is also what the idle sweep takes
	// before closing a connection, so a connection is never closed under a
	// running turn.
	turnMu sync.Mutex

	// connMu guards the bound connection. It is held while dialing, binding
	// and closing — never for the duration of a turn.
	connMu sync.Mutex
	client *gwclient.Client

	// sessionEpoch invalidates callbacks from a connection after the thread
	// retires or deletes the session it was attached to. sessionChanging gates
	// new observed-turn reservations during those operations.
	sessionEpoch    atomic.Uint64
	sessionChanging atomic.Bool

	// rowMu guards the durable row. It is a second, narrower lock than connMu
	// on purpose: the row is written from the turn path, the idle sweep and
	// the cursor saver, and those must not queue behind a dial.
	rowMu sync.Mutex
	row   store.ThreadRow

	// assistantMu guards the last known assistant answer. An observed turn uses
	// it as a baseline if idle-close drops the connection before it can query.
	assistantMu    sync.Mutex
	assistantHash  [32]byte
	assistantKnown bool

	// cur is the turn the connection's events belong to, if any. It is
	// atomically swapped because the gateway's read goroutine reads it while
	// the turn path writes it.
	cur atomic.Pointer[turnState]

	// instrMu guards instr, which the turn path checks and the connection's read
	// goroutine updates when a compaction discards the instruction.
	instrMu sync.Mutex
	instr   instructionState
}

// errSessionGone reports that the session this thread owns is not in the
// gateway any more: it was deleted outside pi-chat (pilish, another client, the
// operator). The thread keeps its key and starts a new session when the user
// asks again (DESIGN.md §4 invariant 3).
var errSessionGone = errors.New("the session is gone")

// isGone reports whether the gateway answered `unknown_session`.

// snapshot returns a copy of the thread's durable row.
func (th *thread) snapshot() store.ThreadRow {
	th.rowMu.Lock()
	defer th.rowMu.Unlock()
	return th.row
}

// update applies fn to the row, keeps the in-memory copy in step, and writes
// it back. A write failure is logged rather than returned: the database is how
// the bot remembers, but a thread must keep working when it hiccups, and the
// row is rewritten on the next change.
//
// The write happens under rowMu. Releasing the lock first would let two updates
// be flushed out of order, leaving the database with an older snapshot than
// memory (a warm marker landing after a close, for instance).
func (th *thread) update(fn func(*store.ThreadRow)) {
	th.rowMu.Lock()
	defer th.rowMu.Unlock()
	fn(&th.row)

	ctx, cancel := context.WithTimeout(context.Background(), storeTimeout)
	defer cancel()
	if err := th.b.store.PutThread(ctx, th.row); err != nil {
		th.log.Error("cannot record the thread state", "error", err)
	}
}

// touch records that the thread was active now, which is what keeps the idle
// sweep away from it.
func (th *thread) touch() {
	now := time.Now()
	th.update(func(row *store.ThreadRow) { row.LastActive = now })
}

// live returns the bound connection, or nil when the thread has none or the
// one it had is gone.
func (th *thread) live() *gwclient.Client {
	th.connMu.Lock()
	defer th.connMu.Unlock()
	if th.client == nil || th.client.Err() != nil {
		return nil
	}
	return th.client
}

// busy reports whether a turn is running in this process. It is the signal the
// status command reads before touching the client: see statusThread.
func (th *thread) busy() bool { return th.cur.Load() != nil }

// assistantBaseline returns a fixed-size fingerprint instead of retaining a
// potentially large answer for every tracked thread.
func (th *thread) assistantBaseline() ([32]byte, bool) {
	th.assistantMu.Lock()
	defer th.assistantMu.Unlock()
	return th.assistantHash, th.assistantKnown
}

// rememberAssistant keeps only the fingerprint needed to distinguish a new
// answer from the last one after reconnect.
func (th *thread) rememberAssistant(text string) {
	th.assistantMu.Lock()
	th.assistantHash = sha256.Sum256([]byte(text))
	th.assistantKnown = true
	th.assistantMu.Unlock()
}

func (th *thread) forgetAssistant() {
	th.assistantMu.Lock()
	th.assistantHash = [32]byte{}
	th.assistantKnown = false
	th.assistantMu.Unlock()
}

// finishTurn releases a reservation after the renderer lifecycle has ended.
// The caller holds turnMu.
func (th *thread) finishTurn(st *turnState) {
	if !th.cur.CompareAndSwap(st, nil) {
		th.log.Debug("a following turn reserved the thread before this renderer finished")
	}
	st.finish()
	th.turnMu.Unlock()
}

// abort stops the turn running in this thread, reporting whether there was one
// to stop. The platform's stop control is also how a stale loading indicator
// gets cleared, and the caller has to tell those two cases apart.
//
// It deliberately does not take turnMu: a turn holds that lock for its whole
// life, so waiting for it would mean stopping pi only after pi had finished.
func (th *thread) abort(ctx context.Context) bool {
	if !th.busy() {
		return false
	}
	client := th.live()
	if client == nil {
		return false
	}
	ctx, cancel := context.WithTimeout(ctx, gatewayTimeout)
	defer cancel()
	if _, err := client.Abort(ctx); err != nil {
		th.log.Warn("cannot stop the turn", "error", err)
		return false
	}
	return true
}

// bound reports whether a connection is still attached to the session this
// thread owns. gwclient keeps Err() nil when the daemon unbinds a client, so
// the binding itself is what has to be checked.
func (th *thread) bound(client *gwclient.Client) bool {
	session := client.Session()
	path := th.snapshot().SessionPath
	return session != nil && (path == "" || session.Path == path)
}

// isGone reports whether the gateway answered `unknown_session`: the session
// the thread pointed at is not there any more.
func isGone(err error) bool {
	var response *gwclient.ResponseError
	return errors.As(err, &response) && response.Code == protocol.CodeUnknownSession
}

// forgetSession records that the thread's session is gone and releases the
// connection that was bound to it.
func (th *thread) forgetSession(ctx context.Context) {
	th.forgetSessionForPath(ctx, "")
}

func (th *thread) forgetSessionForPath(ctx context.Context, expectedPath string) {
	th.connMu.Lock()
	if expectedPath != "" && th.snapshot().SessionPath != expectedPath {
		th.connMu.Unlock()
		th.revokeDeletedSessionBinding(ctx, expectedPath)
		return // an idle deletion callback arrived after this thread moved on
	}
	client := th.client
	th.client = nil
	th.connMu.Unlock()
	if client != nil {
		_ = client.Close()
	}
	th.forgetSessionLocked(ctx, expectedPath)
}

// forgetSessionLocked records that the session is gone. The caller holds connMu
// and has already cleared th.client.
//
// The key stays reserved and the working directory is left alone — the session
// file is what was deleted, and anything the agent wrote is still on disk — but
// the thread stops owning a session, so the next message starts a fresh one
// deliberately instead of re-attaching a tombstone.
func (th *thread) forgetSessionLocked(ctx context.Context, expectedPath string) {
	path := th.snapshot().SessionPath
	if expectedPath != "" && path != expectedPath {
		th.revokeDeletedSessionBinding(ctx, expectedPath)
		return
	}
	if path != "" {
		th.revokeDeletedSessionBinding(ctx, path)
	}
	changed := false
	th.update(func(row *store.ThreadRow) {
		if expectedPath != "" && row.SessionPath != expectedPath {
			return
		}
		changed = true
		row.State = store.StateDeleted
		row.SessionName, row.SessionPath, row.SessionID = "", "", ""
		row.LastSeq, row.LeafID, row.ProgressTS = 0, "", ""
	})
	if changed {
		th.sessionEpoch.Add(1)
		th.forgetAssistant()
		th.log.Warn("the session was deleted outside pi-chat", "thread", th.key)
	}
}

func (th *thread) revokeDeletedSessionBinding(ctx context.Context, path string) {
	storeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), storeTimeout)
	defer cancel()
	if err := th.b.store.RevokeSessionToolBinding(storeCtx, path); err != nil {
		th.log.Error("cannot revoke the deleted session's chat-tools binding", "path", path, "error", err)
	}
}

// ensure returns a connection bound to the thread's session, creating the
// session on first use, re-dialing after an idle close, and replacing a
// connection that died or lost its binding. prompt seeds the project
// directory's name when a session has to be created.
//
// Attaching rather than assuming is invariant 1 in DESIGN.md §4. The subtle
// half of that is the connection that is alive but unbound: the daemon unbinds
// clients when a session is stopped or deleted, and gwclient reports that by
// clearing Session() rather than by failing the connection, so Err() alone
// would let the next prompt create a second session — the exact defect §3
// documents.
func (th *thread) ensure(ctx context.Context, prompt string) (*gwclient.Client, error) {
	th.connMu.Lock()
	defer th.connMu.Unlock()

	if th.client != nil {
		switch {
		case th.client.Err() != nil:
			th.log.Warn("the gateway connection is gone; opening another", "error", th.client.Err())
		case !th.bound(th.client):
			th.log.Warn("the gateway connection lost its binding; re-attaching",
				"session", th.snapshot().SessionName)
		default:
			return th.client, nil
		}
		_ = th.client.Close()
		th.client = nil
	}

	if th.snapshot().SessionPath == "" {
		if err := th.create(ctx, prompt); err != nil {
			return nil, err
		}
	}
	client, err := th.dial(ctx)
	if err != nil {
		return nil, err
	}
	path := th.snapshot().SessionPath
	if _, err := client.SwitchSession(ctx, path); err != nil {
		_ = client.Close()
		if isGone(err) {
			th.forgetSessionLocked(ctx, path)
			return nil, fmt.Errorf("%w: %s", errSessionGone, path)
		}
		return nil, fmt.Errorf("bind session %s: %w", path, err)
	}
	if th.b.cfg.ChatTools.Enabled {
		if err := th.verifyActiveChatTools(ctx, client, path); err != nil {
			_ = client.Close()
			if isGone(err) {
				th.forgetSessionLocked(ctx, path)
				return nil, fmt.Errorf("%w: %s", errSessionGone, path)
			}
			return nil, err
		}
	}
	th.client = client
	th.update(func(row *store.ThreadRow) { row.State = store.StateWarm })
	th.log.Info("bound the thread to its session",
		"session", th.snapshot().SessionName, "path", path)
	return client, nil
}

// create makes the thread's session: a fresh project directory, the injected
// prompt, and a throwaway admin connection.
//
// The caller holds connMu. gw_new_session binds the connection that issues it
// and there is no unbind command, so a pooled admin connection would stay
// attached to every session it ever created and make them unevictable — a cap
// that silently never evicts (DESIGN.md §8, §10). Dial, create, close.
func (th *thread) create(ctx context.Context, prompt string) error {
	th.forgetAssistant()
	if row := th.snapshot(); row.State == store.StateCold && row.SessionPath == "" && row.ProjectDir != "" {
		if err := th.reconcilePendingCreate(ctx, row.ProjectDir); err != nil {
			return err
		}
		if th.snapshot().SessionPath != "" {
			return nil // a prior ambiguous create reached the gateway after its reply was lost
		}
	}
	project, err := th.b.work.Provision(prompt)
	if err != nil {
		return err
	}
	// The directory has no row yet, so only the next startup sweep would find
	// it: a failure below must not leave it behind. Cleanup is the same
	// worktree-aware path /delete uses, so nothing but an empty fresh directory
	// can go.
	created := false
	defer func() {
		if created {
			return
		}
		if _, err := th.b.work.Cleanup(context.WithoutCancel(ctx), project.Dir); err != nil {
			th.log.Warn("cannot clean up the project directory of a failed session",
				"dir", project.Dir, "error", err)
		}
	}()

	piArgs := th.b.piArgs(project)

	ctx, cancel := context.WithTimeout(ctx, sessionCreateTimeout)
	defer cancel()

	admin, err := gwclient.Dial(ctx, gwclient.Config{
		StateDir:  th.b.cfg.Gateway.StateDir,
		TokenFile: th.b.cfg.Gateway.AdminTokenFile,
		Name:      "pi-chat create " + th.t.SessionName(),
		Kind:      kind,
	})
	if err != nil {
		return fmt.Errorf("open the admin connection: %w", err)
	}
	defer admin.Close()
	gatewaySessionCreated := false
	defer func() {
		if !gatewaySessionCreated || created {
			return
		}
		cleanupCtx, cleanupCancel := context.WithTimeout(context.WithoutCancel(ctx), sessionCreateTimeout)
		defer cleanupCancel()
		if _, err := admin.DeleteSession(cleanupCtx, "", false); err != nil && !isGone(err) {
			created = true
			// Leave a pending owner rather than granting tools based only on a
			// live health marker. The next attempt re-reads the tagged catalog row,
			// verifies its durable spawn record and extension, then commits it.
			th.keepPendingProject(project.Dir)
			th.log.Warn("cannot remove a session whose creation did not complete; preserving its project directory", "error", err)
		}
	}()

	ref, err := admin.NewSession(ctx, gwclient.NewSessionRequest{
		Name:   th.sessionName(ctx),
		Cwd:    project.Dir,
		PiArgs: piArgs,
		Tags:   map[string]string{"platform": platformName, "thread": th.key, createAttemptTag: project.Dir},
	})
	if err != nil {
		// A connection can learn that it was rebound before a transport error
		// makes the command result ambiguous. If so, the deferred cleanup still
		// deletes that bound session. Otherwise use the creation tag to reconcile
		// the catalog; never remove a directory a possibly-live pi may still use.
		gatewaySessionCreated = admin.Session() != nil
		if !gatewaySessionCreated {
			var response *gwclient.ResponseError
			if !errors.As(err, &response) {
				created = true // a timed-out request may still be using this directory
				th.reconcileAmbiguousCreate(ctx, project.Dir)
			}
		}
		return fmt.Errorf("create the session: %w", err)
	}
	gatewaySessionCreated = true
	if ref == nil || ref.Path == "" {
		return errors.New("the daemon created a session without a path")
	}
	if th.b.cfg.ChatTools.Enabled {
		if err := th.verifyRecordedChatToolsSpawn(ctx, ref.Path); err != nil {
			return err
		}
		if err := verifyChatToolsExtension(ctx, admin, chattools.ExtensionVersion); err != nil {
			return err
		}
	}
	if err := th.recordCreatedSession(ctx, ref.Name, ref.Path, ref.ID, project.Dir, project.Dir); err != nil {
		return fmt.Errorf("record new session and chat-tools binding: %w", err)
	}
	th.log.Info("created session",
		"session", ref.Name, "path", ref.Path,
		"cwd", project.Dir, "project", project.Name(), "pi_args", piArgs)
	created = true
	return nil
}

// reconcilePendingCreate checks a project directory preserved after an
// ambiguous new-session response. The gateway's creation tag identifies the
// session while the daemon is running; exact name and cwd also recover it after
// a gateway restart drops in-memory creator tags.
func (th *thread) reconcilePendingCreate(ctx context.Context, attemptDir string) error {
	ctx, cancel := context.WithTimeout(ctx, gatewayTimeout)
	defer cancel()
	rows, err := th.b.gw.Sessions(ctx)
	if err != nil {
		return fmt.Errorf("reconcile the previous session creation: %w", err)
	}
	expectedName := th.sessionName(ctx)
	var found *gwclient.SessionRow
	for i := range rows {
		if !isCreateAttemptSession(rows[i], attemptDir, expectedName) {
			continue
		}
		if rows[i].Path == "" {
			return errors.New("the gateway catalog found an incomplete session from the previous create attempt")
		}
		if found != nil && found.Path != rows[i].Path {
			return errors.New("the gateway catalog found multiple sessions from the previous create attempt")
		}
		found = &rows[i]
	}
	if found != nil {
		if th.b.cfg.ChatTools.Enabled {
			if err := verifyChatToolsSpawnRecord(rows, found.Path, th.b.cfg.ChatTools.ExtensionPath); err != nil {
				return fmt.Errorf("verify the previous session's durable chat-tools configuration: %w", err)
			}
			if err := th.verifyRecoveredChatTools(ctx, found.Path); err != nil {
				return fmt.Errorf("verify the previous session's chat-tools extension: %w", err)
			}
		}
		if err := th.recordCreatedSession(ctx, found.Name, found.Path, found.ID, found.Cwd, attemptDir); err != nil {
			return fmt.Errorf("record the session from the previous create attempt: %w", err)
		}
		return nil
	}
	th.update(func(row *store.ThreadRow) {
		row.Cwd, row.ProjectDir = "", ""
	})
	return nil
}

// reconcileAmbiguousCreate records a matching gateway session as this thread's
// current session. If the catalog cannot prove the outcome, it stores the
// directory on the thread row so startup GC cannot delete a path a late Pi may
// still be using; the next create retries the catalog reconciliation first.
func (th *thread) reconcileAmbiguousCreate(ctx context.Context, attemptDir string) {
	cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), sessionCreateTimeout)
	defer cancel()
	if err := th.reconcilePendingCreate(cleanupCtx, attemptDir); err != nil {
		th.keepPendingProject(attemptDir)
		th.log.Warn("cannot reconcile an ambiguous session creation; preserving its project directory", "error", err)
		return
	}
	if th.snapshot().SessionPath == "" {
		th.keepPendingProject(attemptDir)
		th.log.Warn("the catalog cannot yet prove whether an ambiguous session creation succeeded; preserving its project directory",
			"attempt", attemptDir)
	}
}

// keepPendingProject leaves a durable owner for a directory until the gateway
// catalog can resolve an ambiguous create attempt.
func (th *thread) keepPendingProject(projectDir string) {
	th.update(func(row *store.ThreadRow) {
		if row.SessionPath == "" {
			row.Cwd, row.ProjectDir = projectDir, projectDir
		}
	})
}

func isCreateAttemptSession(row gwclient.SessionRow, attemptDir, expectedName string) bool {
	if row.CreatedBy != nil {
		if row.CreatedBy.Kind != kind {
			return false
		}
		if tag := row.CreatedBy.Tags[createAttemptTag]; tag != "" {
			return tag == attemptDir
		}
	}
	return row.Name == expectedName && row.Cwd == attemptDir
}

func (th *thread) verifyRecordedChatToolsSpawn(ctx context.Context, sessionPath string) error {
	rows, err := th.b.gw.Sessions(ctx)
	if err != nil {
		return fmt.Errorf("read the gateway catalog: %w", err)
	}
	return verifyChatToolsSpawnRecord(rows, sessionPath, th.b.cfg.ChatTools.ExtensionPath)
}

func verifyChatToolsSpawnRecord(rows []gwclient.SessionRow, sessionPath, extensionPath string) error {
	if sessionPath == "" || extensionPath == "" {
		return errors.New("the session path and extension path must be set")
	}
	for _, row := range rows {
		if row.Path != sessionPath {
			continue
		}
		for _, path := range row.Spawn["extension"] {
			if filepath.Clean(path) == filepath.Clean(extensionPath) {
				return nil
			}
		}
		return errors.New("the gateway catalog does not record the pi-chat extension for this session")
	}
	return errors.New("the gateway catalog does not contain the new session")
}

// verifyRecoveredChatTools checks both halves of a pending create before it is
// made durable as an active thread binding. SwitchSession applies the gateway's
// recorded spawn configuration if the Pi process had to restart.
func (th *thread) verifyRecoveredChatTools(ctx context.Context, sessionPath string) error {
	client, err := gwclient.Dial(ctx, gwclient.Config{
		StateDir:  th.b.cfg.Gateway.StateDir,
		TokenFile: th.b.cfg.Gateway.ThreadTokenFile,
		Name:      "pi-chat verify chat-tools",
		Kind:      kind,
	})
	if err != nil {
		return fmt.Errorf("open a verification connection: %w", err)
	}
	defer func() {
		if err := client.Close(); err != nil {
			th.log.Warn("cannot close chat-tools verification connection", "error", err)
		}
	}()
	if _, err := client.SwitchSession(ctx, sessionPath); err != nil {
		return fmt.Errorf("attach to the recovered session: %w", err)
	}
	return verifyChatToolsExtension(ctx, client, chattools.ExtensionVersion)
}

// recordCreatedSession commits a new or reconciled session and its optional
// tool binding as one durable owner record.
func (th *thread) recordCreatedSession(ctx context.Context, name, path, id, cwd, projectDir string) error {
	if path == "" {
		return errors.New("cannot record a session without a gateway path")
	}
	if name == "" {
		name = th.sessionName(ctx)
	}
	if cwd == "" {
		cwd = projectDir
	}
	row := th.snapshot()
	row.SessionName, row.SessionPath, row.SessionID = name, path, id
	row.Cwd, row.ProjectDir = cwd, projectDir
	row.State = store.StateCold
	row.Surface = th.t.SurfaceName()
	if th.b.cfg.ChatTools.Enabled {
		binding := store.SessionToolBinding{
			SessionPath:      path,
			Provider:         "slack",
			Surface:          th.t.SurfaceName(),
			WorkspaceID:      th.t.Workspace,
			ChannelID:        th.t.Channel,
			ThreadTS:         th.t.ThreadTS,
			Capabilities:     []string{"read_thread", "read_message", "read_user"},
			ExtensionVersion: chattools.ExtensionVersion,
			State:            store.ToolBindingActive,
			CreatedAt:        time.Now(),
		}
		storeCtx, storeCancel := context.WithTimeout(context.WithoutCancel(ctx), storeTimeout)
		defer storeCancel()
		if err := th.b.store.PutThreadWithToolBinding(storeCtx, row, binding, false); err != nil {
			return err
		}
		th.rowMu.Lock()
		th.row = row
		th.rowMu.Unlock()
		return nil
	}
	th.update(func(current *store.ThreadRow) { *current = row })
	return nil
}

func (th *thread) verifyActiveChatTools(ctx context.Context, client *gwclient.Client, sessionPath string) error {
	binding, found, err := th.b.store.SessionToolBinding(ctx, sessionPath)
	if err != nil {
		return fmt.Errorf("verify the session's chat-tools binding: %w", err)
	}
	if !found || binding.State != store.ToolBindingActive {
		return nil // adopted or revoked sessions remain usable without chat tools
	}
	if binding.Provider != "slack" || binding.Surface != th.t.SurfaceName() ||
		binding.WorkspaceID != th.t.Workspace || binding.ChannelID != th.t.Channel || binding.ThreadTS != th.t.ThreadTS {
		return errors.New("the session's chat-tools binding belongs to a different Slack thread or surface")
	}
	return verifyChatToolsExtension(ctx, client, binding.ExtensionVersion)
}

func verifyChatToolsExtension(ctx context.Context, client *gwclient.Client, version string) error {
	commands, err := client.GetCommands(ctx)
	if err != nil {
		return fmt.Errorf("verify the chat-tools extension in the session: %w", err)
	}
	if !chatToolsExtensionLoaded(commands, version) {
		return errors.New("the pi-chat chat-tools extension is not loaded in the session; refusing to send a prompt")
	}
	return nil
}

func chatToolsExtensionLoaded(commands []gwclient.Command, version string) bool {
	if version != chattools.ExtensionVersion {
		return false
	}
	for _, command := range commands {
		if command.Name == chattools.HealthCommand && command.Source == "extension" &&
			command.Description == "pi-chat-tools:"+version {
			return true
		}
	}
	return false
}

// sessionName is the name a new session gets: the thread's deterministic name,
// with a generation suffix once the thread has had one before.
//
// `/pi new` replaces a thread's session without changing the thread, and two
// sessions of one thread under one name would leave the catalog ambiguous — for
// `/pi resume`, and for anyone reading it. The suffix is the count of sessions
// this thread has retired, so a rebuilt database lands on the same names.
func (th *thread) sessionName(ctx context.Context) string {
	base := th.t.SessionName()
	n, err := th.b.store.RetiredCount(ctx, th.key)
	if err != nil {
		th.log.Warn("cannot count the thread's retired sessions; naming this one as the first",
			"error", err)
		return base
	}
	if n == 0 {
		return base
	}
	return fmt.Sprintf("%s-%d", base, n+1)
}

// retire lets go of the thread's session because a new one is replacing it, and
// records that the old one is still there to be resumed.
//
// It is deliberately not a delete: the session file, its working directory and
// its pi process are all left where they are, because "new session" means a
// clean slate rather than "throw my work away" — that is what `/pi delete` is
// for, with its confirmation button (DESIGN.md §4).
//
// The caller holds turnMu, or has established that no turn is running: a session
// must not be retired under the turn that is using it.
//
// It reports whether there was a session to retire.
func (th *thread) retire(ctx context.Context) (bool, error) {
	row := th.snapshot()
	if row.SessionPath == "" {
		return false, nil
	}

	// Let go of the connection: the thread must not stay attached to a session it
	// no longer owns, and closing saves the cursor, so resuming the old session
	// later continues where this thread stopped rather than replaying it.
	th.sessionEpoch.Add(1)
	th.connMu.Lock()
	client := th.client
	th.client = nil
	th.connMu.Unlock()
	if client != nil {
		th.saveCursor(ctx, client)
		_ = client.Close()
	}
	row = th.snapshot() // saveCursor may have advanced the replay cursor

	ctx, cancel := context.WithTimeout(ctx, storeTimeout)
	defer cancel()
	if err := th.b.store.RetireSession(ctx, store.RetiredSession{
		SessionPath: row.SessionPath,
		ThreadKey:   th.key,
		SessionName: row.SessionName,
		ProjectDir:  row.ProjectDir,
		ObservedTS:  row.ObservedTS,
		LastSeq:     row.LastSeq,
		LeafID:      row.LeafID,
		RetiredAt:   time.Now(),
	}); err != nil {
		return false, err
	}

	th.forgetAssistant()
	th.update(func(r *store.ThreadRow) {
		r.SessionName, r.SessionPath, r.SessionID = "", "", ""
		r.Cwd, r.ProjectDir = "", ""
		r.LastSeq, r.LeafID, r.ProgressTS = 0, "", ""
		// A new session starts with its own conversation: the messages the old one
		// read are in its history, not in this one's.
		r.ObservedTS = ""
		r.State = store.StateCold
	})
	th.log.Info("retired the thread's session; it stays resumable",
		"session", row.SessionName, "path", row.SessionPath)
	return true, nil
}

// deleteReport is what a completed deletion did, for the answer and the log.
type deleteReport struct {
	// AlreadyGone records that the gateway did not have the session any more:
	// there was nothing to delete, but the rest of the work was still worth doing.
	AlreadyGone bool
	// Cleanup is what the provisioner removed, and what it deliberately did not.
	Cleanup workspace.CleanupResult
	// CleanupErr is a cleanup that did not finish. The session is gone by then, so
	// it is reported rather than retried.
	CleanupErr error
}

// destroy deletes the session this thread owns and what pi-chat can prove it put
// on disk.
//
// An error means nothing was deleted: the gateway refused, so the session, its
// directory and the row are exactly as they were and the user can ask again.
// Once the session is gone the row is marked deleted even when the cleanup
// reports trouble, because a row pointing at a session that no longer exists is
// worse than a directory that outlives its session — and the leftovers are what
// the answer is for (DESIGN.md §4, §9).
//
// The caller holds turnMu, so no turn is running on the session being deleted.
func (th *thread) destroy(ctx context.Context) (deleteReport, error) {
	row := th.snapshot()
	var report deleteReport

	// The connection goes first. A delete unbinds attached clients rather than
	// disconnecting them, so leaving this one open would keep a client of a
	// session that is going away, receiving the events of a session this thread no
	// longer owns.
	th.sessionEpoch.Add(1)
	th.connMu.Lock()
	client := th.client
	th.client = nil
	th.connMu.Unlock()
	if client != nil {
		_ = client.Close()
	}

	if row.SessionPath != "" {
		switch err := th.b.life.DeleteSession(ctx, row.SessionPath); {
		case err == nil:
			th.log.Info("deleted the session",
				"session", row.SessionName, "path", row.SessionPath)
		case isGone(err):
			report.AlreadyGone = true
			th.log.Info("the session was already gone; cleaning up what is left",
				"session", row.SessionName, "path", row.SessionPath)
		default:
			return report, fmt.Errorf("delete the session: %w", err)
		}
		// Revoke only after the gateway confirms the session is gone. A refused
		// delete leaves the live session and its binding usable. If persistence
		// fails after deletion, still clear the row: the session cannot issue
		// tools any more, and a repeated delete can retry the revocation.
		if err := th.b.store.RevokeSessionToolBinding(ctx, row.SessionPath); err != nil {
			th.log.Error("cannot revoke the deleted session's chat-tools binding", "path", row.SessionPath, "error", err)
		}
	}

	if row.ProjectDir != "" {
		var err error
		report.Cleanup, err = th.b.work.Cleanup(ctx, row.ProjectDir)
		if err != nil {
			report.CleanupErr = err
			th.log.Warn("the session is gone, but cleaning up its directory failed",
				"dir", row.ProjectDir, "error", err)
		}
	}

	th.update(func(r *store.ThreadRow) {
		r.State = store.StateDeleted
		r.SessionName, r.SessionPath, r.SessionID = "", "", ""
		r.Cwd, r.ProjectDir = "", ""
		r.LastSeq, r.LeafID, r.ProgressTS = 0, "", ""
		// A session that follows in this thread starts its own conversation: what
		// the deleted one read is in its history, not in the next one's.
		r.ObservedTS = ""
	})
	return report, nil
}

// dial opens the thread's long-lived connection. Events arrive on the
// connection's read goroutine through onEvent, which must never block.
//
// The caller holds connMu.
func (th *thread) dial(ctx context.Context) (*gwclient.Client, error) {
	return th.dialConfigured(ctx, "", th.snapshot().SessionPath, true)
}

// dialSession attaches in the handshake without replaying the event stream;
// response checks read session entries directly. The caller holds connMu.
func (th *thread) dialSession(ctx context.Context, sessionPath string) (*gwclient.Client, error) {
	return th.dialConfigured(ctx, sessionPath, sessionPath, false)
}

// dialConfigured separates the optional handshake attachment from the session
// identity used to reject stale events on an unbound connection.
func (th *thread) dialConfigured(ctx context.Context, attachPath, eventPath string, resume bool) (*gwclient.Client, error) {
	row := th.snapshot()
	var eventClient atomic.Pointer[gwclient.Client]
	epoch := th.sessionEpoch.Load()
	cfg := gwclient.Config{
		StateDir:  th.b.cfg.Gateway.StateDir,
		TokenFile: th.b.cfg.Gateway.ThreadTokenFile,
		Name:      "pi-chat " + th.t.SessionName(),
		Kind:      kind,
		Tags:      map[string]string{"platform": platformName, "thread": th.key},
		Session:   attachPath,
		OnEvent: func(ev gwclient.Event) {
			th.onSessionEventWithClient(eventPath, ev, eventClient.Load(), epoch)
		},
	}
	// A previously driven thread resumes from its cursor for live sequence
	// tracking. Frames outside an observed turn are discarded; an unbound dial
	// then attaches with SwitchSession, which does not promise replay of the cold
	// interval. The handshake-attached poller reads durable session entries.
	if resume && (row.LastSeq > 0 || row.LeafID != "") {
		cfg.Resume = &protocol.Resume{SinceSeq: row.LastSeq, LeafEntryID: row.LeafID}
	} else {
		cfg.LiveOnly = true
	}
	client, err := gwclient.Dial(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("open the thread connection: %w", err)
	}
	eventClient.Store(client)
	return client, nil
}

// saveCursorAfterActivity records both the live event sequence and the durable
// session leaf after a turn, so a later cold poll starts after already handled
// assistant output. Live event frames advance sequence numbers but do not carry
// a durable leaf, so GetEntries refreshes that boundary first.
func (th *thread) saveCursorAfterActivity(ctx context.Context, client *gwclient.Client) {
	if client == nil {
		return
	}
	row := th.snapshot()
	queryCtx, queryCancel := context.WithTimeout(ctx, gatewayTimeout)
	page, err := client.GetEntries(queryCtx, row.LeafID)
	queryCancel()
	if err != nil {
		th.log.Debug("cannot refresh the session entry cursor", "error", err)
		th.saveCursor(ctx, client)
		return
	}
	th.saveCursorAt(ctx, client.LastSeq(), page.LeafID)
}

// saveCursor records how much of the session's event stream the connection has
// consumed, so the next attach resumes from there.
func (th *thread) saveCursor(ctx context.Context, client *gwclient.Client) {
	leaf := client.LeafID()
	if leaf == "" {
		queryCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), cursorTimeout)
		_, err := client.GetEntries(queryCtx, "")
		cancel()
		if err != nil {
			th.log.Debug("cannot initialize the session leaf cursor", "error", err)
		}
	}
	cursor := client.Cursor()
	th.saveCursorAt(ctx, cursor.SinceSeq, cursor.LeafEntryID)
}

func (th *thread) saveCursorAt(ctx context.Context, seq uint64, leaf string) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), cursorTimeout)
	defer cancel()
	if err := th.b.store.SetThreadCursor(ctx, th.key, seq, leaf); err != nil {
		th.log.Warn("cannot save the replay cursor", "error", err)
		return
	}
	th.update(func(row *store.ThreadRow) {
		row.LastSeq, row.LeafID = seq, leaf
	})
}

// noteProgress remembers the message a turn is writing into, so a restart knows
// which reply it left half-written (DESIGN.md §7).
func (th *thread) noteProgress(ctx context.Context, r Renderer) {
	reporter, ok := r.(ProgressReporter)
	if !ok {
		return
	}
	ts := reporter.Progress()
	if ts == "" {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, storeTimeout)
	defer cancel()
	if err := th.b.store.SetProgressTS(ctx, th.key, ts); err != nil {
		th.log.Warn("cannot record the reply handle", "error", err)
		return
	}
	th.update(func(row *store.ThreadRow) { row.ProgressTS = ts })
}

// close ends the thread's connection and leaves the session alone: the session
// file is what makes the thread resumable, and the daemon hibernates an idle
// pi process on its own.
//
// The caller holds turnMu (or has established that no turn is running), so no
// turn is cut off by this.
func (th *thread) close(ctx context.Context) {
	th.connMu.Lock()
	client := th.client
	th.client = nil
	th.connMu.Unlock()
	if client == nil {
		return
	}
	// Save the cursor before letting go, so reopening resumes where this
	// connection stopped rather than replaying from the head.
	th.saveCursor(ctx, client)
	byeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), cursorTimeout)
	defer cancel()
	_ = client.Bye(byeCtx)
	_ = client.Close()
	th.update(func(row *store.ThreadRow) { row.State = store.StateCold })
	th.log.Info("closed the thread connection; the session stays")
}
