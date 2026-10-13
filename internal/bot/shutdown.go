package bot

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/tigersoldier/pi-chat/internal/store"
	"github.com/tigersoldier/pi-gateway/gwclient"
	"github.com/tigersoldier/pi-gateway/protocol"
)

const (
	shutdownNotice        = "pi-chat is shutting down. Do not start unrelated work. If you are already working, finish safely, then give a concise status update with what is done and what remains."
	shutdownReplyPrefix   = "*While pi-chat was restarting, this session replied:*\n\n"
	shutdownReplyLimit    = 8000 // Slack's message limit is in bytes; 8k runes stays below it.
	shutdownNotifyTimeout = 10 * time.Second
	shutdownCheckTimeout  = 2 * time.Second
	shutdownCheckpointTTL = 7 * 24 * time.Hour
)

type shutdownNotifier interface {
	TurnRunning() bool
	Prompt(context.Context, string, ...protocol.ImageContent) (*gwclient.Response, error)
	FollowUp(context.Context, string, ...protocol.ImageContent) (*gwclient.Response, error)
}

type shutdownResponseReader interface {
	TurnRunning() bool
	GetEntries(context.Context, string) (gwclient.EntriesPage, error)
}

type shutdownClient interface {
	shutdownNotifier
	shutdownResponseReader
	LeafID() string
}

type shutdownCheckpointWriter interface {
	PutShutdownCheckpoint(context.Context, store.ShutdownCheckpoint) error
}

// shutdownNoticeText adds a per-shutdown marker so recovery can identify the
// assistant response to this notice rather than a later, unrelated turn.
func shutdownNoticeText(id string) string {
	return shutdownNotice + "\n\n[pi-chat shutdown marker: " + id + "; do not repeat this marker in your response.]"
}

func newShutdownNoticeID() (string, error) {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", fmt.Errorf("generate shutdown notice id: %w", err)
	}
	raw[6] = (raw[6] & 0x0f) | 0x40
	raw[8] = (raw[8] & 0x3f) | 0x80
	hexID := hex.EncodeToString(raw[:])
	return hexID[:8] + "-" + hexID[8:12] + "-" + hexID[12:16] + "-" + hexID[16:20] + "-" + hexID[20:], nil
}

// sendShutdownNotice never steers or aborts work already in progress. An idle
// session gets the notice as a prompt; a running session gets it queued behind
// its current turn.
func sendShutdownNotice(ctx context.Context, client shutdownNotifier, locallyBusy bool, notice string) error {
	if locallyBusy || client.TurnRunning() {
		_, err := client.FollowUp(ctx, notice)
		return err
	}
	_, err := client.Prompt(ctx, notice)
	return err
}

// queueShutdownNotice captures the current leaf and durably records the unique
// notice marker before sending it. It fails closed if either operation fails.
func queueShutdownNotice(ctx context.Context, checkpoints shutdownCheckpointWriter, row store.ThreadRow, client shutdownClient, locallyBusy bool) error {
	page, err := client.GetEntries(ctx, client.LeafID())
	if err != nil {
		return fmt.Errorf("read the current session leaf: %w", err)
	}
	noticeID, err := newShutdownNoticeID()
	if err != nil {
		return err
	}
	checkpoint := store.ShutdownCheckpoint{
		ThreadKey: row.ThreadKey, SessionPath: row.SessionPath,
		LeafID: page.LeafID, NoticeID: noticeID,
	}
	if err := checkpoints.PutShutdownCheckpoint(ctx, checkpoint); err != nil {
		return fmt.Errorf("save shutdown response checkpoint: %w", err)
	}
	if err := sendShutdownNotice(ctx, client, locallyBusy, shutdownNoticeText(noticeID)); err != nil {
		return fmt.Errorf("send shutdown notice: %w", err)
	}
	return nil
}

