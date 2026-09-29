package bot

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tigersoldier/pi-gateway/gwclient"
	"github.com/tigersoldier/pi-gateway/protocol"

	"github.com/tigersoldier/pi-chat/internal/store"
)

// lifecycleFor returns the test core's fake gateway lifecycle, which the core
// reaches through its own interface.
func lifecycleFor(t *testing.T, b *Bot) *fakeLifecycle {
	t.Helper()
	life, ok := b.life.(*fakeLifecycle)
	if !ok {
		t.Fatalf("the test core holds %T as its lifecycle, want the fake", b.life)
	}
	return life
}

// deletePress is the Delete button's payload, as the adapter delivers it: the
// thread the message was in, and the session path the confirmation named.
func deletePress(value string) Action {
	return Action{
		EventID:   "Ev-delete-" + value,
		Channel:   "C1",
		Thread:    &Thread{Workspace: "T1", Channel: "C1", ThreadTS: "1700000000.000100"},
		UserID:    "U1",
		Workspace: "T1",
		ActionID:  ActionDelete,
		Value:     value,
		MessageTS: "1700000000.000200",
	}
}

func TestDeleteAsksBeforeItDeletes(t *testing.T) {
	b, platform, st := newTestBot(t)
	life := lifecycleFor(t, b)
	th := threadWithSession(t, b, st, "C1", "1700000000.000100")
	row := th.snapshot()

	b.HandleCommand(context.Background(), commandRequest("1700000000.000100", "/delete"))

	waitFor(t, "the confirmation", func() bool { return len(platform.postedNotices()) == 1 })
	if deleted := life.deletedSessions(); len(deleted) != 0 {
		t.Fatalf("the question deleted %v", deleted)
	}
	if _, err := os.Stat(row.ProjectDir); err != nil {
		t.Errorf("the question removed the project directory: %v", err)
	}

	notice := platform.postedNotices()[0]
	if !notice.Ephemeral {
		t.Error("the confirmation is visible to the whole channel")
	}
	if !strings.Contains(notice.Text, row.SessionName) {
		t.Errorf("the confirmation does not name the session: %q", notice.Text)
	}
	if len(notice.Buttons) != 2 {
		t.Fatalf("the confirmation has %d buttons, want two", len(notice.Buttons))
	}
	confirm, cancel := notice.Buttons[0], notice.Buttons[1]
	if confirm.ActionID != ActionDelete || confirm.Style != "danger" {
		t.Errorf("the confirm button is %#v, want a danger button named %q", confirm, ActionDelete)
	}
	if cancel.ActionID != ActionDeleteCancel {
		t.Errorf("the cancel button is %#v, want %q", cancel, ActionDeleteCancel)
	}
	// Both buttons carry the session the confirmation was about, which is what the
	// press checks before it deletes anything.
	for _, button := range notice.Buttons {
		if button.Value != row.SessionPath {
			t.Errorf("button %q carries %q, want the session path", button.ActionID, button.Value)
		}
	}
}

func TestDeleteDeletesTheSessionAndItsWorktree(t *testing.T) {
	b, platform, st := newTestBot(t)
	life := lifecycleFor(t, b)
	th := threadWithSession(t, b, st, "C1", "1700000000.000100")
	row := th.snapshot()
	if row.ObservedTS == "" {
		t.Fatal("the seeded thread has no watermark to test the clearing of")
	}

	b.HandleAction(context.Background(), deletePress(row.SessionPath))

	waitFor(t, "the deletion", func() bool { return len(life.deletedSessions()) == 1 })
	if got := life.deletedSessions()[0]; got != row.SessionPath {
		t.Errorf("deleted %q, want %q", got, row.SessionPath)
	}
	if _, err := os.Stat(row.ProjectDir); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("the project directory survived: %v", err)
	}

	got, found, err := st.Thread(context.Background(), row.ThreadKey)
	if err != nil || !found {
		t.Fatalf("the row is gone (found=%v, err=%v); the key has to stay reserved", found, err)
	}
	if got.State != store.StateDeleted {
		t.Errorf("state = %q, want %q", got.State, store.StateDeleted)
	}
	if got.SessionName != "" || got.SessionPath != "" || got.SessionID != "" {
		t.Errorf("the row still names a session: %#v", got)
	}
	if got.Cwd != "" || got.ProjectDir != "" {
		t.Errorf("the row still names a directory: %#v", got)
	}
	if got.ObservedTS != "" {
		t.Errorf("the row kept the deleted session's watermark: %q", got.ObservedTS)
	}

	waitFor(t, "the answer", func() bool { return len(platform.postedNotices()) >= 2 })
	notices := platform.postedNotices()
	text := notices[len(notices)-1].Text
	if !strings.Contains(text, "Deleted") || !strings.Contains(text, row.SessionName) {
		t.Errorf("the answer does not say what happened: %q", text)
	}
	if !strings.Contains(text, "fresh session") {
		t.Errorf("the answer does not say what happens next: %q", text)
	}
	if statuses := platform.reportedStatuses(); len(statuses) == 0 || statuses[len(statuses)-1] != StatusClosed {
		t.Errorf("statuses = %v, want the session closed", statuses)
	}
}

