package bot

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/tigersoldier/pi-chat/internal/store"
	"github.com/tigersoldier/pi-gateway/gwclient"
	"github.com/tigersoldier/pi-gateway/protocol"
)

type fakeShutdownClient struct {
	running bool
	leafID  string

	promptText   string
	followUpText string
	entries      gwclient.EntriesPage
	entriesErr   error
	lastSince    string
	entryReads   int
}

func (c *fakeShutdownClient) TurnRunning() bool { return c.running }
func (c *fakeShutdownClient) LeafID() string    { return c.leafID }

func (c *fakeShutdownClient) Prompt(_ context.Context, text string, _ ...protocol.ImageContent) (*gwclient.Response, error) {
	c.promptText = text
	return nil, nil
}

func (c *fakeShutdownClient) FollowUp(_ context.Context, text string, _ ...protocol.ImageContent) (*gwclient.Response, error) {
	c.followUpText = text
	return nil, nil
}

func (c *fakeShutdownClient) GetEntries(_ context.Context, since string) (gwclient.EntriesPage, error) {
	c.entryReads++
	c.lastSince = since
	return c.entries, c.entriesErr
}

type fakeShutdownCheckpointWriter struct {
	checkpoint store.ShutdownCheckpoint
	err        error
	writes     int
}

func (s *fakeShutdownCheckpointWriter) PutShutdownCheckpoint(_ context.Context, row store.ShutdownCheckpoint) error {
	s.writes++
	if s.err != nil {
		return s.err
	}
	s.checkpoint = row
	return nil
}

func TestSendShutdownNoticeDoesNotPreemptWork(t *testing.T) {
	tests := []struct {
		name       string
		busy       bool
		localBusy  bool
		wantPrompt bool
	}{
		{name: "idle session", wantPrompt: true},
		{name: "running session", busy: true},
		{name: "local turn still unwinding", localBusy: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			client := &fakeShutdownClient{running: test.busy}
			if err := sendShutdownNotice(context.Background(), client, test.localBusy, shutdownNoticeText("notice-1")); err != nil {
				t.Fatalf("sendShutdownNotice: %v", err)
			}
			if test.wantPrompt != (client.promptText != "") {
				t.Errorf("prompt text = %q, wantPrompt=%v", client.promptText, test.wantPrompt)
			}
			if (!test.wantPrompt) != (client.followUpText != "") {
				t.Errorf("follow-up text = %q, wantPrompt=%v", client.followUpText, test.wantPrompt)
			}
		})
	}
}

func TestQueueShutdownNoticeCapturesCurrentLeafAndPersistsFirst(t *testing.T) {
	client := &fakeShutdownClient{
		leafID: "stale-leaf",
		entries: gwclient.EntriesPage{
			Entries: []json.RawMessage{sessionEntry(t, "old", "assistant", "previous answer")},
			LeafID:  "current-leaf",
		},
	}
	writer := &fakeShutdownCheckpointWriter{}
	row := store.ThreadRow{ThreadKey: "thread-1", SessionPath: "/sessions/a.jsonl"}

	if err := queueShutdownNotice(context.Background(), writer, row, client, true); err != nil {
		t.Fatalf("queueShutdownNotice: %v", err)
	}
	if client.lastSince != "stale-leaf" {
		t.Errorf("GetEntries since = %q, want stale-leaf", client.lastSince)
	}
	if writer.writes != 1 || writer.checkpoint.LeafID != "current-leaf" || writer.checkpoint.NoticeID == "" {
		t.Fatalf("checkpoint = %+v, writes=%d", writer.checkpoint, writer.writes)
	}
	if client.followUpText == "" || !strings.Contains(client.followUpText, writer.checkpoint.NoticeID) || client.promptText != "" {
		t.Fatalf("notice text did not carry the persisted ID: prompt=%q follow-up=%q", client.promptText, client.followUpText)
	}
}