// shutdownResponse returns only the assistant text that follows this unique
// shutdown notice, stopping at the next user message. It avoids get_state: pi's
// state query is not safe mid-turn.
func shutdownResponse(ctx context.Context, client shutdownResponseReader, checkpoint store.ShutdownCheckpoint) (answer string, ready bool, leafID, noticeEntryID string, abandoned bool, err error) {
	if client.TurnRunning() {
		return "", false, checkpoint.LeafID, checkpoint.NoticeEntryID, false, nil
	}
	page, err := client.GetEntries(ctx, checkpoint.LeafID)
	if err != nil {
		return "", false, checkpoint.LeafID, checkpoint.NoticeEntryID, false, err
	}
	if client.TurnRunning() {
		return "", false, checkpoint.LeafID, checkpoint.NoticeEntryID, false, nil
	}

	start := 0
	noticeEntryID = checkpoint.NoticeEntryID
	if noticeEntryID == "" {
		found := false
		for i, raw := range page.Entries {
			id, role, text, ok := decodeSessionEntry(raw)
			if !ok || role != "user" || !strings.Contains(text, checkpoint.NoticeID) {
				continue
			}
			if id == "" {
				return "", false, checkpoint.LeafID, "", false, nil
			}
			noticeEntryID, start, found = id, i+1, true
			break
		}
		if !found {
			return "", false, page.LeafID, "", false, nil
		}
	}

	for _, raw := range page.Entries[start:] {
		_, role, text, ok := decodeSessionEntry(raw)
		if !ok {
			continue
		}
		if role == "user" {
			if strings.TrimSpace(answer) != "" {
				if client.TurnRunning() {
					return "", false, checkpoint.LeafID, checkpoint.NoticeEntryID, false, nil
				}
				return strings.TrimSpace(answer), true, page.LeafID, noticeEntryID, false, nil
			}
			return "", false, page.LeafID, noticeEntryID, true, nil
		}
		if role == "assistant" && strings.TrimSpace(text) != "" {
			answer = text
		}
	}
	if client.TurnRunning() {
		return "", false, checkpoint.LeafID, checkpoint.NoticeEntryID, false, nil
	}
	if strings.TrimSpace(answer) != "" {
		return strings.TrimSpace(answer), true, page.LeafID, noticeEntryID, false, nil
	}
	return "", false, page.LeafID, noticeEntryID, false, nil
}

func decodeSessionEntry(raw json.RawMessage) (id, role, text string, ok bool) {
	var entry struct {
		ID      string `json:"id"`
		Message struct {
			Role    string          `json:"role"`
			Content json.RawMessage `json:"content"`
		} `json:"message"`
	}
	if json.Unmarshal(raw, &entry) != nil {
		return "", "", "", false
	}
	if json.Unmarshal(entry.Message.Content, &text) == nil {
		return entry.ID, entry.Message.Role, text, true
	}
	var blocks []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if json.Unmarshal(entry.Message.Content, &blocks) != nil {
		return entry.ID, entry.Message.Role, "", true
	}
	var out strings.Builder
	for _, block := range blocks {
		if block.Type == "text" {
			out.WriteString(block.Text)
		}
	}
	return entry.ID, entry.Message.Role, out.String(), true
}

// CheckShutdownResponses reconnects to sessions that received a shutdown
// notice and relays any answer that completed while pi-chat was offline. A
// running session or a transient gateway/Slack failure remains pending and is
// checked again by the maintenance loop.
func (b *Bot) CheckShutdownResponses(ctx context.Context) {
	checkpoints, err := b.store.ShutdownCheckpoints(ctx)
	if err != nil {
		b.log.Warn("cannot list shutdown response checks", "error", err)
		return
	}
	for _, checkpoint := range checkpoints {
		if ctx.Err() != nil {
			return
		}
		if checkpoint.Posted || checkpoint.NoticeID == "" {
			b.deleteShutdownCheckpoint(ctx, checkpoint)
			continue
		}
		if time.Since(checkpoint.CreatedAt) > shutdownCheckpointTTL {
			b.deleteShutdownCheckpoint(ctx, checkpoint)
			b.log.Info("expired a shutdown response check", "thread", checkpoint.ThreadKey)
			continue
		}
		row, found, err := b.store.Thread(ctx, checkpoint.ThreadKey)
		if err != nil {
			b.log.Warn("cannot load a shutdown session", "thread", checkpoint.ThreadKey, "error", err)
			continue
		}
		if !found || row.SessionPath != checkpoint.SessionPath || row.State == store.StateDeleted {
			b.deleteShutdownCheckpoint(ctx, checkpoint)
			continue
		}

		t := Thread{
			Workspace: row.WorkspaceID,
			Channel:   row.ChannelID,
			ThreadTS:  row.ThreadTS,
			Surface:   row.Surface,
		}
		th, ok := b.tracked(t)
		if !ok || !th.turnMu.TryLock() {
			continue
		}
		if th.snapshot().SessionPath != checkpoint.SessionPath {
			b.deleteShutdownCheckpoint(ctx, checkpoint)
			th.turnMu.Unlock()
			continue
		}
		b.checkShutdownResponse(ctx, checkpoint, t, th)
		th.turnMu.Unlock()
	}
}

