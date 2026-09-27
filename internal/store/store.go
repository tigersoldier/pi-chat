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
const schemaVersion = 1

// schema is the whole database. `pending_ui` and `admissions` are created here
// but only written from phase 2 on: the schema is one artifact, and creating
// the tables now means the feature that needs them is not also a migration.
const schema = `
CREATE TABLE IF NOT EXISTS threads (
    thread_key   TEXT PRIMARY KEY,          -- workspace:channel:thread_ts
    workspace_id TEXT NOT NULL,
    channel_id   TEXT NOT NULL,
    thread_ts    TEXT NOT NULL,
    session_name TEXT NOT NULL DEFAULT '',
    session_path TEXT NOT NULL DEFAULT '',
    session_id   TEXT NOT NULL DEFAULT '',
    cwd          TEXT NOT NULL DEFAULT '',
    project_dir  TEXT NOT NULL DEFAULT '',
    state        TEXT NOT NULL DEFAULT 'cold',   -- warm | cold | deleted
    last_seq     INTEGER NOT NULL DEFAULT 0,     -- replay cursor
    leaf_id      TEXT NOT NULL DEFAULT '',
    progress_ts  TEXT NOT NULL DEFAULT '',       -- reply being streamed/patched
    created_at   INTEGER NOT NULL,
    last_active  INTEGER NOT NULL
);

CREATE INDEX IF NOT EXISTS threads_session_path ON threads(session_path);
CREATE INDEX IF NOT EXISTS threads_channel ON threads(workspace_id, channel_id);
CREATE INDEX IF NOT EXISTS threads_project_dir ON threads(project_dir);

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
`

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

// init creates the schema and checks the version.
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
	if _, err := s.db.ExecContext(ctx, schema); err != nil {
		return fmt.Errorf("apply the schema: %w", err)
	}
	// user_version does not accept a bound parameter.
	if _, err := s.db.ExecContext(ctx, fmt.Sprintf("PRAGMA user_version = %d", schemaVersion)); err != nil {
		return fmt.Errorf("record the schema version: %w", err)
	}
	return nil
}

// Close closes the database.
func (s *Store) Close() error {
	if s == nil || s.db == nil {
		return nil
	}
	return s.db.Close()
}
