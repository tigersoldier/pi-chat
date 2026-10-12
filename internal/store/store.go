// Package store is pi-chat's durable chat-side state (DESIGN.md §7).
//
// The gateway owns sessions, pi processes and the session catalog. This
// database owns the other half: which chat thread maps onto which session,
// where each thread's replay cursor sits, which inbound events have already
// been handled, and the dialogs and queued messages that have to survive a
// restart.
//
// It holds **no conversation content** — no transcripts, no assistant text, no
// tool output. Every column is an identifier, a path, a state name or a
// timestamp, plus the pending prompt text in `admissions` (which lives only
// until it is dispatched). That is what the privacy claim in DESIGN.md §7
// rests on, so it has to stay true.
//
// SQLite in WAL mode with one writer connection. The load is a handful of
// rows per turn, so serializing access costs nothing and removes a whole class
// of `SQLITE_BUSY` failure.
package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	_ "modernc.org/sqlite" // pure-Go driver: no cgo, so the unit stays static
)

// schemaVersion is bumped when the schema changes in a way an older binary
// cannot cope with. A database from a newer pi-chat is refused rather than
// silently misread.
const schemaVersion = 8

// schema is the whole database. `pending_ui` and `admissions` are created here
// but only written from phase 2 on: the schema is one artifact, and creating
// the tables now means the feature that needs them is not also a migration.
const schema = `
CREATE TABLE IF NOT EXISTS threads (
    thread_key   TEXT PRIMARY KEY,          -- workspace:channel:thread_ts
    workspace_id TEXT NOT NULL,
    channel_id   TEXT NOT NULL,
    thread_ts    TEXT NOT NULL,
    surface      TEXT NOT NULL DEFAULT 'app',
    session_name TEXT NOT NULL DEFAULT '',
    session_path TEXT NOT NULL DEFAULT '',
    session_id   TEXT NOT NULL DEFAULT '',
    cwd          TEXT NOT NULL DEFAULT '',
    project_dir  TEXT NOT NULL DEFAULT '',
    state        TEXT NOT NULL DEFAULT 'cold',   -- warm | cold | deleted
    last_seq     INTEGER NOT NULL DEFAULT 0,     -- replay cursor
    leaf_id      TEXT NOT NULL DEFAULT '',
    progress_ts  TEXT NOT NULL DEFAULT '',       -- reply being streamed/patched
    observed_ts  TEXT NOT NULL DEFAULT '',       -- observation watermark
    created_at   INTEGER NOT NULL,
    last_active  INTEGER NOT NULL
);

CREATE INDEX IF NOT EXISTS threads_session_path ON threads(session_path);
CREATE INDEX IF NOT EXISTS threads_channel ON threads(workspace_id, channel_id);
CREATE INDEX IF NOT EXISTS threads_project_dir ON threads(project_dir);

CREATE TABLE IF NOT EXISTS session_tool_bindings (
    session_path      TEXT PRIMARY KEY,
    provider          TEXT NOT NULL,
    surface           TEXT NOT NULL,
    workspace_id      TEXT NOT NULL,
    channel_id        TEXT NOT NULL,
    thread_ts         TEXT NOT NULL,
    capabilities      TEXT NOT NULL,
    extension_version TEXT NOT NULL,
    state             TEXT NOT NULL CHECK (state IN ('active', 'revoked')),
    created_at        INTEGER NOT NULL
);

CREATE INDEX IF NOT EXISTS session_tool_bindings_state ON session_tool_bindings(state);

CREATE TABLE IF NOT EXISTS seen (
    event_id    TEXT PRIMARY KEY,           -- platform event ID, dedupe key
    received_at INTEGER NOT NULL
);

CREATE TABLE IF NOT EXISTS pending_ui (
    dialog_id  TEXT PRIMARY KEY,
    thread_key TEXT NOT NULL,
    message_ts TEXT NOT NULL DEFAULT '',
    created_at INTEGER NOT NULL
);

CREATE TABLE IF NOT EXISTS admissions (
    id         INTEGER PRIMARY KEY AUTOINCREMENT,
    thread_key TEXT NOT NULL,
    text       TEXT NOT NULL,
    event_id   TEXT NOT NULL,
    queued_at  INTEGER NOT NULL
);

-- Sessions /pi new replaced but did not delete: they stay resumable, so their
-- working directories have to survive the sweep that removes the ones no row
-- accounts for (DESIGN.md §4, §9).
CREATE TABLE IF NOT EXISTS retired (
    session_path TEXT PRIMARY KEY,
    thread_key   TEXT NOT NULL,
    session_name TEXT NOT NULL DEFAULT '',
    project_dir  TEXT NOT NULL DEFAULT '',
    observed_ts  TEXT NOT NULL DEFAULT '',
    last_seq     INTEGER NOT NULL DEFAULT 0,
    leaf_id      TEXT NOT NULL DEFAULT '',
    retired_at   INTEGER NOT NULL
);

CREATE INDEX IF NOT EXISTS retired_thread ON retired(thread_key);

-- Sessions pi-chat notified during a clean shutdown, pending a response check.
CREATE TABLE IF NOT EXISTS shutdown_pending (
    thread_key       TEXT PRIMARY KEY,
    session_path     TEXT NOT NULL,
    leaf_id          TEXT NOT NULL DEFAULT '',
    notice_id        TEXT NOT NULL DEFAULT '',
    notice_entry_id  TEXT NOT NULL DEFAULT '',
    posted           INTEGER NOT NULL DEFAULT 0 CHECK (posted IN (0, 1)),
    created_at       INTEGER NOT NULL
);

-- A surface's own bookkeeping, for an adapter that has no threads to hang it on
-- (see surface.go). Not chat-shaped, on purpose: it holds a cursor and a list of
-- messages the daemon posted, never conversation content.
CREATE TABLE IF NOT EXISTS surface_kv (
    surface    TEXT NOT NULL,
    key        TEXT NOT NULL,
    value      TEXT NOT NULL DEFAULT '',
    updated_at INTEGER NOT NULL,
    PRIMARY KEY (surface, key)
);

CREATE TABLE IF NOT EXISTS surface_posted (
    surface   TEXT NOT NULL,
    ts        TEXT NOT NULL,
    posted_at INTEGER NOT NULL,
    PRIMARY KEY (surface, ts)
);

CREATE INDEX IF NOT EXISTS surface_posted_at ON surface_posted(posted_at);
`