func TestDeleteLeavesWhatIsNotPiChatsToRemove(t *testing.T) {
	// The cleanup removes worktrees pi-chat made. A file the agent wrote next to
	// them is somebody's work, so the directory stays and the answer names it
	// (DESIGN.md §9).
	b, platform, st := newTestBot(t)
	life := lifecycleFor(t, b)
	th := threadWithSession(t, b, st, "C1", "1700000000.000100")
	row := th.snapshot()
	keep := filepath.Join(row.ProjectDir, "notes.md")
	if err := os.WriteFile(keep, []byte("mine"), 0o600); err != nil {
		t.Fatalf("write the file: %v", err)
	}

	b.HandleAction(context.Background(), deletePress(row.SessionPath))

	waitFor(t, "the deletion", func() bool { return len(life.deletedSessions()) == 1 })
	waitFor(t, "the answer", func() bool { return len(platform.postedNotices()) >= 2 })
	if _, err := os.Stat(keep); err != nil {
		t.Errorf("the file the agent wrote is gone: %v", err)
	}
	notices := platform.postedNotices()
	if text := notices[len(notices)-1].Text; !strings.Contains(text, keep) {
		t.Errorf("the answer does not name what it left in place: %q", text)
	}
}

func TestDeleteRefusesWhileATurnIsRunning(t *testing.T) {
	b, platform, st := newTestBot(t)
	life := lifecycleFor(t, b)
	th := threadWithSession(t, b, st, "C1", "1700000000.000100")
	row := th.snapshot()

	th.turnMu.Lock()
	defer th.turnMu.Unlock()

	b.HandleAction(context.Background(), deletePress(row.SessionPath))

	waitFor(t, "the refusal", func() bool { return len(platform.postedNotices()) == 1 })
	if deleted := life.deletedSessions(); len(deleted) != 0 {
		t.Fatalf("a session was deleted under a running turn: %v", deleted)
	}
	if _, err := os.Stat(row.ProjectDir); err != nil {
		t.Errorf("the project directory went with the refusal: %v", err)
	}
	if text := platform.postedNotices()[0].Text; !strings.Contains(text, "A turn is running") {
		t.Errorf("the refusal does not say why: %q", text)
	}
}

func TestDeleteRefusesAConfirmationForAnotherSession(t *testing.T) {
	// A confirmation is about the session it named. By the time it is pressed the
	// thread may own a different one — a `/new` in between — and deleting that
	// instead of the session somebody agreed to delete is how a stale button
	// destroys work.
	b, platform, st := newTestBot(t)
	life := lifecycleFor(t, b)
	th := threadWithSession(t, b, st, "C1", "1700000000.000100")
	row := th.snapshot()

	b.HandleAction(context.Background(), deletePress("/sessions/somewhere-else.jsonl"))

	waitFor(t, "the refusal", func() bool { return len(platform.postedNotices()) == 1 })
	if deleted := life.deletedSessions(); len(deleted) != 0 {
		t.Fatalf("a stale confirmation deleted %v", deleted)
	}
	if _, err := os.Stat(row.ProjectDir); err != nil {
		t.Errorf("the project directory went with the refusal: %v", err)
	}
	if text := platform.postedNotices()[0].Text; !strings.Contains(text, "/delete") {
		t.Errorf("the refusal does not say how to ask again: %q", text)
	}
}

func TestDeleteOfASessionTheGatewayAlreadyLost(t *testing.T) {
	// The session was deleted outside pi-chat: there is nothing to delete, but the
	// directory it left behind is still pi-chat's to clean and the row still points
	// at a session that does not exist.
	b, platform, st := newTestBot(t)
	life := lifecycleFor(t, b)
	th := threadWithSession(t, b, st, "C1", "1700000000.000100")
	row := th.snapshot()
	life.failDelete(&gwclient.ResponseError{
		Command: "gw_delete_session", Code: protocol.CodeUnknownSession, Message: "unknown session",
	})

	b.HandleAction(context.Background(), deletePress(row.SessionPath))

	waitFor(t, "the answer", func() bool { return len(platform.postedNotices()) >= 2 })
	if _, err := os.Stat(row.ProjectDir); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("the directory of a session that was already gone survived: %v", err)
	}
	got, _, err := st.Thread(context.Background(), row.ThreadKey)
	if err != nil {
		t.Fatalf("read the row: %v", err)
	}
	if got.State != store.StateDeleted || got.SessionPath != "" {
		t.Errorf("the row still points at the lost session: %#v", got)
	}
	notices := platform.postedNotices()
	if text := notices[len(notices)-1].Text; !strings.Contains(text, "already forgotten") {
		t.Errorf("the answer does not say the session was already gone: %q", text)
	}
}