func TestQueueShutdownNoticeFailsClosed(t *testing.T) {
	row := store.ThreadRow{ThreadKey: "thread-1", SessionPath: "/sessions/a.jsonl"}
	t.Run("leaf read fails", func(t *testing.T) {
		client := &fakeShutdownClient{entriesErr: errors.New("gateway offline")}
		writer := &fakeShutdownCheckpointWriter{}
		if err := queueShutdownNotice(context.Background(), writer, row, client, false); err == nil {
			t.Fatal("queueShutdownNotice succeeded despite GetEntries failure")
		}
		if writer.writes != 0 || client.promptText != "" || client.followUpText != "" {
			t.Fatalf("failure had side effects: writes=%d prompt=%q follow-up=%q", writer.writes, client.promptText, client.followUpText)
		}
	})
	t.Run("checkpoint write fails", func(t *testing.T) {
		client := &fakeShutdownClient{entries: gwclient.EntriesPage{LeafID: "current-leaf"}}
		writer := &fakeShutdownCheckpointWriter{err: errors.New("database unavailable")}
		if err := queueShutdownNotice(context.Background(), writer, row, client, false); err == nil {
			t.Fatal("queueShutdownNotice succeeded despite checkpoint write failure")
		}
		if writer.writes != 1 || client.promptText != "" || client.followUpText != "" {
			t.Fatalf("failure had side effects: writes=%d prompt=%q follow-up=%q", writer.writes, client.promptText, client.followUpText)
		}
	})
}

func TestCheckShutdownResponsesDoesNotRepeatPostedCheckpoint(t *testing.T) {
	b, platform, st := newTestBot(t)
	checkpoint := store.ShutdownCheckpoint{
		ThreadKey: "thread-1", SessionPath: "/sessions/a.jsonl", NoticeID: "notice-1",
	}
	if err := st.PutShutdownCheckpoint(context.Background(), checkpoint); err != nil {
		t.Fatalf("PutShutdownCheckpoint: %v", err)
	}
	if err := st.MarkShutdownCheckpointPosted(context.Background(), checkpoint.ThreadKey, checkpoint.SessionPath, checkpoint.NoticeID); err != nil {
		t.Fatalf("MarkShutdownCheckpointPosted: %v", err)
	}

	b.CheckShutdownResponses(context.Background())

	rows, err := st.ShutdownCheckpoints(context.Background())
	if err != nil || len(rows) != 0 {
		t.Fatalf("checkpoints after cleanup = %+v, err=%v", rows, err)
	}
	if len(platform.notices) != 0 {
		t.Fatalf("posted checkpoint was delivered again: %+v", platform.notices)
	}
}

