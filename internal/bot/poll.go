package bot

import (
	"context"
	"encoding/json"
	"sort"
	"strings"
	"time"

	"github.com/tigersoldier/pi-gateway/gwclient"

	"github.com/tigersoldier/pi-chat/internal/config"
	"github.com/tigersoldier/pi-chat/internal/store"
)

const coldPollTimeout = 5 * time.Second

// pollColdSessions checks a bounded batch of cold sessions for assistant
// entries written since their saved leaf. It polls for at most one hour after
// the idle-close window; older sessions wait for an explicit Slack turn.
func (b *Bot) pollColdSessions(ctx context.Context) {
	rows, err := b.store.Threads(ctx)
	if err != nil {
		b.log.Warn("cannot list threads for cold-session polling", "error", err)
		return
	}
	now := time.Now()
	warmWindow := time.Duration(b.cfg.Concurrency.ThreadIdleCloseMinutes) * time.Minute
	candidates := make([]store.ThreadRow, 0, len(rows))
	valid := make(map[string]struct{}, len(rows))
	for _, row := range rows {
		if !coldPollEligible(row, now, warmWindow) {
			continue
		}
		candidates = append(candidates, row)
		valid[coldPollKey(row)] = struct{}{}
	}
	sort.Slice(candidates, func(i, j int) bool {
		return candidates[i].ThreadKey < candidates[j].ThreadKey
	})
	if len(candidates) == 0 {
		b.mu.Lock()
		clear(b.coldPollAfter)
		b.mu.Unlock()
		return
	}

	b.mu.Lock()
	for key := range b.coldPollAfter {
		if _, ok := valid[key]; !ok {
			delete(b.coldPollAfter, key)
		}
	}
	selected, cursor := selectColdPollBatch(
		candidates, b.coldPollAfter, b.coldPollCursor, now, config.ColdPollBatchSize, warmWindow)
	b.coldPollCursor = cursor
	b.mu.Unlock()

	for _, row := range selected {
		if ctx.Err() != nil {
			return
		}
		t := Thread{Workspace: row.WorkspaceID, Channel: row.ChannelID, ThreadTS: row.ThreadTS}
		th, _ := b.register(t, row)
		if th.pollColdSession(ctx) {
			b.clearColdPoll(row)
		}
	}
}

func coldPollEligible(row store.ThreadRow, now time.Time, warmWindow time.Duration) bool {
	if row.State != store.StateCold || row.SessionPath == "" || row.LastActive.IsZero() {
		return false
	}
	pollUntil := row.LastActive.Add(warmWindow + config.ColdPollWindow)
	return now.Before(pollUntil)
}

func coldPollKey(row store.ThreadRow) string {
	return row.ThreadKey + "\x00" + row.SessionPath
}

// selectColdPollBatch rotates through sorted candidates so a bounded sweep
// does not starve threads later in the key order.
func selectColdPollBatch(
	candidates []store.ThreadRow,
	nextPoll map[string]time.Time,
	cursor string,
	now time.Time,
	limit int,
	warmWindow time.Duration,
) ([]store.ThreadRow, string) {
	if len(candidates) == 0 || limit <= 0 {
		return nil, cursor
	}
	start := sort.Search(len(candidates), func(i int) bool {
		return candidates[i].ThreadKey > cursor
	})
	if start == len(candidates) {
		start = 0
	}
	selected := make([]store.ThreadRow, 0, min(limit, len(candidates)))
	for i := 0; i < len(candidates) && len(selected) < limit; i++ {
		row := candidates[(start+i)%len(candidates)]
		if next := nextPoll[coldPollKey(row)]; next.After(now) {
			continue
		}
		selected = append(selected, row)
		nextPoll[coldPollKey(row)] = now.Add(coldPollIntervalFor(row, now, warmWindow))
	}
	if len(selected) > 0 {
		cursor = selected[len(selected)-1].ThreadKey
	}
	return selected, cursor
}

func coldPollIntervalFor(row store.ThreadRow, now time.Time, warmWindow time.Duration) time.Duration {
	elapsed := now.Sub(row.LastActive.Add(warmWindow))
	if elapsed < config.ColdPollFastWindow {
		return config.ColdPollFastInterval
	}
	return config.ColdPollSlowInterval
}

func (b *Bot) scheduleColdPoll(row store.ThreadRow, now time.Time, warmWindow time.Duration) {
	if row.ThreadKey == "" || row.SessionPath == "" {
		return
	}
	b.mu.Lock()
	b.coldPollAfter[coldPollKey(row)] = now.Add(coldPollIntervalFor(row, now, warmWindow))
	b.mu.Unlock()
}

func (b *Bot) clearColdPoll(row store.ThreadRow) {
	b.mu.Lock()
	delete(b.coldPollAfter, coldPollKey(row))
	b.mu.Unlock()
}

