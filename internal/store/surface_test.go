package store

import (
	"context"
	"testing"
	"time"
)

// TestSurfaceKVRemembersOverwritesAndStaysNamespaced covers the whole job of the
// key/value half: a cursor written is a cursor read, a later write wins, and one
// surface cannot see another's keys.
func TestSurfaceKVRemembersOverwritesAndStaysNamespaced(t *testing.T) {
	ctx := context.Background()
	s, _ := newStore(t)

	if _, found, err := s.SurfaceKV(ctx, "self_dm", "cursor"); err != nil || found {
		t.Fatalf("unknown key = (_, %v, %v), want (_, false, nil)", found, err)
	}
	if err := s.SetSurfaceKV(ctx, "self_dm", "cursor", "1700000000.000100"); err != nil {
		t.Fatalf("SetSurfaceKV: %v", err)
	}
	got, found, err := s.SurfaceKV(ctx, "self_dm", "cursor")
	if err != nil {
		t.Fatalf("SurfaceKV: %v", err)
	}
	if !found || got != "1700000000.000100" {
		t.Fatalf("SurfaceKV = (%q, %v), want the value just written", got, found)
	}
	if err := s.SetSurfaceKV(ctx, "self_dm", "cursor", "1700000002.000000"); err != nil {
		t.Fatalf("SetSurfaceKV again: %v", err)
	}
	got, _, err = s.SurfaceKV(ctx, "self_dm", "cursor")
	if err != nil || got != "1700000002.000000" {
		t.Fatalf("SurfaceKV after the second write = (%q, %v), want the newer value", got, err)
	}
	if err := s.SetSurfaceKV(ctx, "other", "cursor", "elsewhere"); err != nil {
		t.Fatalf("SetSurfaceKV for another surface: %v", err)
	}
	if got, _, _ := s.SurfaceKV(ctx, "self_dm", "cursor"); got != "1700000002.000000" {
		t.Errorf("another surface's write changed this one: %q", got)
	}
}

// TestSurfacePostedLedger is the record that tells the agent's own answer from
// the human's request in a conversation where both are the same author.
func TestSurfacePostedLedger(t *testing.T) {
	ctx := context.Background()
	s, _ := newStore(t)

	if posted, err := s.SurfacePosted(ctx, "self_dm", "1700000000.000100"); err != nil || posted {
		t.Fatalf("unknown ts = (%v, %v), want (false, nil)", posted, err)
	}
	if err := s.MarkSurfacePosted(ctx, "self_dm", "1700000000.000100"); err != nil {
		t.Fatalf("MarkSurfacePosted: %v", err)
	}
	if err := s.MarkSurfacePosted(ctx, "self_dm", "1700000000.000100"); err != nil {
		t.Fatalf("MarkSurfacePosted twice must be idempotent: %v", err)
	}
	posted, err := s.SurfacePosted(ctx, "self_dm", "1700000000.000100")
	if err != nil || !posted {
		t.Fatalf("SurfacePosted = (%v, %v), want (true, nil)", posted, err)
	}
	if posted, _ := s.SurfacePosted(ctx, "other", "1700000000.000100"); posted {
		t.Error("the ledger leaked into another surface's namespace")
	}
	// An empty timestamp is not a message and must not become a ledger entry
	// every later empty timestamp matches.
	if err := s.MarkSurfacePosted(ctx, "self_dm", ""); err != nil {
		t.Fatalf("MarkSurfacePosted(\"\"): %v", err)
	}
	if posted, _ := s.SurfacePosted(ctx, "self_dm", ""); posted {
		t.Error("an empty timestamp was recorded")
	}
}

// TestPruneSurfacePosted keeps the ledger proportional to a day of traffic
// rather than to the install's uptime.
func TestPruneSurfacePosted(t *testing.T) {
	ctx := context.Background()
	s, _ := newStore(t)

	for _, ts := range []string{"1700000000.000100", "1700000000.000200"} {
		if err := s.MarkSurfacePosted(ctx, "self_dm", ts); err != nil {
			t.Fatalf("MarkSurfacePosted(%s): %v", ts, err)
		}
	}
	// A cutoff in the past keeps everything: pruning is about age, not count.
	if n, err := s.PruneSurfacePosted(ctx, time.Now().Add(-time.Hour)); err != nil || n != 0 {
		t.Fatalf("PruneSurfacePosted(old) = (%d, %v), want (0, nil)", n, err)
	}
	// A cutoff in the future prunes what was just written, which is the same
	// code path with the clock moved.
	n, err := s.PruneSurfacePosted(ctx, time.Now().Add(time.Hour))
	if err != nil || n != 2 {
		t.Fatalf("PruneSurfacePosted(future) = (%d, %v), want (2, nil)", n, err)
	}
	if posted, _ := s.SurfacePosted(ctx, "self_dm", "1700000000.000100"); posted {
		t.Error("a pruned entry is still in the ledger")
	}
}

// TestSurfaceStateSurvivesAReopen is the property that matters: a restart must
// find the cursor where it left it, or the daemon replays a personal DM as a
// batch of commands.
func TestSurfaceStateSurvivesAReopen(t *testing.T) {
	ctx := context.Background()
	s, path := newStore(t)

	if err := s.SetSurfaceKV(ctx, "self_dm", "cursor", "1700000009.000000"); err != nil {
		t.Fatalf("SetSurfaceKV: %v", err)
	}
	if err := s.MarkSurfacePosted(ctx, "self_dm", "1700000009.000000"); err != nil {
		t.Fatalf("MarkSurfacePosted: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	again, err := Open(ctx, path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer again.Close()
	if got, found, err := again.SurfaceKV(ctx, "self_dm", "cursor"); err != nil || !found || got != "1700000009.000000" {
		t.Fatalf("cursor after reopen = (%q, %v, %v), want the stored value", got, found, err)
	}
	if posted, err := again.SurfacePosted(ctx, "self_dm", "1700000009.000000"); err != nil || !posted {
		t.Fatalf("ledger after reopen = (%v, %v), want (true, nil)", posted, err)
	}
}

// TestSurfaceStateWorksOnAMigratedDatabase opens the v1 database the migration
// tests build: an install that upgrades and then turns the self-DM on must get
// the tables from the migration, not from a new file.
func TestSurfaceStateWorksOnAMigratedDatabase(t *testing.T) {
	ctx := context.Background()
	s, err := Open(ctx, writeV1Database(t))
	if err != nil {
		t.Fatalf("Open a migrated database: %v", err)
	}
	defer s.Close()

	if err := s.SetSurfaceKV(ctx, "self_dm", "cursor", "1700000001.000000"); err != nil {
		t.Fatalf("SetSurfaceKV on a migrated database: %v", err)
	}
	if err := s.MarkSurfacePosted(ctx, "self_dm", "1700000001.000000"); err != nil {
		t.Fatalf("MarkSurfacePosted on a migrated database: %v", err)
	}
	if _, found, err := s.SurfaceKV(ctx, "self_dm", "cursor"); err != nil || !found {
		t.Fatalf("SurfaceKV on a migrated database = (_, %v, %v)", found, err)
	}
}
