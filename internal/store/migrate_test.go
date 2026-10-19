package store

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
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

func writeV9Database(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "state", "pi-chat.db")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatalf("create the directory: %v", err)
	}
	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatalf("open legacy database: %v", err)
	}
	defer db.Close()
	v9Schema := strings.Replace(schema, "    requester_user_id TEXT NOT NULL DEFAULT '',\n", "", 1)
	if v9Schema == schema {
		t.Fatal("current schema does not contain the requester column")
	}
	if _, err := db.Exec(v9Schema); err != nil {
		t.Fatalf("write the v9 schema: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO pending_ui
        (route_id, request_id, thread_key, session_path, method, message_ts, created_at, expires_at)
        VALUES ('old-route', 'old-dialog', 'T1:C1:1.1', '/s/a.jsonl', 'input', '9.9', 1700000000, 0)`); err != nil {
		t.Fatalf("insert v9 pending route: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO seen (event_id, received_at) VALUES ('ui-route:old-route', 1700000000)`); err != nil {
		t.Fatalf("insert v9 route replay key: %v", err)
	}
	if _, err := db.Exec("PRAGMA user_version = 9"); err != nil {
		t.Fatalf("stamp version 9: %v", err)
	}
	return path
}

// TestMigratesAVersionOneDatabase is the migration path's whole job: a database
// written by an older build opens, keeps its rows, and gains the schema current
// code reads and writes.
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
	checkpoint := ShutdownCheckpoint{
		ThreadKey: "T1:C1:1.1", SessionPath: "/s/a.jsonl", LeafID: "leaf-1", NoticeID: "notice-1",
	}
	if err := s.PutShutdownCheckpoint(ctx, checkpoint); err != nil {
		t.Fatalf("PutShutdownCheckpoint on the migrated database: %v", err)
	}
	if rows, err := s.ShutdownCheckpoints(ctx); err != nil || len(rows) != 1 || rows[0].LeafID != checkpoint.LeafID || rows[0].NoticeID != checkpoint.NoticeID {
		t.Errorf("shutdown checkpoints on the migrated database = %+v, err=%v", rows, err)
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
	pending := PendingUI{
		RouteID: "migrated-route", RequestID: "migrated-dialog", ThreadKey: row.ThreadKey, RequesterUserID: "U1", SessionPath: row.SessionPath,
		Method: "confirm", CreatedAt: time.Now(), ExpiresAt: time.Now().Add(time.Minute),
	}
	if inserted, err := s.PutPendingUI(ctx, pending); err != nil || !inserted {
		t.Errorf("pending UI on a migrated v1 database = inserted %v, err %v", inserted, err)
	}
}

func TestMigratesVersionNinePendingDialogsWithoutRequester(t *testing.T) {
	ctx := context.Background()
	path := writeV9Database(t)
	s, err := Open(ctx, path)
	if err != nil {
		t.Fatalf("Open a v9 database: %v", err)
	}
	defer s.Close()

	var version int
	if err := s.db.QueryRowContext(ctx, "PRAGMA user_version").Scan(&version); err != nil {
		t.Fatalf("read the version: %v", err)
	}
	if version != 10 {
		t.Fatalf("version after migration = %d, want 10", version)
	}
	legacy, found, err := s.PendingUI(ctx, "old-route")
	if err != nil || !found {
		t.Fatalf("legacy PendingUI = found %v, err %v", found, err)
	}
	if legacy.RequesterUserID != "" {
		t.Fatalf("legacy requester = %q, want unknown", legacy.RequesterUserID)
	}
	unowned, err := s.UnownedPendingUI(ctx)
	if err != nil || len(unowned) != 1 || unowned[0].RouteID != legacy.RouteID {
		t.Fatalf("unowned pending routes = %+v, err %v", unowned, err)
	}
	if inserted, err := s.PutPendingUI(ctx, PendingUI{
		RouteID: legacy.RouteID, RequestID: legacy.RequestID, ThreadKey: legacy.ThreadKey,
		RequesterUserID: "U1", SessionPath: legacy.SessionPath, Method: legacy.Method,
	}); err == nil || inserted {
		t.Fatalf("legacy route was re-authorized: inserted=%v err=%v", inserted, err)
	}
}

// TestMigratesOnlyOnce checks that the second open of a migrated database is a
// no-op: a migration that runs twice would fail on its own ALTER, which is how a
// restart would break a working install.
func TestFailedMigrationStepCanBeRetriedWithoutAPartialDDLChange(t *testing.T) {
	ctx := context.Background()
	path := writeV1Database(t)
	legacy, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatalf("open legacy database: %v", err)
	}
	if _, err := legacy.ExecContext(ctx, `CREATE TABLE session_tool_bindings (session_path TEXT PRIMARY KEY)`); err != nil {
		t.Fatalf("create a conflicting legacy table: %v", err)
	}
	if err := legacy.Close(); err != nil {
		t.Fatalf("close legacy database: %v", err)
	}

	if s, err := Open(ctx, path); err == nil {
		_ = s.Close()
		t.Fatal("Open succeeded despite the broken v5 index target")
	}
	check, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatalf("reopen failed migration: %v", err)
	}
	var version int
	if err := check.QueryRowContext(ctx, "PRAGMA user_version").Scan(&version); err != nil {
		t.Fatalf("read version after failure: %v", err)
	}
	if version != 4 {
		t.Errorf("version after failed migration = %d, want 4", version)
	}
	columns, err := check.QueryContext(ctx, "PRAGMA table_info(threads)")
	if err != nil {
		t.Fatalf("read threads columns: %v", err)
	}
	hasSurface := false
	for columns.Next() {
		var cid, notnull, pk int
		var name, typ string
		var defaultValue sql.NullString
		if err := columns.Scan(&cid, &name, &typ, &notnull, &defaultValue, &pk); err != nil {
			t.Fatalf("scan threads column: %v", err)
		}
		hasSurface = hasSurface || name == "surface"
	}
	if err := columns.Err(); err != nil {
		t.Fatalf("iterate threads columns: %v", err)
	}
	if err := columns.Close(); err != nil {
		t.Fatalf("close columns: %v", err)
	}
	if hasSurface {
		t.Error("failed v5 migration left the first ALTER TABLE behind")
	}
	if _, err := check.ExecContext(ctx, "DROP TABLE session_tool_bindings"); err != nil {
		t.Fatalf("remove the conflicting table: %v", err)
	}
	if err := check.Close(); err != nil {
		t.Fatalf("close retry connection: %v", err)
	}

	s, err := Open(ctx, path)
	if err != nil {
		t.Fatalf("retry after correcting the conflict: %v", err)
	}
	defer s.Close()
	if err := s.db.QueryRowContext(ctx, "PRAGMA user_version").Scan(&version); err != nil {
		t.Fatalf("read retry version: %v", err)
	}
	if version != schemaVersion {
		t.Errorf("version after retry = %d, want %d", version, schemaVersion)
	}
}

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
