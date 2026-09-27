package bot

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/tigersoldier/pi-gateway/gwclient"
	"github.com/tigersoldier/pi-gateway/protocol"

	"github.com/tigersoldier/pi-chat/internal/store"
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

	// rowMu guards the durable row. It is a second, narrower lock than connMu
	// on purpose: the row is written from the turn path, the idle sweep and
	// the cursor saver, and those must not queue behind a dial.
	rowMu sync.Mutex
	row   store.ThreadRow

	// cur is the turn the connection's events belong to, if any. It is
	// atomically swapped because the gateway's read goroutine reads it while
	// the turn path writes it.
	cur atomic.Pointer[turnState]
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
func (th *thread) forgetSession() {
	th.connMu.Lock()
	client := th.client
	th.client = nil
	th.connMu.Unlock()
	if client != nil {
		_ = client.Close()
	}
	th.forgetSessionLocked()
}

// forgetSessionLocked records that the session is gone. The caller holds connMu
// and has already cleared th.client.
//
// The key stays reserved and the working directory is left alone — the session
// file is what was deleted, and anything the agent wrote is still on disk — but
// the thread stops owning a session, so the next message starts a fresh one
// deliberately instead of re-attaching a tombstone.
func (th *thread) forgetSessionLocked() {
	th.update(func(row *store.ThreadRow) {
		row.State = store.StateDeleted
		row.SessionName, row.SessionPath, row.SessionID = "", "", ""
		row.LastSeq, row.LeafID, row.ProgressTS = 0, "", ""
	})
	th.log.Warn("the session was deleted outside pi-chat", "thread", th.key)
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
			th.forgetSessionLocked()
			return nil, fmt.Errorf("%w: %s", errSessionGone, path)
		}
		return nil, fmt.Errorf("bind session %s: %w", path, err)
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

	ref, err := admin.NewSession(ctx, gwclient.NewSessionRequest{
		Name:   th.t.SessionName(),
		Cwd:    project.Dir,
		PiArgs: piArgs,
		Tags:   map[string]string{"platform": platformName, "thread": th.key},
	})
	if err != nil {
		return fmt.Errorf("create the session: %w", err)
	}
	if ref == nil || ref.Path == "" {
		return errors.New("the daemon created a session without a path")
	}
	th.update(func(row *store.ThreadRow) {
		row.SessionName, row.SessionPath, row.SessionID = ref.Name, ref.Path, ref.ID
		row.Cwd, row.ProjectDir = project.Dir, project.Dir
	})
	th.log.Info("created session",
		"session", ref.Name, "path", ref.Path,
		"cwd", project.Dir, "project", project.Name(), "pi_args", piArgs)
	created = true
	return nil
}

// dial opens the thread's long-lived connection. Events arrive on the
// connection's read goroutine through onEvent, which must never block.
//
// The caller holds connMu.
func (th *thread) dial(ctx context.Context) (*gwclient.Client, error) {
	cfg := gwclient.Config{
		StateDir:  th.b.cfg.Gateway.StateDir,
		TokenFile: th.b.cfg.Gateway.ThreadTokenFile,
		Name:      "pi-chat " + th.t.SessionName(),
		Kind:      kind,
		Tags:      map[string]string{"platform": platformName, "thread": th.key},
		OnEvent:   th.onEvent,
	}
	// A thread that has been driven before resumes from its cursor, so the
	// events of an idle window (or of a restart) are not replayed into a
	// thread that already saw them. The replayed frames are discarded by
	// onEvent, which only renders inside a turn, and the watermark they carry
	// is what the next save records. A first attach has nothing to resume, so
	// it starts at the head (DESIGN.md §4).
	if row := th.snapshot(); row.LastSeq > 0 || row.LeafID != "" {
		cfg.Resume = &protocol.Resume{SinceSeq: row.LastSeq, LeafEntryID: row.LeafID}
	} else {
		cfg.LiveOnly = true
	}
	client, err := gwclient.Dial(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("open the thread connection: %w", err)
	}
	return client, nil
}

// saveCursor records how much of the session's event stream the connection has
// consumed, so the next attach resumes from there.
func (th *thread) saveCursor(ctx context.Context, client *gwclient.Client) {
	seq, leaf := client.LastSeq(), client.LeafID()
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
