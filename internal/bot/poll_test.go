package bot

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/tigersoldier/pi-chat/internal/config"
	"github.com/tigersoldier/pi-chat/internal/store"
)

func TestColdPollEligibilityIsBounded(t *testing.T) {
	now := time.Date(2026, 10, 11, 12, 0, 0, 0, time.UTC)
	warmWindow := time.Hour
	row := store.ThreadRow{
		State:       store.StateCold,
		SessionPath: "/session.jsonl",
		LastActive:  now.Add(-time.Hour - 59*time.Minute),
	}
	if !coldPollEligible(row, now, warmWindow) {
		t.Fatal("a cold session inside its polling window was not eligible")
	}
	row.LastActive = now.Add(-warmWindow - config.ColdPollWindow)
	if coldPollEligible(row, now, warmWindow) {
		t.Fatal("a cold session at the polling cutoff remained eligible")
	}
	row.LastActive = now
	row.State = store.StateWarm
	if coldPollEligible(row, now, warmWindow) {
		t.Fatal("a warm session was eligible for cold polling")
	}
	row.State = store.StateCold
	row.SessionPath = ""
	if coldPollEligible(row, now, warmWindow) {
		t.Fatal("a sessionless thread was eligible for cold polling")
	}
}

func TestSelectColdPollBatchRotatesWithinLimit(t *testing.T) {
	now := time.Now()
	rows := []store.ThreadRow{
		{ThreadKey: "a", SessionPath: "/a"},
		{ThreadKey: "b", SessionPath: "/b"},
		{ThreadKey: "c", SessionPath: "/c"},
		{ThreadKey: "d", SessionPath: "/d"},
	}
	next := make(map[string]time.Time)
	for i := range rows {
		rows[i].LastActive = now
	}
	first, cursor := selectColdPollBatch(rows, next, "", now, 2, 0)
	if len(first) != 2 || first[0].ThreadKey != "a" || first[1].ThreadKey != "b" {
		t.Fatalf("first batch = %#v, want [a b]", first)
	}
	second, cursor := selectColdPollBatch(rows, next, cursor, now.Add(config.ColdPollFastInterval), 2, 0)
	if len(second) != 2 || second[0].ThreadKey != "c" || second[1].ThreadKey != "d" {
		t.Fatalf("second batch = %#v, want [c d]", second)
	}
	if got := coldPollIntervalFor(rows[0], now.Add(config.ColdPollFastWindow), 0); got != config.ColdPollSlowInterval {
		t.Fatalf("interval after fast window = %s, want %s", got, config.ColdPollSlowInterval)
	}
}

func TestLatestAssistantTextAfterUsesEntryTimestamp(t *testing.T) {
	entries := []json.RawMessage{
		json.RawMessage(`{"type":"message","timestamp":"2026-10-11T11:59:58.000Z","message":{"role":"assistant","content":[{"type":"text","text":"old"}]}}`),
		json.RawMessage(`{"type":"message","timestamp":"2026-10-11T12:00:02.000Z","message":{"role":"assistant","content":[{"type":"text","text":"new"}]}}`),
	}
	got, ok := latestAssistantTextAfter(entries, time.Date(2026, 10, 11, 12, 0, 0, 0, time.UTC))
	if !ok || got != "new" {
		t.Fatalf("latestAssistantTextAfter() = %q, %v; want new, true", got, ok)
	}
}

func TestReconcileObservedEntriesUsesNewLeafWhenBaselineIsUnknown(t *testing.T) {
	st := newTurnState()
	entries := []json.RawMessage{
		json.RawMessage(`{"type":"message","message":{"role":"assistant","content":[{"type":"text","text":"result"}]}}`),
	}
	if !reconcileObservedEntries(st, "result", "old-leaf", "new-leaf", entries) {
		t.Fatal("a new assistant entry was not recovered without a text baseline")
	}
	if got := st.text(); got != "result" {
		t.Fatalf("recovered text = %q, want result", got)
	}
	if reconcileObservedEntries(newTurnState(), "result", "old-leaf", "old-leaf", entries) {
		t.Fatal("an unchanged leaf was mistaken for new assistant work")
	}
	withoutAPIText := newTurnState()
	if !reconcileObservedEntries(withoutAPIText, "", "old-leaf", "new-leaf", entries) {
		t.Fatal("new assistant entry text did not recover an empty last-text response")
	}
	if got := withoutAPIText.text(); got != "result" {
		t.Fatalf("entry fallback = %q, want result", got)
	}
}

func TestLatestAssistantText(t *testing.T) {
	entries := []json.RawMessage{
		json.RawMessage(`{"type":"message","message":{"role":"user","content":"hello"}}`),
		json.RawMessage(`{"type":"message","message":{"role":"assistant","content":[{"type":"thinking","thinking":"hidden"}]}}`),
		json.RawMessage(`{"type":"message","message":{"role":"assistant","content":[{"type":"text","text":"old"}]}}`),
		json.RawMessage(`{"type":"message","message":{"role":"assistant","content":[{"type":"text","text":"new "},{"type":"text","text":"answer"}]}}`),
	}
	got, ok := latestAssistantText(entries)
	if !ok || got != "new answer" {
		t.Fatalf("latestAssistantText() = %q, %v; want %q, true", got, ok, "new answer")
	}

	got, ok = latestAssistantText([]json.RawMessage{
		json.RawMessage(`{"type":"message","message":{"role":"assistant","content":[{"type":"thinking","thinking":"hidden"}]}}`),
	})
	if ok || got != "" {
		t.Fatalf("textless assistant entry = %q, %v; want empty, false", got, ok)
	}
}