func (b *Bot) checkShutdownResponse(ctx context.Context, checkpoint store.ShutdownCheckpoint, t Thread, th *thread) {
	checkCtx, cancel := context.WithTimeout(ctx, shutdownCheckTimeout)
	defer cancel()

	th.connMu.Lock()
	client := th.client
	if client != nil && (client.Err() != nil || !th.bound(client)) {
		_ = client.Close()
		th.client = nil
		client = nil
	}
	temporary := false
	var err error
	if client == nil {
		client, err = th.dialSession(checkCtx, checkpoint.SessionPath)
		temporary = err == nil
	}
	th.connMu.Unlock()
	if err != nil {
		if isGone(err) {
			b.deleteShutdownCheckpoint(checkCtx, checkpoint)
			return
		}
		b.log.Debug("cannot reattach a shutdown session", "thread", checkpoint.ThreadKey, "error", err)
		return
	}
	if temporary {
		defer closeShutdownClient(client)
	}
	if !th.bound(client) {
		b.deleteShutdownCheckpoint(checkCtx, checkpoint)
		return
	}
	answer, ready, leafID, noticeEntryID, abandoned, err := shutdownResponse(checkCtx, client, checkpoint)
	if err != nil {
		if isGone(err) {
			b.deleteShutdownCheckpoint(checkCtx, checkpoint)
			return
		}
		b.log.Debug("cannot inspect a shutdown session response", "thread", checkpoint.ThreadKey, "error", err)
		return
	}
	if abandoned {
		b.deleteShutdownCheckpoint(checkCtx, checkpoint)
		b.log.Info("cleared a shutdown response check superseded before reply", "thread", checkpoint.ThreadKey)
		return
	}
	if !ready {
		if leafID != checkpoint.LeafID || noticeEntryID != checkpoint.NoticeEntryID {
			if err := b.store.AdvanceShutdownCheckpoint(checkCtx, checkpoint.ThreadKey, checkpoint.SessionPath, checkpoint.NoticeID, leafID, noticeEntryID); err != nil {
				b.log.Warn("cannot advance a shutdown response check", "thread", checkpoint.ThreadKey, "error", err)
			}
		}
		return
	}

	text := shutdownReplyPrefix + boundedShutdownReply(answer)
	if err := b.plat.Post(checkCtx, Notice{Thread: &t, Text: text}); err != nil {
		b.log.Warn("cannot post a shutdown session response", "thread", checkpoint.ThreadKey, "error", err)
		return
	}
	persistCtx, cancelPersist := context.WithTimeout(context.WithoutCancel(ctx), storeTimeout)
	defer cancelPersist()
	if err := b.store.MarkShutdownCheckpointPosted(persistCtx, checkpoint.ThreadKey, checkpoint.SessionPath, checkpoint.NoticeID); err != nil {
		b.log.Warn("cannot record a posted shutdown response", "thread", checkpoint.ThreadKey, "error", err)
	}
	th.saveCursor(checkCtx, client)
	b.deleteShutdownCheckpoint(persistCtx, checkpoint)
	b.log.Info("posted a response from a session active during shutdown", "thread", checkpoint.ThreadKey)
}

func closeShutdownClient(client *gwclient.Client) {
	_ = client.Close()
}

func (b *Bot) deleteShutdownCheckpoint(ctx context.Context, checkpoint store.ShutdownCheckpoint) {
	if err := b.store.DeleteShutdownCheckpoint(ctx, checkpoint.ThreadKey, checkpoint.SessionPath, checkpoint.NoticeID); err != nil {
		b.log.Warn("cannot clear a shutdown response check", "thread", checkpoint.ThreadKey, "error", err)
	}
}

func boundedShutdownReply(answer string) string {
	runes := []rune(answer)
	if len(runes) <= shutdownReplyLimit {
		return answer
	}
	return string(runes[:shutdownReplyLimit]) + "\n\n_(response truncated)_"
}

// notifySessionsOfShutdown gives each warm session a chance to finish its
// current work and leaves a durable cursor for the next process to check.
func (b *Bot) notifySessionsOfShutdown(ctx context.Context) {
	for _, th := range b.allThreads() {
		if ctx.Err() != nil {
			return
		}
		row := th.snapshot()
		if row.State != store.StateWarm || row.SessionPath == "" {
			continue
		}
		client := th.live()
		if client == nil || !th.bound(client) {
			continue
		}

		if err := queueShutdownNotice(ctx, b.store, row, client, th.busy()); err != nil {
			b.log.Warn("cannot checkpoint or notify a session of shutdown", "thread", row.ThreadKey, "error", err)
			continue
		}
		b.log.Info("notified a session of shutdown", "thread", row.ThreadKey)
	}
}