// migrations bring an older database up to schemaVersion, one step at a time.
// A fresh database skips them: `schema` is written whole, so it already carries
// every column a step would add.
//
// A step stamps its version only after all of its statements succeed, so a
// failure leaves the database at the version the next start will retry rather
// than half-migrated and claiming to be current.
var migrations = []struct {
	to   int
	stmt []string
}{
	{to: 2, stmt: []string{
		// The observation watermark (DESIGN.md §4): the newest message of the
		// thread's conversation that a prompt has already carried, so the next
		// turn fetches what came after it and nothing twice.
		`ALTER TABLE threads ADD COLUMN observed_ts TEXT NOT NULL DEFAULT ''`,
	}},
	{to: 3, stmt: []string{
		// Sessions a new one replaced, kept so their working directories survive
		// the startup sweep (DESIGN.md §4, §9).
		`CREATE TABLE IF NOT EXISTS retired (
            session_path TEXT PRIMARY KEY,
            thread_key   TEXT NOT NULL,
            session_name TEXT NOT NULL DEFAULT '',
            project_dir  TEXT NOT NULL DEFAULT '',
            retired_at   INTEGER NOT NULL
        )`,
		`CREATE INDEX IF NOT EXISTS retired_thread ON retired(thread_key)`,
	}},
	{to: 4, stmt: []string{
		// The self-DM surface's cursor and posted ledger (DESIGN.md §12). A
		// migration rather than only an entry in the schema so an install that
		// turns the surface on later does not have to recreate its database.
		`CREATE TABLE IF NOT EXISTS surface_kv (
            surface    TEXT NOT NULL,
            key        TEXT NOT NULL,
            value      TEXT NOT NULL DEFAULT '',
            updated_at INTEGER NOT NULL,
            PRIMARY KEY (surface, key)
        )`,
		`CREATE TABLE IF NOT EXISTS surface_posted (
            surface   TEXT NOT NULL,
            ts        TEXT NOT NULL,
            posted_at INTEGER NOT NULL,
            PRIMARY KEY (surface, ts)
        )`,
		`CREATE INDEX IF NOT EXISTS surface_posted_at ON surface_posted(posted_at)`,
	}},
	{to: 5, stmt: []string{
		`ALTER TABLE threads ADD COLUMN surface TEXT NOT NULL DEFAULT 'app'`,
		`CREATE TABLE IF NOT EXISTS session_tool_bindings (
            session_path      TEXT PRIMARY KEY,
            provider          TEXT NOT NULL,
            surface           TEXT NOT NULL,
            workspace_id      TEXT NOT NULL,
            channel_id        TEXT NOT NULL,
            thread_ts         TEXT NOT NULL,
            capabilities      TEXT NOT NULL,
            extension_version TEXT NOT NULL,
            state             TEXT NOT NULL CHECK (state IN ('active', 'revoked')),
            created_at        INTEGER NOT NULL
        )`,
		`CREATE INDEX IF NOT EXISTS session_tool_bindings_state ON session_tool_bindings(state)`,
	}},
	{to: 6, stmt: []string{
		`ALTER TABLE retired ADD COLUMN observed_ts TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE retired ADD COLUMN last_seq INTEGER NOT NULL DEFAULT 0`,
		`ALTER TABLE retired ADD COLUMN leaf_id TEXT NOT NULL DEFAULT ''`,
	}},
	{to: 7, stmt: []string{
		`CREATE TABLE IF NOT EXISTS shutdown_pending (
            thread_key   TEXT PRIMARY KEY,
            session_path TEXT NOT NULL,
            leaf_id      TEXT NOT NULL DEFAULT '',
            created_at   INTEGER NOT NULL
        )`,
	}},
	{to: 8, stmt: []string{
		`ALTER TABLE shutdown_pending ADD COLUMN notice_id TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE shutdown_pending ADD COLUMN notice_entry_id TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE shutdown_pending ADD COLUMN posted INTEGER NOT NULL DEFAULT 0 CHECK (posted IN (0, 1))`,
	}},
}