func TestShutdownResponseRequiresTheMatchingNoticeAndStopsAtNextUser(t *testing.T) {
	noticeID := "notice-123"
	notice := sessionEntry(t, "notice-entry", "user", shutdownNoticeText(noticeID))
	oldAnswer := sessionEntry(t, "old-answer", "assistant", "old answer")
	shutdownAnswer := sessionEntry(t, "shutdown-answer", "assistant", "finished safely")
	nextUser := sessionEntry(t, "next-user", "user", "a new unrelated request")
	nextAnswer := sessionEntry(t, "next-answer", "assistant", "unrelated answer")
	toolEntry := json.RawMessage(`{"id":"tool-entry","type":"message","message":{"role":"assistant","content":[{"type":"toolCall","name":"run"}]}}`)

	tests := []struct {
		name              string
		client            *fakeShutdownClient
		checkpoint        store.ShutdownCheckpoint
		wantAnswer        string
		wantReady         bool
		wantLeaf          string
		wantNoticeEntryID string
		wantAbandoned     bool
		wantReads         int
		wantErr           bool
	}{
		{
			name:       "running session is not inspected",
			client:     &fakeShutdownClient{running: true},
			checkpoint: store.ShutdownCheckpoint{LeafID: "before", NoticeEntryID: "notice-entry", NoticeID: noticeID},
			wantLeaf:   "before", wantNoticeEntryID: "notice-entry",
		},
		{
			name:       "old assistant without matching marker is ignored",
			client:     &fakeShutdownClient{entries: gwclient.EntriesPage{Entries: []json.RawMessage{oldAnswer}, LeafID: "current"}},
			checkpoint: store.ShutdownCheckpoint{LeafID: "before", NoticeID: noticeID},
			wantLeaf:   "current", wantReads: 1,
		},
		{
			name:       "marker is remembered while waiting for answer",
			client:     &fakeShutdownClient{entries: gwclient.EntriesPage{Entries: []json.RawMessage{notice}, LeafID: "notice-entry"}},
			checkpoint: store.ShutdownCheckpoint{LeafID: "before", NoticeID: noticeID},
			wantLeaf:   "notice-entry", wantNoticeEntryID: "notice-entry", wantReads: 1,
		},
		{
			name:       "shutdown answer is returned",
			client:     &fakeShutdownClient{entries: gwclient.EntriesPage{Entries: []json.RawMessage{notice, shutdownAnswer}, LeafID: "shutdown-answer"}},
			checkpoint: store.ShutdownCheckpoint{LeafID: "before", NoticeID: noticeID},
			wantAnswer: "finished safely", wantReady: true, wantLeaf: "shutdown-answer", wantNoticeEntryID: "notice-entry", wantReads: 1,
		},
		{
			name:       "later user answer is not attributed to shutdown",
			client:     &fakeShutdownClient{entries: gwclient.EntriesPage{Entries: []json.RawMessage{notice, shutdownAnswer, nextUser, nextAnswer}, LeafID: "next-answer"}},
			checkpoint: store.ShutdownCheckpoint{LeafID: "before", NoticeID: noticeID},
			wantAnswer: "finished safely", wantReady: true, wantLeaf: "next-answer", wantNoticeEntryID: "notice-entry", wantReads: 1,
		},
		{
			name:       "remembered marker resumes after its cursor",
			client:     &fakeShutdownClient{entries: gwclient.EntriesPage{Entries: []json.RawMessage{shutdownAnswer, nextUser, nextAnswer}, LeafID: "next-answer"}},
			checkpoint: store.ShutdownCheckpoint{LeafID: "notice-entry", NoticeEntryID: "notice-entry", NoticeID: noticeID},
			wantAnswer: "finished safely", wantReady: true, wantLeaf: "next-answer", wantNoticeEntryID: "notice-entry", wantReads: 1,
		},
		{
			name:       "marker superseded without answer",
			client:     &fakeShutdownClient{entries: gwclient.EntriesPage{Entries: []json.RawMessage{notice, nextUser}, LeafID: "next-user"}},
			checkpoint: store.ShutdownCheckpoint{LeafID: "before", NoticeID: noticeID},
			wantLeaf:   "next-user", wantNoticeEntryID: "notice-entry", wantAbandoned: true, wantReads: 1,
		},
		{
			name:       "tool call does not count as a text reply",
			client:     &fakeShutdownClient{entries: gwclient.EntriesPage{Entries: []json.RawMessage{notice, toolEntry}, LeafID: "tool-entry"}},
			checkpoint: store.ShutdownCheckpoint{LeafID: "before", NoticeID: noticeID},
			wantLeaf:   "tool-entry", wantNoticeEntryID: "notice-entry", wantReads: 1,
		},
		{
			name:       "entry fetch failed",
			client:     &fakeShutdownClient{entriesErr: errors.New("gateway offline")},
			checkpoint: store.ShutdownCheckpoint{LeafID: "before", NoticeID: noticeID},
			wantLeaf:   "before", wantReads: 1, wantErr: true,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			answer, ready, leafID, noticeEntryID, abandoned, err := shutdownResponse(context.Background(), test.client, test.checkpoint)
			if test.wantErr {
				if err == nil {
					t.Fatal("shutdownResponse succeeded despite the gateway error")
				}
			} else if err != nil {
				t.Fatalf("shutdownResponse: %v", err)
			}
			if answer != test.wantAnswer || ready != test.wantReady || leafID != test.wantLeaf || noticeEntryID != test.wantNoticeEntryID || abandoned != test.wantAbandoned {
				t.Errorf("shutdownResponse = (%q, %v, %q, %q, %v), want (%q, %v, %q, %q, %v)",
					answer, ready, leafID, noticeEntryID, abandoned,
					test.wantAnswer, test.wantReady, test.wantLeaf, test.wantNoticeEntryID, test.wantAbandoned)
			}
			if test.client.entryReads != test.wantReads {
				t.Errorf("entry reads = %d, want %d", test.client.entryReads, test.wantReads)
			}
		})
	}
}

func TestBoundedShutdownReply(t *testing.T) {
	got := boundedShutdownReply(strings.Repeat("x", shutdownReplyLimit+1))
	if !strings.HasSuffix(got, "_(response truncated)_") {
		t.Fatalf("bounded reply has no truncation marker: %q", got[len(got)-40:])
	}
	if len([]rune(got)) > shutdownReplyLimit+40 {
		t.Errorf("bounded reply has %d runes, too many", len([]rune(got)))
	}
}

func sessionEntry(t *testing.T, id, role, text string) json.RawMessage {
	t.Helper()
	entry, err := json.Marshal(map[string]any{
		"id":   id,
		"type": "message",
		"message": map[string]any{
			"role":    role,
			"content": []map[string]string{{"type": "text", "text": text}},
		},
	})
	if err != nil {
		t.Fatalf("encode session entry: %v", err)
	}
	return entry
}