func TestDeleteReportsAFailureWithoutLosingTheSession(t *testing.T) {
	// A gateway that refuses leaves everything as it was, so the user can ask
	// again — and must be told that nothing happened, not shown a success.
	b, platform, st := newTestBot(t)
	life := lifecycleFor(t, b)
	th := threadWithSession(t, b, st, "C1", "1700000000.000100")
	row := th.snapshot()
	life.failDelete(errors.New("the daemon is not answering"))

	b.HandleAction(context.Background(), deletePress(row.SessionPath))

	waitFor(t, "the answer", func() bool { return len(platform.postedNotices()) >= 2 })
	if _, err := os.Stat(row.ProjectDir); err != nil {
		t.Errorf("the project directory went with the failure: %v", err)
	}
	got, _, err := st.Thread(context.Background(), row.ThreadKey)
	if err != nil {
		t.Fatalf("read the row: %v", err)
	}
	if got.State != store.StateCold || got.SessionPath != row.SessionPath {
		t.Errorf("a failed delete changed the row: %#v", got)
	}
	notices := platform.postedNotices()
	if text := notices[len(notices)-1].Text; !strings.Contains(text, "Nothing was deleted") {
		t.Errorf("the answer claims a success it did not have: %q", text)
	}
}

func TestDeleteCancelDeletesNothing(t *testing.T) {
	b, platform, st := newTestBot(t)
	life := lifecycleFor(t, b)
	th := threadWithSession(t, b, st, "C1", "1700000000.000100")
	row := th.snapshot()

	press := deletePress(row.SessionPath)
	press.ActionID = ActionDeleteCancel
	b.HandleAction(context.Background(), press)

	waitFor(t, "the answer", func() bool { return len(platform.postedNotices()) == 1 })
	if deleted := life.deletedSessions(); len(deleted) != 0 {
		t.Fatalf("cancel deleted %v", deleted)
	}
	if _, err := os.Stat(row.ProjectDir); err != nil {
		t.Errorf("cancel removed the project directory: %v", err)
	}
	notice := platform.postedNotices()[0]
	if !strings.Contains(notice.Text, "Nothing was deleted") {
		t.Errorf("cancel answered %q", notice.Text)
	}
	// The confirmation is replaced rather than left behind: a stale Delete button
	// in a thread is a trap.
	if notice.Update != press.MessageTS {
		t.Errorf("the confirmation was not replaced (Update = %q)", notice.Update)
	}
}

func TestDeleteWithoutASessionWritesNothing(t *testing.T) {
	// /delete is read-only when there is nothing to delete: it must not register a
	// thread row for a thread pi-chat has never worked in.
	b, platform, st := newTestBot(t)

	b.HandleCommand(context.Background(), commandRequest("1700000000.000100", "/delete"))

	waitFor(t, "the answer", func() bool { return len(platform.postedNotices()) == 1 })
	if text := platform.postedNotices()[0].Text; !strings.Contains(text, "no session") {
		t.Errorf("the answer is %q, want a plain statement", text)
	}
	if _, found, err := st.Thread(context.Background(), "T1:C1:1700000000.000100"); err != nil || found {
		t.Errorf("a read-only command wrote a thread row (found=%v, err=%v)", found, err)
	}
}

func TestDeleteAdoptedSessionLeavesItsDirectoryAlone(t *testing.T) {
	// An adopted session's working directory is somebody else's: pi-chat did not
	// create it and must not remove it (DESIGN.md §9).
	b, platform, st := newTestBot(t)
	life := lifecycleFor(t, b)
	dir := t.TempDir()
	row := store.ThreadRow{
		ThreadKey:   "T1:C1:1700000000.000100",
		WorkspaceID: "T1",
		ChannelID:   "C1",
		ThreadTS:    "1700000000.000100",
		SessionName: "adopted",
		SessionPath: "/elsewhere/session.jsonl",
		Cwd:         dir,
		ProjectDir:  "",
		State:       store.StateCold,
	}
	seedThread(t, st, row)

	b.HandleCommand(context.Background(), commandRequest("1700000000.000100", "/delete"))

	waitFor(t, "the confirmation", func() bool { return len(platform.postedNotices()) == 1 })
	if text := platform.postedNotices()[0].Text; !strings.Contains(text, "not pi-chat's to remove") {
		t.Errorf("the confirmation does not warn that its directory stays: %q", text)
	}

	b.HandleAction(context.Background(), deletePress(row.SessionPath))
	waitFor(t, "the deletion", func() bool { return len(life.deletedSessions()) == 1 })
	if _, err := os.Stat(dir); err != nil {
		t.Errorf("an adopted session's directory was removed: %v", err)
	}
}
