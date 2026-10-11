package store

import (
	"errors"
	"path/filepath"
	"testing"
	"time"
)

func toolBinding(path string) SessionToolBinding {
	return SessionToolBinding{
		SessionPath:      path,
		Provider:         "slack",
		Surface:          "app",
		WorkspaceID:      "T1",
		ChannelID:        "C1",
		ThreadTS:         "1700000000.000100",
		Capabilities:     []string{"read_thread", "read_message", "read_user"},
		ExtensionVersion: "v1",
		State:            ToolBindingActive,
	}
}

func TestSessionToolBindingRoundTripAndIdempotentBind(t *testing.T) {
	ctx := t.Context()
	s, _ := newStore(t)
	want := toolBinding("/sessions/a.jsonl")
	if err := s.BindSessionToolBinding(ctx, want, false); err != nil {
		t.Fatalf("BindSessionToolBinding: %v", err)
	}
	// Same-origin binding is safe to repeat during recovery.
	if err := s.BindSessionToolBinding(ctx, want, false); err != nil {
		t.Fatalf("idempotent bind: %v", err)
	}
	got, found, err := s.SessionToolBinding(ctx, want.SessionPath)
	if err != nil {
		t.Fatalf("SessionToolBinding: %v", err)
	}
	if !found {
		t.Fatal("binding not found")
	}
	if got.Provider != want.Provider || got.Surface != want.Surface ||
		got.WorkspaceID != want.WorkspaceID || got.ChannelID != want.ChannelID ||
		got.ThreadTS != want.ThreadTS || got.State != ToolBindingActive ||
		got.ExtensionVersion != want.ExtensionVersion {
		t.Errorf("binding = %+v, want the stored metadata", got)
	}
	if len(got.Capabilities) != 3 || got.Capabilities[0] != "read_message" || got.Capabilities[1] != "read_thread" || got.Capabilities[2] != "read_user" {
		t.Errorf("capabilities = %v, want sorted read-only allowlist", got.Capabilities)
	}
	if got.CreatedAt.IsZero() || !got.CreatedAt.Before(time.Now().Add(time.Second)) {
		t.Errorf("CreatedAt = %v, want a database timestamp", got.CreatedAt)
	}
}

func TestToolBindingNeedsExplicitRebindAndRevocation(t *testing.T) {
	ctx := t.Context()
	s, _ := newStore(t)
	first := toolBinding("/sessions/a.jsonl")
	if err := s.BindSessionToolBinding(ctx, first, false); err != nil {
		t.Fatal(err)
	}
	second := first
	second.Surface = "self_dm"
	second.ChannelID = "D2"
	if err := s.BindSessionToolBinding(ctx, second, false); !errors.Is(err, ErrToolBindingConflict) {
		t.Fatalf("unconfirmed rebind error = %v, want ErrToolBindingConflict", err)
	}
	if err := s.BindSessionToolBinding(ctx, second, true); err != nil {
		t.Fatalf("confirmed rebind: %v", err)
	}
	got, _, err := s.SessionToolBinding(ctx, first.SessionPath)
	if err != nil {
		t.Fatal(err)
	}
	if got.Surface != second.Surface || got.ChannelID != second.ChannelID {
		t.Errorf("binding after rebind = %+v, want new origin %+v", got, second)
	}
	if err := s.RevokeSessionToolBinding(ctx, first.SessionPath); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	got, found, err := s.SessionToolBinding(ctx, first.SessionPath)
	if err != nil || !found {
		t.Fatalf("binding after revoke: found=%v err=%v", found, err)
	}
	if got.State != ToolBindingRevoked {
		t.Errorf("state after revoke = %q, want revoked", got.State)
	}
	if err := s.BindSessionToolBinding(ctx, second, false); !errors.Is(err, ErrToolBindingConflict) {
		t.Fatalf("implicit reactivation error = %v, want ErrToolBindingConflict", err)
	}
	if err := s.BindSessionToolBinding(ctx, second, true); err != nil {
		t.Fatalf("confirmed reactivation: %v", err)
	}
}

func TestPutThreadWithToolBindingCommitsMatchingOwner(t *testing.T) {
	ctx := t.Context()
	s, _ := newStore(t)
	thread := sampleThread()
	binding := toolBinding(thread.SessionPath)
	if err := s.PutThreadWithToolBinding(ctx, thread, binding, false); err != nil {
		t.Fatalf("PutThreadWithToolBinding: %v", err)
	}
	gotThread, found, err := s.Thread(ctx, thread.ThreadKey)
	if err != nil || !found || gotThread.SessionPath != thread.SessionPath {
		t.Fatalf("thread = %+v, found=%v err=%v", gotThread, found, err)
	}
	gotBinding, found, err := s.SessionToolBinding(ctx, thread.SessionPath)
	if err != nil || !found || gotBinding.State != ToolBindingActive || gotBinding.Surface != thread.Surface {
		t.Fatalf("binding = %+v, found=%v err=%v", gotBinding, found, err)
	}
}

func TestPutThreadWithToolBindingIsAtomic(t *testing.T) {
	ctx := t.Context()
	s, _ := newStore(t)
	oldThread := sampleThread()
	oldThread.ThreadKey, oldThread.ChannelID = "T1:D1:1700000000.000100", "D1"
	oldThread.SessionName, oldThread.Surface = "existing-self-dm", "self_dm"
	if err := s.PutThread(ctx, oldThread); err != nil {
		t.Fatal(err)
	}
	oldBinding := toolBinding(oldThread.SessionPath)
	oldBinding.Surface, oldBinding.ChannelID = "self_dm", "D1"
	if err := s.BindSessionToolBinding(ctx, oldBinding, false); err != nil {
		t.Fatal(err)
	}

	thread := sampleThread()
	thread.SessionName = "must-roll-back"
	binding := toolBinding(thread.SessionPath) // matches the attempted app thread, not the old self-DM owner
	if err := s.PutThreadWithToolBinding(ctx, thread, binding, false); !errors.Is(err, ErrToolBindingConflict) {
		t.Fatalf("conflicting transaction error = %v, want ErrToolBindingConflict", err)
	}
	got, found, err := s.Thread(ctx, oldThread.ThreadKey)
	if err != nil || !found {
		t.Fatalf("Thread: found=%v err=%v", found, err)
	}
	if got.SessionName != "existing-self-dm" || got.Surface != "self_dm" {
		t.Fatalf("thread update committed despite binding conflict: %+v", got)
	}
	gotBinding, _, err := s.SessionToolBinding(ctx, oldBinding.SessionPath)
	if err != nil {
		t.Fatal(err)
	}
	if gotBinding.Surface != "self_dm" || gotBinding.ChannelID != "D1" {
		t.Errorf("binding after rollback = %+v, want original self-DM binding", gotBinding)
	}

	mismatch := binding
	mismatch.SessionPath = "/sessions/other.jsonl"
	if err := s.PutThreadWithToolBinding(ctx, thread, mismatch, false); err == nil {
		t.Fatal("thread and tool binding with different session paths were accepted")
	}
}

func TestCanonicalSessionPathRequiresAbsolutePath(t *testing.T) {
	if _, err := CanonicalSessionPath("relative.jsonl"); err == nil {
		t.Fatal("relative path was accepted")
	}
	got, err := CanonicalSessionPath(filepath.Join("/sessions", "x", "..", "a.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if got != "/sessions/a.jsonl" {
		t.Errorf("canonical path = %q, want /sessions/a.jsonl", got)
	}
}