// Store is the open database.
type Store struct {
	db *sql.DB
}

// Open opens (and on first use creates) the database at path, applying the
// schema. The parent directory is created when missing.
func Open(ctx context.Context, path string) (*Store, error) {
	if path == "" {
		return nil, errors.New("store: no database path configured")
	}
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return nil, fmt.Errorf("store: create %s: %w", dir, err)
		}
	}

	// Pragmas go in the DSN so the driver applies them to every connection it
	// opens, not just the first: busy_timeout and foreign_keys are
	// per-connection settings that a one-off PRAGMA would leave unset on the
	// rest of the pool.
	dsn := "file:" + path + "?_pragma=busy_timeout(5000)" +
		"&_pragma=journal_mode(WAL)" +
		"&_pragma=synchronous(NORMAL)" +
		"&_pragma=foreign_keys(ON)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("store: open %s: %w", path, err)
	}
	// One writer connection: see the package comment.
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)

	s := &Store{db: db}
	if err := s.init(ctx); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("store: %s: %w", path, err)
	}
	return s, nil
}

// init creates the schema, migrates an older one, and checks the version.
func (s *Store) init(ctx context.Context) error {
	var version int
	if err := s.db.QueryRowContext(ctx, "PRAGMA user_version").Scan(&version); err != nil {
		return fmt.Errorf("read the schema version: %w", err)
	}
	switch {
	case version > schemaVersion:
		return fmt.Errorf("the database was written by a newer pi-chat (schema %d, this build understands %d)",
			version, schemaVersion)
	case version == schemaVersion:
		return nil
	}

	// user_version alone cannot tell a fresh database from one written before
	// the version was stamped, and only one of them needs migrating. The tables
	// decide.
	existing, err := s.tableExists(ctx, "threads")
	if err != nil {
		return err
	}
	if existing {
		if err := s.migrate(ctx, version); err != nil {
			return err
		}
	}
	// Every statement in the schema is IF NOT EXISTS, so running it after a
	// migration creates whatever a migration did not — a table a later version
	// added — and leaves existing tables alone. Keep the whole schema and its
	// version stamp in one transaction so a fresh database can retry after an
	// interrupted first open.
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin applying the schema: %w", err)
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, schema); err != nil {
		return fmt.Errorf("apply the schema: %w", err)
	}
	if err := recordVersion(ctx, tx, schemaVersion); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit the schema: %w", err)
	}
	return nil
}

