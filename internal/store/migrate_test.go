package store

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"testing"
)

// schemaV1 is the schema as version 1 wrote it: the threads table without the
// observation watermark. A migration test needs a database from before the
// change, and the only honest way to have one is to write it out.
const schemaV1 = `
CREATE TABLE IF NOT EXISTS threads (
    thread_key   TEXT PRIMARY KEY,
    workspace_id TEXT NOT NULL,
    channel_id   TEXT NOT NULL,
    thread_ts    TEXT NOT NULL,
    session_name TEXT NOT NULL DEFAULT '',
    session_path TEXT NOT NULL DEFAULT '',
    session_id   TEXT NOT NULL DEFAULT '',
    cwd          TEXT NOT NULL DEFAULT '',
    project_dir  TEXT NOT NULL DEFAULT '',
    state        TEXT NOT NULL DEFAULT 'cold',
    last_seq     INTEGER NOT NULL DEFAULT 0,
    leaf_id      TEXT NOT NULL DEFAULT '',
    progress_ts  TEXT NOT NULL DEFAULT '',
    created_at   INTEGER NOT NULL,
    last_active  INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS seen (
    event_id    TEXT PRIMARY KEY,
    received_at INTEGER NOT NULL
);
`

// writeV1Database creates a database as the previous schema version left it,
// with one thread in it, and reports its path.
func writeV1Database(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "state", "pi-chat.db")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatalf("create the directory: %v", err)
	}
	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer db.Close()
	if _, err := db.Exec(schemaV1); err != nil {
		t.Fatalf("write the v1 schema: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO threads
        (thread_key, workspace_id, channel_id, thread_ts, session_name, session_path,
         state, created_at, last_active)
        VALUES ('T1:C1:1.1', 'T1', 'C1', '1.1', 'slack-t1-c1-1-1', '/s/a.jsonl',
                'cold', 1700000000, 1700000000)`); err != nil {
		t.Fatalf("write a v1 row: %v", err)
	}
	if _, err := db.Exec("PRAGMA user_version = 1"); err != nil {
		t.Fatalf("stamp version 1: %v", err)
	}
	return path
}

// TestMigratesAVersionOneDatabase is the migration path's whole job: a database
// written by the previous build opens, keeps its rows, and gains the column the
// new one reads.
func TestMigratesAVersionOneDatabase(t *testing.T) {
	ctx := context.Background()
	path := writeV1Database(t)

	s, err := Open(ctx, path)
	if err != nil {
		t.Fatalf("Open a v1 database: %v", err)
	}
	defer s.Close()

	var version int
	if err := s.db.QueryRowContext(ctx, "PRAGMA user_version").Scan(&version); err != nil {
		t.Fatalf("read the version: %v", err)
	}
	if version != schemaVersion {
		t.Errorf("version after the migration = %d, want %d", version, schemaVersion)
	}

	row, found, err := s.Thread(ctx, "T1:C1:1.1")
	if err != nil {
		t.Fatalf("Thread: %v", err)
	}
	if !found {
		t.Fatal("the migration lost the row that was already there")
	}
	if row.ObservedTS != "" {
		t.Errorf("ObservedTS of a migrated row = %q, want empty", row.ObservedTS)
	}
	if row.SessionPath != "/s/a.jsonl" {
		t.Errorf("the migration changed the session path: %q", row.SessionPath)
	}

	// The point of the column is that the new build can write it.
	if err := s.SetThreadObservedTS(ctx, row.ThreadKey, "1700000000.000500"); err != nil {
		t.Fatalf("SetThreadObservedTS: %v", err)
	}
	row, _, err = s.Thread(ctx, row.ThreadKey)
	if err != nil {
		t.Fatalf("Thread after the write: %v", err)
	}
	if row.ObservedTS != "1700000000.000500" {
		t.Errorf("ObservedTS = %q, want the value just written", row.ObservedTS)
	}
}

// TestMigratesOnlyOnce checks that the second open of a migrated database is a
// no-op: a migration that runs twice would fail on its own ALTER, which is how a
// restart would break a working install.
func TestMigratesOnlyOnce(t *testing.T) {
	ctx := context.Background()
	path := writeV1Database(t)

	for i := range 2 {
		s, err := Open(ctx, path)
		if err != nil {
			t.Fatalf("Open %d: %v", i+1, err)
		}
		if err := s.Close(); err != nil {
			t.Fatalf("Close %d: %v", i+1, err)
		}
	}
}

// TestObservedWatermarkIsIndependentOfTheCursor pins the two to different
// columns: they advance for different reasons (the event stream versus the
// conversation), and a shared one would lose messages on every restart.
func TestObservedWatermarkIsIndependentOfTheCursor(t *testing.T) {
	ctx := context.Background()
	s, _ := newStore(t)
	row := sampleThread()
	if err := s.PutThread(ctx, row); err != nil {
		t.Fatalf("PutThread: %v", err)
	}

	if err := s.SetThreadObservedTS(ctx, row.ThreadKey, "1700000000.000900"); err != nil {
		t.Fatalf("SetThreadObservedTS: %v", err)
	}
	got, _, err := s.Thread(ctx, row.ThreadKey)
	if err != nil {
		t.Fatalf("Thread: %v", err)
	}
	if got.LastSeq != row.LastSeq || got.LeafID != row.LeafID {
		t.Errorf("the watermark write moved the cursor: seq %d->%d, leaf %q->%q",
			row.LastSeq, got.LastSeq, row.LeafID, got.LeafID)
	}
	if got.ObservedTS != "1700000000.000900" {
		t.Errorf("ObservedTS = %q, want the value just written", got.ObservedTS)
	}
}

// TestVersionStatementsCoverEverySchema is what keeps the allowlist honest:
// stamping a version needs a literal statement, and a new schema version that
// forgets one would otherwise fail at Open on a live install.
func TestVersionStatementsCoverEverySchema(t *testing.T) {
	needed := map[int]bool{schemaVersion: true}
	for _, m := range migrations {
		needed[m.to] = true
	}
	for version := range needed {
		if version == 0 {
			continue
		}
		if _, ok := versionStatements[version]; !ok {
			t.Errorf("no version statement for schema %d", version)
		}
	}
	for version := range versionStatements {
		if version > schemaVersion {
			t.Errorf("versionStatements has an entry for %d, newer than schemaVersion %d",
				version, schemaVersion)
		}
	}
}