// pollColdSession briefly reattaches to a cold session and checks entries
// beyond the saved leaf. If a turn is already running, or a completed assistant
// answer is found, it hands the session to the ordinary observed-turn renderer
// and leaves the connection warm. Otherwise close saves the new cursor.
func (th *thread) pollColdSession(parent context.Context) bool {
	if !th.turnMu.TryLock() {
		return th.busy() || th.snapshot().State != store.StateCold
	}
	if th.busy() {
		th.turnMu.Unlock()
		return true
	}
	row := th.snapshot()
	if row.State != store.StateCold || row.SessionPath == "" {
		th.turnMu.Unlock()
		return true
	}

	ctx, cancel := context.WithTimeout(parent, coldPollTimeout)
	defer cancel()
	client, err := th.ensure(ctx, "")
	if err != nil {
		th.turnMu.Unlock()
		if ctx.Err() == nil && !isGone(err) {
			th.log.Debug("cold-session poll could not attach", "error", err)
		}
		return th.snapshot().State != store.StateCold
	}
	if th.busy() {
		th.turnMu.Unlock()
		return true
	}
	if _, err := client.GetState(ctx); err != nil {
		th.log.Debug("cold-session poll could not read session state", "error", err)
		th.close(ctx)
		th.turnMu.Unlock()
		return false
	}
	if th.busy() {
		th.turnMu.Unlock()
		return true
	}

	page, err := client.GetEntries(ctx, row.LeafID)
	if err != nil {
		th.log.Debug("cold-session poll could not read new session entries", "error", err)
		th.close(ctx)
		th.turnMu.Unlock()
		return false
	}
	if th.busy() {
		th.turnMu.Unlock()
		return true
	}

	running := client.TurnRunning()
	if running {
		answer, hasAnswer := "", false
		if row.LeafID != "" && page.LeafID != row.LeafID {
			answer, hasAnswer = latestAssistantText(page.Entries)
		} else if row.LeafID == "" {
			answer, hasAnswer = latestAssistantTextAfter(page.Entries, row.LastActive.Add(time.Second))
		}
		th.startPolledObservedTurn(client, answer, hasAnswer)
		return true
	}

	// LastActive is the durable lower bound for assistant entries, including
	// rows whose old cursor did not yet contain a session leaf. The extra second
	// accounts for LastActive's whole-second storage precision.
	answer, hasAnswer := latestAssistantTextAfter(page.Entries, row.LastActive.Add(time.Second))
	if hasAnswer {
		th.startPolledObservedTurn(client, answer, true)
		return true
	}
	if row.LeafID == "" {
		// No safe new answer was found. Seed the process-local baseline, then
		// poll from the leaf returned by GetEntries on the next cycle.
		if text, readErr := client.GetLastAssistantText(ctx); readErr == nil {
			th.rememberAssistant(text)
		}
		th.close(ctx)
		th.turnMu.Unlock()
		return false
	}
	if th.busy() {
		th.turnMu.Unlock()
		return true
	}
	if client.TurnRunning() {
		th.startPolledObservedTurn(client, "", false)
		return true
	}

	th.close(ctx)
	th.turnMu.Unlock()
	return false
}

// startPolledObservedTurn reserves an observed turn while pollColdSession holds
// turnMu, then hands rendering to its ordinary asynchronous worker.
func (th *thread) startPolledObservedTurn(client *gwclient.Client, answer string, hasAnswer bool) {
	st := newTurnState()
	st.renderable.Store(true)
	st.markWork()
	if hasAnswer {
		st.replace(answer)
	}
	if !th.cur.CompareAndSwap(nil, st) {
		th.turnMu.Unlock()
		return
	}
	if !client.TurnRunning() {
		st.markDone()
	}
	th.turnMu.Unlock()
	go th.renderObservedTurn(st, client)
}

// reconcileObservedEntries recovers a completed assistant answer when a
// reconnect lost every delta and no text baseline was available. A changed
// session leaf plus new assistant text proves the answer belongs to new work.
func reconcileObservedEntries(st *turnState, final, previousLeaf, currentLeaf string, entries []json.RawMessage) bool {
	if previousLeaf == "" || currentLeaf == previousLeaf {
		return false
	}
	entryText, ok := latestAssistantText(entries)
	if !ok || entryText == "" {
		return false
	}
	if final == "" {
		final = entryText
	}
	st.replace(final)
	return true
}

// latestAssistantText extracts the newest non-empty assistant text from
// session entries. It stores no transcript content; the returned text is held
// only long enough to hand it to the renderer.
func latestAssistantText(entries []json.RawMessage) (string, bool) {
	var latest string
	for _, raw := range entries {
		_, text, ok := assistantEntryText(raw)
		if ok && text != "" {
			latest = text
		}
	}
	return latest, latest != ""
}

func latestAssistantTextAfter(entries []json.RawMessage, after time.Time) (string, bool) {
	var latest string
	for _, raw := range entries {
		at, text, ok := assistantEntryText(raw)
		if ok && at.After(after) && text != "" {
			latest = text
		}
	}
	return latest, latest != ""
}

func assistantEntryText(raw json.RawMessage) (time.Time, string, bool) {
	var entry struct {
		Type      string `json:"type"`
		Timestamp string `json:"timestamp"`
		Message   struct {
			Role    string          `json:"role"`
			Content json.RawMessage `json:"content"`
		} `json:"message"`
	}
	if err := json.Unmarshal(raw, &entry); err != nil || entry.Type != "message" || entry.Message.Role != "assistant" {
		return time.Time{}, "", false
	}
	at, _ := time.Parse(time.RFC3339Nano, entry.Timestamp)
	return at, contentText(entry.Message.Content), true
}

func contentText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var parts []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if json.Unmarshal(raw, &parts) == nil {
		var b strings.Builder
		for _, part := range parts {
			if part.Type == "text" {
				b.WriteString(part.Text)
			}
		}
		return b.String()
	}
	var text string
	_ = json.Unmarshal(raw, &text)
	return text
}