// migrate applies every migration newer than the database's version.
func (s *Store) migrate(ctx context.Context, from int) error {
	for _, m := range migrations {
		if m.to <= from {
			continue
		}
		tx, err := s.db.BeginTx(ctx, nil)
		if err != nil {
			return fmt.Errorf("begin migration to schema %d: %w", m.to, err)
		}
		for _, stmt := range m.stmt {
			if _, err := tx.ExecContext(ctx, stmt); err != nil {
				_ = tx.Rollback()
				return fmt.Errorf("migrate the schema to %d: %w", m.to, err)
			}
		}
		if err := recordVersion(ctx, tx, m.to); err != nil {
			_ = tx.Rollback()
			return err
		}
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("commit migration to schema %d: %w", m.to, err)
		}
	}
	return nil
}

// versionStatements is every schema version this build can stamp, as statements
// rather than as formats.
//
// SQLite does not accept a bound parameter as a PRAGMA value (`PRAGMA
// user_version = ?` is a syntax error), so the version has to reach the
// statement as text. It reaches it as one of these literals and never as
// something assembled, which is what keeps the one interpolated-looking corner
// of this package reviewable. TestVersionStatementsCoverEverySchema fails when a
// new schema version forgets its entry.
var versionStatements = map[int]string{
	1: "PRAGMA user_version = 1",
	2: "PRAGMA user_version = 2",
	3: "PRAGMA user_version = 3",
	4: "PRAGMA user_version = 4",
	5: "PRAGMA user_version = 5",
	6: "PRAGMA user_version = 6",
	7: "PRAGMA user_version = 7",
	8: "PRAGMA user_version = 8",
}

// recordVersion stamps the schema version on the same executor as the schema
// changes, so a migration and its version cannot be committed separately.
func recordVersion(ctx context.Context, exec execContext, version int) error {
	stmt, ok := versionStatements[version]
	if !ok {
		return fmt.Errorf("no statement records schema version %d", version)
	}
	if _, err := exec.ExecContext(ctx, stmt); err != nil {
		return fmt.Errorf("record the schema version: %w", err)
	}
	return nil
}

// tableExists reports whether a table is part of the schema.
func (s *Store) tableExists(ctx context.Context, name string) (bool, error) {
	var found string
	err := s.db.QueryRowContext(ctx,
		`SELECT name FROM sqlite_master WHERE type = 'table' AND name = ?`, name).Scan(&found)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return false, nil
	case err != nil:
		return false, fmt.Errorf("look for the %s table: %w", name, err)
	}
	return true, nil
}

// Close closes the database.
func (s *Store) Close() error {
	if s == nil || s.db == nil {
		return nil
	}
	return s.db.Close()
}
