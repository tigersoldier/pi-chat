package bot

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tigersoldier/pi-chat/internal/config"
	"github.com/tigersoldier/pi-chat/internal/store"
	"github.com/tigersoldier/pi-gateway/gwclient"
	"github.com/tigersoldier/pi-gateway/protocol"
)

// discardLogger keeps test output quiet.
func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// textDelta builds the message_update pi sends for streamed assistant text.
func textDelta(t *testing.T, s string) gwclient.Event {
	t.Helper()
	delta, err := json.Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	raw := `{"type":"message_update","assistantMessageEvent":{"type":"text_delta","contentIndex":0,"delta":` + string(delta) + `}}`
	return gwclient.Event{Type: "message_update", Raw: []byte(raw)}
}

func toolEvent(t *testing.T, typ, callID, name string, isError bool) gwclient.Event {
	t.Helper()
	raw, err := json.Marshal(struct {
		Type       string `json:"type"`
		ToolCallID string `json:"toolCallId"`
		ToolName   string `json:"toolName"`
		IsError    bool   `json:"isError,omitempty"`
	}{Type: typ, ToolCallID: callID, ToolName: name, IsError: isError})
	if err != nil {
		t.Fatal(err)
	}
	return gwclient.Event{Type: typ, Raw: raw}
}

// messageEnd builds the message_end pi sends for a completed message of any
// role, including the stop reason and error text.
func messageEnd(t *testing.T, role, stopReason, errorMessage string) gwclient.Event {
	t.Helper()
	raw, err := json.Marshal(map[string]any{
		"type": "message_end",
		"message": map[string]any{
			"role":         role,
			"content":      []any{},
			"stopReason":   stopReason,
			"errorMessage": errorMessage,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	return gwclient.Event{Type: "message_end", Raw: raw}
}

func assistantMessageEnd(t *testing.T, stopReason, errorMessage string) gwclient.Event {
	t.Helper()
	return messageEnd(t, "assistant", stopReason, errorMessage)
}

// gatewayEvent builds one pi/gateway event with the supplied fields.
func gatewayEvent(t *testing.T, typ string, fields map[string]any) gwclient.Event {
	t.Helper()
	payload := map[string]any{"type": typ}
	for key, value := range fields {
		payload[key] = value
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	return gwclient.Event{Type: typ, Raw: raw}
}

func lastProgressTask(updates []TaskProgress, id string) (TaskProgress, bool) {
	var last TaskProgress
	found := false
	for _, update := range updates {
		if update.ID == id {
			last, found = update, true
		}
	}
	return last, found
}

func TestTurnStateAccumulatesTextDeltas(t *testing.T) {
	st := newTurnState()
	log := discardLogger()

	st.feed(textDelta(t, "Hel"), log)
	st.feed(textDelta(t, "lo"), log)

	if got := st.takePending(); got != "Hello" {
		t.Fatalf("pending text = %q, want %q", got, "Hello")
	}
	if got := st.takePending(); got != "" {
		t.Fatalf("takePending did not drain: %q", got)
	}
	if got := st.text(); got != "Hello" {
		t.Fatalf("full text = %q, want %q", got, "Hello")
	}
	if !st.sawWork() {
		t.Fatal("a text delta should count as the turn having started")
	}
}

func TestTurnStateTracksToolTaskProgressWithoutArgumentsOrOutput(t *testing.T) {
	st := newTurnState()
	log := discardLogger()
	startedAt := time.Unix(1_000, 0)
	st.progressStartedAt = startedAt

	start := toolEvent(t, "tool_execution_start", "call-1", "bash", false)
	start.Raw = []byte(`{"type":"tool_execution_start","toolCallId":"call-1","toolName":"bash","args":{"command":"do-not-share"}}`)
	st.feed(start, log)
	end := toolEvent(t, "tool_execution_end", "call-1", "bash", true)
	end.Raw = []byte(`{"type":"tool_execution_end","toolCallId":"call-1","toolName":"bash","isError":true,"result":{"secret":"do-not-share"}}`)
	st.feed(end, log)

	updates := st.takeTaskUpdates()
	if len(updates) != 1 || updates[0].ID != turnProgressTaskID || updates[0].Title != "Running a command" || updates[0].Status != TaskInProgress || updates[0].Details != "" {
		t.Fatalf("in-progress task update = %+v, want no rolling details", updates)
	}
	if strings.Contains(updates[0].Title, "do-not-share") {
		t.Fatal("tool arguments or output leaked into the progress title")
	}
	if updates := st.takeTaskUpdates(); len(updates) != 0 {
		t.Fatalf("tool completion should not create a second task: %+v", updates)
	}

	st.finishProgressTaskAt(TaskComplete, startedAt.Add(42*time.Second))
	updates = st.takeTaskUpdates()
	if len(updates) != 1 || updates[0].ID != turnProgressTaskID || updates[0].Title != "Completed · 1 action · 42s" || updates[0].Status != TaskComplete {
		t.Fatalf("turn completion update = %+v", updates)
	}
}

func TestProviderFailureReplacesTheGenericNoAnswerError(t *testing.T) {
	st := newTurnState()
	st.markWork()
	log := discardLogger()
	st.feed(assistantMessageEnd(t, "error", "Codex error: The usage limit has been reached"), log)

	err := st.turnFailure(false)
	if err == nil || !strings.Contains(err.Error(), "usage limit") {
		t.Fatalf("noAnswerError = %v, want the provider's own message", err)
	}
	if strings.Contains(err.Error(), "produced no answer") {
		t.Fatalf("the generic no-answer text hid the provider failure: %v", err)
	}

	// A later successful assistant message means a retry succeeded; the stale
	// error must not describe the turn.
	st.feed(assistantMessageEnd(t, "stop", ""), log)
	if err := st.turnFailure(false); !strings.Contains(err.Error(), "produced no answer") {
		t.Fatalf("after a successful message noAnswerError = %v, want the generic text", err)
	}

	// An error with no provider text still says the request failed; it does not
	// pretend the agent simply had nothing to say.
	bare := newTurnState()
	bare.markWork()
	bare.feed(assistantMessageEnd(t, "error", ""), log)
	if got := bare.turnFailure(false).Error(); got != "the model request failed" {
		t.Fatalf("turnFailure without detail = %q, want the request failure", got)
	}
}

func TestProviderErrorAfterTextIsStillAFailure(t *testing.T) {
	st := newTurnState()
	log := discardLogger()
	st.feed(textDelta(t, "partial answer"), log)
	st.feed(assistantMessageEnd(t, "error", "Codex error: The usage limit has been reached"), log)

	if err := st.turnFailure(true); err == nil || !strings.Contains(err.Error(), "usage limit") {
		t.Fatalf("turnFailure = %v, want the provider error despite text", err)
	}
	if got := st.doneProgressStatus(); got != TaskError {
		t.Fatalf("task status = %q, want incomplete", got)
	}
}

func TestTurnOutcomeReportsTruncation(t *testing.T) {
	st := newTurnState()
	log := discardLogger()
	st.feed(textDelta(t, "partial answer"), log)
	st.feed(assistantMessageEnd(t, "length", ""), log)

	if err := st.turnFailure(true); err == nil || !strings.Contains(err.Error(), "output limit") {
		t.Fatalf("turnFailure = %v, want the output-limit failure", err)
	}
	if got := st.doneProgressStatus(); got != TaskError {
		t.Fatalf("task status = %q, want incomplete", got)
	}
}

func TestTurnOutcomeReportsAborted(t *testing.T) {
	t.Run("without text", func(t *testing.T) {
		st := newTurnState()
		st.feed(assistantMessageEnd(t, "aborted", ""), discardLogger())
		st.feed(gatewayEvent(t, "agent_settled", map[string]any{"aborted": true}), discardLogger())

		if err := st.terminalFailure(false); err == nil || !strings.Contains(err.Error(), "stopped") {
			t.Fatalf("terminalFailure = %v, want the abort message", err)
		}
		if got := st.doneProgressStatus(); got != TaskError {
			t.Fatalf("task status = %q, want incomplete", got)
		}
	})

	t.Run("via the settled flag", func(t *testing.T) {
		st := newTurnState()
		st.markWork()
		st.feed(gatewayEvent(t, "agent_settled", map[string]any{"aborted": true}), discardLogger())

		if err := st.terminalFailure(false); err == nil || !strings.Contains(err.Error(), "stopped") {
			t.Fatalf("terminalFailure = %v, want the abort message", err)
		}
	})

	t.Run("with partial text", func(t *testing.T) {
		st := newTurnState()
		log := discardLogger()
		st.feed(textDelta(t, "partial answer"), log)
		st.feed(assistantMessageEnd(t, "aborted", ""), log)
		st.feed(gatewayEvent(t, "agent_settled", map[string]any{"aborted": true}), log)

		if err := st.turnFailure(true); err != nil {
			t.Fatalf("turnFailure = %v, want the partial answer to finish", err)
		}
		if got := st.doneProgressStatus(); got != TaskError {
			t.Fatalf("task status = %q, want incomplete", got)
		}
	})
}

func TestTurnOutcomeReportsASessionCrash(t *testing.T) {
	st := newTurnState()
	log := discardLogger()
	st.feed(textDelta(t, "partial answer"), log)
	st.feed(gatewayEvent(t, "gw_session_state", map[string]any{
		"state": "crashed", "reason": "exit", "exitCode": 2,
	}), log)
	st.feed(assistantMessageEnd(t, "stop", ""), log)
	st.feed(gatewayEvent(t, "agent_settled", nil), log)

	err := st.turnFailure(true)
	if err == nil || !strings.Contains(err.Error(), "crashed") {
		t.Fatalf("turnFailure = %v, want the session crash", err)
	}
	if got := PublicMessage(err); got != "the session process crashed while this turn was running" {
		t.Fatalf("public message = %q, want the crash without its cause", got)
	}
	if strings.Contains(PublicMessage(err), "exit code") || !strings.Contains(err.Error(), "exit code 2") {
		t.Fatalf("cause visibility = %q, want exit code only in the log-facing error", err)
	}
	if got := st.doneProgressStatus(); got != TaskError {
		t.Fatalf("task status = %q, want incomplete", got)
	}
}

func TestTurnOutcomeFallsBackToRetryError(t *testing.T) {
	st := newTurnState()
	st.markWork()
	log := discardLogger()
	st.feed(gatewayEvent(t, "auto_retry_start", map[string]any{"errorMessage": "529 overloaded"}), log)
	st.feed(gatewayEvent(t, "auto_retry_end", map[string]any{"success": false, "finalError": "529 overloaded"}), log)

	if err := st.turnFailure(false); err == nil || !strings.Contains(err.Error(), "after retries") {
		t.Fatalf("turnFailure = %v, want the retry failure", err)
	}
}

func TestTurnOutcomeFallsBackToCompactionError(t *testing.T) {
	st := newTurnState()
	st.markWork()
	st.feed(gatewayEvent(t, "compaction_end", map[string]any{"errorMessage": "summary failed"}), discardLogger())

	if err := st.turnFailure(false); err == nil || !strings.Contains(err.Error(), "compacted") {
		t.Fatalf("turnFailure = %v, want the compaction failure", err)
	}
}

func TestTurnOutcomeFallsBackToExtensionError(t *testing.T) {
	st := newTurnState()
	st.markWork()
	st.feed(gatewayEvent(t, "extension_error", map[string]any{"event": "tool_call", "error": "boom"}), discardLogger())

	err := st.turnFailure(false)
	if err == nil || !strings.Contains(err.Error(), "extension failed") || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("turnFailure = %v, want the extension failure", err)
	}
	if got := PublicMessage(err); got != "an extension failed during this turn" {
		t.Fatalf("public message = %q, want the extension detail out of chat", got)
	}
}

func TestTurnOutcomeFallsBackToGatewayError(t *testing.T) {
	st := newTurnState()
	st.markWork()
	st.feed(gatewayEvent(t, "gw_error", map[string]any{"code": "session_crashed", "message": "pi died"}), discardLogger())

	if err := st.turnFailure(false); err == nil || !strings.Contains(err.Error(), "gateway") || !strings.Contains(err.Error(), "pi died") {
		t.Fatalf("turnFailure = %v, want the gateway failure", err)
	}
}

func TestGatewayErrorWithARequestIDIsIgnored(t *testing.T) {
	st := newTurnState()
	st.markWork()
	st.feed(gatewayEvent(t, "gw_error", map[string]any{
		"code": "bad_request", "message": "not this turn", "id": "r_1",
	}), discardLogger())

	if err := st.turnFailure(false); !strings.Contains(err.Error(), "produced no answer") {
		t.Fatalf("turnFailure = %v, want the request-scoped gw_error ignored", err)
	}
}

func TestSessionCrashCauseSurvivesLaterSessionStates(t *testing.T) {
	st := newTurnState()
	st.feed(gatewayEvent(t, "gw_session_state", map[string]any{
		"state": "crashed", "reason": "exit", "exitCode": 2,
	}), discardLogger())
	st.feed(gatewayEvent(t, "gw_session_state", map[string]any{"state": "restarting"}), discardLogger())
	st.feed(gatewayEvent(t, "gw_session_state", map[string]any{"state": "stopped"}), discardLogger())

	err := st.turnFailure(false)
	if err == nil || PublicMessage(err) != "the session process crashed while this turn was running" {
		t.Fatalf("turnFailure = %v, want the original crash", err)
	}
	if !strings.Contains(err.Error(), "exit code 2") {
		t.Fatalf("turnFailure = %q, want the crash exit code kept for the log", err)
	}
}

func TestTurnOutcomeUsesAgentEndWhenMessageEndIsMissing(t *testing.T) {
	st := newTurnState()
	st.markWork()
	st.feed(gatewayEvent(t, "agent_end", map[string]any{
		"messages": []any{map[string]any{
			"role": "assistant", "stopReason": "error", "errorMessage": "from agent end",
		}},
	}), discardLogger())

	if err := st.turnFailure(false); err == nil || !strings.Contains(err.Error(), "from agent end") {
		t.Fatalf("turnFailure = %v, want the agent_end fallback", err)
	}
}

func TestTurnOutcomeIgnoresRecoveredNoise(t *testing.T) {
	st := newTurnState()
	log := discardLogger()
	st.feed(textDelta(t, "answer"), log)
	st.feed(gatewayEvent(t, "auto_retry_start", map[string]any{"errorMessage": "529 overloaded"}), log)
	st.feed(gatewayEvent(t, "auto_retry_end", map[string]any{"success": true}), log)
	st.feed(gatewayEvent(t, "extension_error", map[string]any{"event": "tool_call", "error": "boom"}), log)
	st.feed(assistantMessageEnd(t, "stop", ""), log)

	if err := st.turnFailure(true); err != nil {
		t.Fatalf("turnFailure = %v, want a successful answer", err)
	}
	if got := st.doneProgressStatus(); got != TaskComplete {
		t.Fatalf("task status = %q, want complete", got)
	}
}

func TestTurnOutcomeReportsAReadError(t *testing.T) {
	st := newTurnState()
	st.recordReadError(errors.New("read failed"))

	if err := st.turnFailure(false); err == nil || !strings.Contains(err.Error(), "read the agent's answer") {
		t.Fatalf("turnFailure = %v, want the read failure", err)
	}
}

func TestSessionCrashEndsTheTurn(t *testing.T) {
	st := newTurnState()
	st.feed(gatewayEvent(t, "gw_session_state", map[string]any{
		"state": "crashed", "reason": "exit", "exitCode": 2,
	}), discardLogger())

	select {
	case <-st.doneCh:
	default:
		t.Fatal("a session crash did not end the turn")
	}
	if !st.hardSessionFailure() {
		t.Fatal("the crash was not recorded as a hard session failure")
	}
	err := st.turnFailure(false)
	if err == nil || !strings.Contains(PublicMessage(err), "crashed") {
		t.Fatalf("turnFailure = %v, want the crash", err)
	}
	if !strings.Contains(err.Error(), "exit code 2") {
		t.Fatalf("turnFailure = %q, want the exit code in the logged cause", err)
	}
}

func TestStopAfterRetryFailureReportsTheStop(t *testing.T) {
	st := newTurnState()
	st.markWork()
	st.feed(assistantMessageEnd(t, "error", "529 overloaded"), discardLogger())
	st.feed(gatewayEvent(t, "agent_settled", map[string]any{"aborted": true}), discardLogger())

	err := st.turnFailure(false)
	if err == nil || !strings.Contains(PublicMessage(err), "stopped") {
		t.Fatalf("turnFailure = %v, want the user stop", err)
	}
	if strings.Contains(PublicMessage(err), "model request") {
		t.Fatalf("the stop was reported as a provider error: %v", err)
	}
}

func TestFailedProviderTurnEndsTheTaskAsIncomplete(t *testing.T) {
	for _, tc := range []struct {
		name       string
		stopReason string
		errorText  string
		streamText string
		want       TaskProgressStatus
	}{
		{"provider error", "error", "Codex error: The usage limit has been reached", "", TaskError},
		{"answer", "stop", "", "", TaskComplete},
		// A terminal provider error is a failure even when text was streamed;
		// Fail preserves that text and appends the warning.
		{"provider error after text", "error", "Codex error: The usage limit has been reached", "I started on it", TaskError},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st := newTurnState()
			log := discardLogger()
			st.feed(toolEvent(t, "tool_execution_start", "call-1", "bash", false), log)
			if updates := st.takeTaskUpdates(); len(updates) != 1 || updates[0].Status != TaskInProgress {
				t.Fatalf("in-progress updates = %+v", updates)
			}
			if tc.streamText != "" {
				st.feed(textDelta(t, tc.streamText), log)
			}

			st.feed(assistantMessageEnd(t, tc.stopReason, tc.errorText), log)
			st.feed(gwclient.Event{Type: "agent_settled"}, log)

			updates := st.takeTaskUpdates()
			if len(updates) != 1 || updates[0].Status != tc.want {
				t.Fatalf("terminal updates = %+v, want status %q", updates, tc.want)
			}
			if tc.want == TaskError && !strings.Contains(updates[0].Title, "Incomplete") {
				t.Fatalf("terminal title = %q, want an incomplete task", updates[0].Title)
			}
		})
	}
}

func TestProviderFailureTextIsCompacted(t *testing.T) {
	st := newTurnState()
	st.markWork()
	detail := "\x1b[31mCodex error:\nusage limit\x1b[0m " + strings.Repeat("x", maxTurnErrorRunes+50)
	st.feed(assistantMessageEnd(t, "error", detail), discardLogger())

	got := st.turnFailure(false).Error()
	if strings.ContainsAny(got, "\x1b\n") {
		t.Fatalf("failure text kept ANSI or a newline: %q", got)
	}
	if want := len([]rune("the model request failed: ")) + maxTurnErrorRunes; len([]rune(got)) > want {
		t.Fatalf("failure text is %d runes, want at most %d", len([]rune(got)), want)
	}
}

func TestNonAssistantMessageEndDoesNotClobberTheFailure(t *testing.T) {
	st := newTurnState()
	st.markWork()
	log := discardLogger()
	st.feed(assistantMessageEnd(t, "error", "Codex error: The usage limit has been reached"), log)
	st.feed(messageEnd(t, "toolResult", "error", "the tool failed"), log)

	if got := st.turnFailure(false).Error(); !strings.Contains(got, "usage limit") {
		t.Fatalf("turnFailure = %q, want the assistant failure", got)
	}
}

// TestRunTurnSurfacesTheProviderFailure exercises the production path from the
// gateway event to the error startTurn reports, not just the helper.
func TestRunTurnSurfacesTheProviderFailure(t *testing.T) {
	b, platform, st := newTestBot(t)
	th := testThread(t, b, st)
	client := startTurnGateway(t, th, func(enc *json.Encoder) {
		_ = enc.Encode(map[string]any{"type": "agent_start"})
		_ = enc.Encode(map[string]any{
			"type": "message_end",
			"message": map[string]any{
				"role":         "assistant",
				"content":      []any{},
				"stopReason":   "error",
				"errorMessage": "Codex error: The usage limit has been reached",
			},
		})
		_ = enc.Encode(map[string]any{"type": "agent_settled"})
	})
	th.client = client
	th.update(func(r *store.ThreadRow) { r.SessionPath = "/test-session.jsonl" })

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	err := th.runTurn(ctx, Message{TS: "1.2", Text: "hello"}, &recordingRenderer{})
	if err == nil || !strings.Contains(PublicMessage(err), "usage limit") {
		t.Fatalf("runTurn public message = %v, want the provider's message", err)
	}
	if strings.Contains(err.Error(), "produced no answer") {
		t.Fatalf("runTurn kept the generic no-answer text: %v", err)
	}
	if completed := platform.completedRequests(); len(completed) != 0 {
		t.Errorf("completed requests = %+v, want none after a failed turn", completed)
	}
}

func TestRunTurnReportsAProviderFailureAfterPartialText(t *testing.T) {
	b, platform, st := newTestBot(t)
	th := testThread(t, b, st)
	client := startTurnGateway(t, th, func(enc *json.Encoder) {
		_ = enc.Encode(map[string]any{"type": "agent_start"})
		_ = enc.Encode(map[string]any{
			"type": "message_update",
			"assistantMessageEvent": map[string]any{
				"type": "text_delta", "contentIndex": 0, "delta": "partial answer",
			},
		})
		_ = enc.Encode(map[string]any{
			"type": "message_end",
			"message": map[string]any{
				"role":         "assistant",
				"content":      []any{},
				"stopReason":   "error",
				"errorMessage": "Codex error: The usage limit has been reached",
			},
		})
		_ = enc.Encode(map[string]any{"type": "agent_settled"})
	})
	th.client = client
	th.update(func(r *store.ThreadRow) { r.SessionPath = "/test-session.jsonl" })

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := th.runTurn(ctx, Message{TS: "1.2", Text: "hello"}, &recordingRenderer{}); err == nil || !strings.Contains(PublicMessage(err), "usage limit") {
		t.Fatalf("runTurn public message = %v, want the provider failure after partial text", err)
	}
	if completed := platform.completedRequests(); len(completed) != 0 {
		t.Errorf("completed requests = %+v, want none after a failed turn", completed)
	}
}

func TestRunTurnContinuesAfterProgressMarkerOnlyStop(t *testing.T) {
	b, platform, st := newTestBot(t)
	th := testThread(t, b, st)
	var mu sync.Mutex
	var prompts []string
	client := startTurnGatewayWithLastText(t, th, func(enc *json.Encoder, prompt int, text string) string {
		mu.Lock()
		prompts = append(prompts, text)
		mu.Unlock()
		_ = enc.Encode(map[string]any{"type": "agent_start"})
		switch prompt {
		case 1:
			_ = enc.Encode(map[string]any{
				"type": "message_update",
				"assistantMessageEvent": map[string]any{
					"type": "text_delta", "contentIndex": 0,
					"delta": "[[slack-progress: Checking service logs]]",
				},
			})
			_ = enc.Encode(map[string]any{
				"type": "message_end",
				"message": map[string]any{
					"role": "assistant", "content": []any{}, "stopReason": "stop",
				},
			})
			_ = enc.Encode(map[string]any{"type": "agent_settled"})
			return "[[slack-progress: Checking service logs]]"
		case 2:
			_ = enc.Encode(map[string]any{
				"type": "message_update",
				"assistantMessageEvent": map[string]any{
					"type": "text_delta", "contentIndex": 0, "delta": "Finished the task.",
				},
			})
			_ = enc.Encode(map[string]any{
				"type": "message_end",
				"message": map[string]any{
					"role": "assistant", "content": []any{}, "stopReason": "stop",
				},
			})
			_ = enc.Encode(map[string]any{"type": "agent_settled"})
			return "Finished the task."
		default:
			t.Errorf("unexpected prompt %d", prompt)
			return ""
		}
	})
	th.client = client
	th.update(func(r *store.ThreadRow) { r.SessionPath = "/test-session.jsonl" })

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	renderer := &recordingRenderer{}
	if err := th.runTurn(ctx, Message{TS: "1.2", Text: "hello"}, renderer); err != nil {
		t.Fatalf("runTurn = %v, want the answer from the continuation", err)
	}
	if got := renderer.finalAnswers(); len(got) != 1 || got[0] != "Finished the task." {
		t.Fatalf("final answers = %q, want the continuation answer", got)
	}
	if completed := platform.completedRequests(); len(completed) != 1 {
		t.Fatalf("completed requests = %+v, want one completed user turn", completed)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(prompts) != 2 || prompts[1] != progressMarkerContinuationText {
		t.Fatalf("prompts = %q, want the user prompt and one continuation", prompts)
	}
}

func TestRunTurnStopsRetryingAfterProgressMarkerLimit(t *testing.T) {
	b, platform, st := newTestBot(t)
	th := testThread(t, b, st)
	var mu sync.Mutex
	prompts := 0
	marker := "[[slack-progress: Checking service logs]]"
	client := startTurnGatewayWithLastText(t, th, func(enc *json.Encoder, _ int, _ string) string {
		mu.Lock()
		prompts++
		mu.Unlock()
		_ = enc.Encode(map[string]any{"type": "agent_start"})
		_ = enc.Encode(map[string]any{
			"type": "message_update",
			"assistantMessageEvent": map[string]any{
				"type": "text_delta", "contentIndex": 0, "delta": marker,
			},
		})
		_ = enc.Encode(map[string]any{
			"type": "message_end",
			"message": map[string]any{
				"role": "assistant", "content": []any{}, "stopReason": "stop",
			},
		})
		_ = enc.Encode(map[string]any{"type": "agent_settled"})
		return marker
	})
	th.client = client
	th.update(func(r *store.ThreadRow) { r.SessionPath = "/test-session.jsonl" })

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	err := th.runTurn(ctx, Message{TS: "1.2", Text: "hello"}, &recordingRenderer{})
	if err == nil || !strings.Contains(err.Error(), "produced no answer") {
		t.Fatalf("runTurn = %v, want the no-answer error after the retry limit", err)
	}
	if completed := platform.completedRequests(); len(completed) != 0 {
		t.Errorf("completed requests = %+v, want none when the retry limit is exhausted", completed)
	}
	mu.Lock()
	defer mu.Unlock()
	if want := maxConsecutiveProgressMarkerRetries + 1; prompts != want {
		t.Fatalf("prompts = %d, want %d bounded attempts", prompts, want)
	}
}

func TestRunTurnContinuesAfterProgressMarkerStopsWithToolWorkBetweenThem(t *testing.T) {
	b, platform, st := newTestBot(t)
	th := testThread(t, b, st)
	var mu sync.Mutex
	prompts := 0
	marker := "[[slack-progress: Checking service logs]]"
	const answer = "Finished after several tool calls."
	stops := maxConsecutiveProgressMarkerRetries + 1
	client := startTurnGatewayWithLastText(t, th, func(enc *json.Encoder, prompt int, _ string) string {
		mu.Lock()
		prompts++
		mu.Unlock()
		_ = enc.Encode(map[string]any{"type": "agent_start"})
		if prompt <= stops {
			callID := fmt.Sprintf("call-%d", prompt)
			_ = enc.Encode(map[string]any{
				"type": "tool_execution_start", "toolCallId": callID, "toolName": "bash",
			})
			_ = enc.Encode(map[string]any{
				"type": "tool_execution_end", "toolCallId": callID, "toolName": "bash",
			})
			_ = enc.Encode(map[string]any{
				"type": "message_update",
				"assistantMessageEvent": map[string]any{
					"type": "text_delta", "contentIndex": 0, "delta": marker,
				},
			})
			_ = enc.Encode(map[string]any{
				"type": "message_end",
				"message": map[string]any{
					"role": "assistant", "content": []any{}, "stopReason": "stop",
				},
			})
			_ = enc.Encode(map[string]any{"type": "agent_settled"})
			return marker
		}
		_ = enc.Encode(map[string]any{
			"type": "message_update",
			"assistantMessageEvent": map[string]any{
				"type": "text_delta", "contentIndex": 0, "delta": answer,
			},
		})
		_ = enc.Encode(map[string]any{
			"type": "message_end",
			"message": map[string]any{
				"role": "assistant", "content": []any{}, "stopReason": "stop",
			},
		})
		_ = enc.Encode(map[string]any{"type": "agent_settled"})
		return answer
	})
	th.client = client
	th.update(func(r *store.ThreadRow) { r.SessionPath = "/test-session.jsonl" })

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	renderer := &recordingRenderer{}
	if err := th.runTurn(ctx, Message{TS: "1.2", Text: "hello"}, renderer); err != nil {
		t.Fatalf("runTurn = %v, want the answer after non-consecutive stops", err)
	}
	if got := renderer.finalAnswers(); len(got) != 1 || got[0] != answer {
		t.Fatalf("final answers = %q, want the answer after non-consecutive stops", got)
	}
	if completed := platform.completedRequests(); len(completed) != 1 {
		t.Fatalf("completed requests = %+v, want one completed user turn", completed)
	}
	mu.Lock()
	defer mu.Unlock()
	if want := stops + 1; prompts != want {
		t.Fatalf("prompts = %d, want %d attempts", prompts, want)
	}
}

func TestRunTurnStopsAfterTotalProgressMarkerContinuationLimit(t *testing.T) {
	b, platform, st := newTestBot(t)
	th := testThread(t, b, st)
	var mu sync.Mutex
	prompts := 0
	marker := "[[slack-progress: Checking service logs]]"
	client := startTurnGatewayWithLastText(t, th, func(enc *json.Encoder, prompt int, _ string) string {
		mu.Lock()
		prompts++
		mu.Unlock()
		callID := fmt.Sprintf("call-%d", prompt)
		_ = enc.Encode(map[string]any{"type": "agent_start"})
		_ = enc.Encode(map[string]any{
			"type": "tool_execution_start", "toolCallId": callID, "toolName": "bash",
		})
		_ = enc.Encode(map[string]any{
			"type": "tool_execution_end", "toolCallId": callID, "toolName": "bash",
		})
		_ = enc.Encode(map[string]any{
			"type": "message_update",
			"assistantMessageEvent": map[string]any{
				"type": "text_delta", "contentIndex": 0, "delta": marker,
			},
		})
		_ = enc.Encode(map[string]any{
			"type": "message_end",
			"message": map[string]any{
				"role": "assistant", "content": []any{}, "stopReason": "stop",
			},
		})
		_ = enc.Encode(map[string]any{"type": "agent_settled"})
		return marker
	})
	th.client = client
	th.update(func(r *store.ThreadRow) { r.SessionPath = "/test-session.jsonl" })

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	err := th.runTurn(ctx, Message{TS: "1.2", Text: "hello"}, &recordingRenderer{})
	if err == nil || !strings.Contains(err.Error(), "produced no answer") {
		t.Fatalf("runTurn = %v, want the no-answer error after the total continuation limit", err)
	}
	if completed := platform.completedRequests(); len(completed) != 0 {
		t.Errorf("completed requests = %+v, want none when the continuation limit is exhausted", completed)
	}
	mu.Lock()
	defer mu.Unlock()
	if want := maxProgressMarkerContinuations + 1; prompts != want {
		t.Fatalf("prompts = %d, want %d bounded attempts", prompts, want)
	}
}

func TestContinuationRearmResetsToolWorkButSettlementKeepsIt(t *testing.T) {
	st := newTurnState()
	st.feed(toolEvent(t, "tool_execution_start", "call-1", "bash", false), discardLogger())
	if !st.sawToolExecution() {
		t.Fatal("a tool start did not record tool work")
	}
	st.markDone()
	if !st.sawToolExecution() {
		t.Fatal("markDone cleared the tool-work signal needed to budget continuations")
	}
	st.rearmForContinuation()
	if st.sawToolExecution() {
		t.Fatal("rearm kept tool work from the previous response")
	}
}

func TestRunTurnDoesNotContinueAfterProgressMarkerOnProviderFailure(t *testing.T) {
	b, platform, st := newTestBot(t)
	th := testThread(t, b, st)
	var mu sync.Mutex
	prompts := 0
	marker := "[[slack-progress: Checking service logs]]"
	client := startTurnGatewayWithLastText(t, th, func(enc *json.Encoder, _ int, _ string) string {
		mu.Lock()
		prompts++
		mu.Unlock()
		_ = enc.Encode(map[string]any{"type": "agent_start"})
		_ = enc.Encode(map[string]any{
			"type": "message_update",
			"assistantMessageEvent": map[string]any{
				"type": "text_delta", "contentIndex": 0, "delta": marker,
			},
		})
		_ = enc.Encode(map[string]any{
			"type": "message_end",
			"message": map[string]any{
				"role": "assistant", "content": []any{}, "stopReason": "error",
				"errorMessage": "provider exploded",
			},
		})
		_ = enc.Encode(map[string]any{"type": "agent_settled"})
		return marker
	})
	th.client = client
	th.update(func(r *store.ThreadRow) { r.SessionPath = "/test-session.jsonl" })

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	err := th.runTurn(ctx, Message{TS: "1.2", Text: "hello"}, &recordingRenderer{})
	if err == nil || !strings.Contains(PublicMessage(err), "provider exploded") {
		t.Fatalf("runTurn = %v, want the provider failure instead of a continuation", err)
	}
	if completed := platform.completedRequests(); len(completed) != 0 {
		t.Errorf("completed requests = %+v, want none after a failed turn", completed)
	}
	mu.Lock()
	defer mu.Unlock()
	if prompts != 1 {
		t.Fatalf("prompts = %d, want no continuation after a provider failure", prompts)
	}
}

func TestRunTurnPreservesAnInlineProgressMarkerInTheAnswer(t *testing.T) {
	b, _, st := newTestBot(t)
	th := testThread(t, b, st)
	const answer = "The marker is [[slack-progress: not a real status]] in prose."
	client := startTurnGatewayWithLastText(t, th, func(enc *json.Encoder, _ int, _ string) string {
		_ = enc.Encode(map[string]any{"type": "agent_start"})
		_ = enc.Encode(map[string]any{
			"type": "message_update",
			"assistantMessageEvent": map[string]any{
				"type": "text_delta", "contentIndex": 0, "delta": answer,
			},
		})
		_ = enc.Encode(map[string]any{
			"type": "message_end",
			"message": map[string]any{
				"role": "assistant", "content": []any{}, "stopReason": "stop",
			},
		})
		_ = enc.Encode(map[string]any{"type": "agent_settled"})
		return answer
	})
	th.client = client
	th.update(func(r *store.ThreadRow) { r.SessionPath = "/test-session.jsonl" })

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	renderer := &recordingRenderer{}
	if err := th.runTurn(ctx, Message{TS: "1.2", Text: "hello"}, renderer); err != nil {
		t.Fatalf("runTurn = %v, want the inline marker to be ordinary text", err)
	}
	if got := renderer.rendered(); got != answer {
		t.Fatalf("streamed answer = %q, want the inline marker preserved", got)
	}
	if got := renderer.finalAnswers(); len(got) != 1 || got[0] != answer {
		t.Fatalf("final answer = %q, want the inline marker preserved", got)
	}
}

func TestObservedTurnReportsATerminalFailureWithoutText(t *testing.T) {
	b, platform, st := newTestBot(t)
	th := testThread(t, b, st)
	turn := newTurnState()
	turn.outcome.sessionState = "crashed"
	turn.outcome.sessionReason = "exit"
	turn.markDone()
	th.cur.Store(turn)

	th.renderObservedTurn(turn, nil)

	notices := platform.postedNotices()
	if len(notices) != 1 || !strings.Contains(notices[0].Text, "crashed") {
		t.Fatalf("notices = %+v, want the session crash reported", notices)
	}
	if strings.Contains(notices[0].Text, "exit") || strings.Contains(notices[0].Text, "reason") {
		t.Fatalf("notice leaked the session cause: %q", notices[0].Text)
	}
}

func TestObservedTurnStaysSilentForASuccessfulTextlessWake(t *testing.T) {
	b, platform, st := newTestBot(t)
	th := testThread(t, b, st)
	turn := newTurnState()
	turn.markDone()
	th.cur.Store(turn)

	th.renderObservedTurn(turn, nil)

	if notices := platform.postedNotices(); len(notices) != 0 {
		t.Fatalf("notices = %+v, want no notice for a successful textless wake", notices)
	}
}

func TestObservedTurnReportsAFailureWithPartialText(t *testing.T) {
	b, platform, st := newTestBot(t)
	th := testThread(t, b, st)
	turn := newTurnState()
	turn.replace("partial answer")
	turn.outcome.stopReason = "error"
	turn.outcome.errorMessage = "provider exploded"
	turn.markDone()
	th.cur.Store(turn)

	th.renderObservedTurn(turn, nil)

	platform.mu.Lock()
	r := platform.renderer
	platform.mu.Unlock()
	if r == nil {
		t.Fatal("the observed turn did not start a renderer")
	}
	if got := r.failureText(); !strings.Contains(got, "provider exploded") {
		t.Fatalf("renderer failure = %q, want the provider failure with partial text", got)
	}
	if got := r.failurePublic(); !strings.Contains(got, "provider exploded") {
		t.Fatalf("renderer public failure = %q, want the provider detail in chat", got)
	}
}

func TestRunTurnEndsWhenTheSessionCrashes(t *testing.T) {
	b, platform, st := newTestBot(t)
	th := testThread(t, b, st)
	client := startTurnGateway(t, th, func(enc *json.Encoder) {
		// The running turn sets the gateway latch; a crash never settles it.
		_ = enc.Encode(map[string]any{"type": "gw_turn", "state": "running", "turnId": "t_1"})
		_ = enc.Encode(map[string]any{"type": "agent_start"})
		_ = enc.Encode(map[string]any{
			"type": "gw_session_state", "state": "crashed", "reason": "exit", "exitCode": 2,
		})
	})
	th.client = client
	th.update(func(r *store.ThreadRow) { r.SessionPath = "/test-session.jsonl" })

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	err := th.runTurn(ctx, Message{TS: "1.2", Text: "hello"}, &recordingRenderer{})
	if err == nil || !strings.Contains(PublicMessage(err), "crashed") {
		t.Fatalf("runTurn = %v, want the session crash, not a wait for a settlement", err)
	}
	if strings.Contains(PublicMessage(err), "exit code") || !strings.Contains(err.Error(), "exit code 2") {
		t.Fatalf("cause visibility = %q, want exit code in the log-facing error only", err)
	}
	if completed := platform.completedRequests(); len(completed) != 0 {
		t.Errorf("completed requests = %+v, want none after a crash", completed)
	}
}

func TestRunTurnReportsAStopInsteadOfAConnectionLoss(t *testing.T) {
	b, _, st := newTestBot(t)
	th := testThread(t, b, st)
	client := startTurnGateway(t, th, func(enc *json.Encoder) {
		_ = enc.Encode(map[string]any{"type": "gw_turn", "state": "running", "turnId": "t_1"})
		_ = enc.Encode(map[string]any{"type": "agent_start"})
	})
	th.client = client
	th.update(func(r *store.ThreadRow) { r.SessionPath = "/test-session.jsonl" })

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		done <- th.runTurn(ctx, Message{TS: "1.2", Text: "hello"}, &recordingRenderer{})
	}()
	waitFor(t, "the turn to start", func() bool {
		cur := th.cur.Load()
		if cur == nil {
			return false
		}
		cur.mu.Lock()
		defer cur.mu.Unlock()
		return cur.started
	})
	if stopped, err := th.abort(context.Background()); err != nil || !stopped {
		t.Fatalf("abort = (%v, %v), want the live turn stopped", stopped, err)
	}
	select {
	case err := <-done:
		if err == nil || !strings.Contains(PublicMessage(err), "stopped") {
			t.Fatalf("runTurn = %v, want the stop message", err)
		}
		if strings.Contains(PublicMessage(err), "gateway connection") {
			t.Fatalf("the stop was reported as a connection loss: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("runTurn did not return after the stop")
	}
}

// startTurnGateway serves one in-process gateway connection. onPrompt emits the
// events the fake pi would emit; every command gets an empty successful response.
func startTurnGateway(t *testing.T, th *thread, onPrompt func(*json.Encoder)) *gwclient.Client {
	t.Helper()
	return startTurnGatewayWithLastText(t, th, func(enc *json.Encoder, _ int, _ string) string {
		onPrompt(enc)
		return ""
	})
}

// startTurnGatewayWithLastText is startTurnGateway with a scripted
// get_last_assistant_text response. onPrompt handles the n-th prompt (1-based)
// and returns the text that get_last_assistant_text reports afterwards.
func startTurnGatewayWithLastText(t *testing.T, th *thread, onPrompt func(*json.Encoder, int, string) string) *gwclient.Client {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	done := make(chan struct{})
	go func() {
		defer close(done)
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		enc, dec := json.NewEncoder(conn), json.NewDecoder(bufio.NewReader(conn))
		var hello json.RawMessage
		if dec.Decode(&hello) != nil {
			return
		}
		_ = enc.Encode(protocol.Welcome{Type: "gw_welcome", Protocol: protocol.Version,
			ClientID: "turn-test", Concurrency: "queue",
			Session: &protocol.SessionRef{Path: "/test-session.jsonl"}})
		prompts := 0
		lastAssistant := ""
		for {
			var raw json.RawMessage
			if dec.Decode(&raw) != nil {
				return
			}
			var command struct{ Type, ID, Message string }
			_ = json.Unmarshal(raw, &command)
			if command.Type == "prompt" {
				prompts++
				lastAssistant = onPrompt(enc, prompts, command.Message)
			}
			data := map[string]any{}
			if command.Type == "get_last_assistant_text" {
				data["text"] = lastAssistant
			}
			_ = enc.Encode(map[string]any{
				"type": "response", "id": command.ID, "command": command.Type,
				"success": true, "data": data,
			})
		}
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)
	client, err := gwclient.Dial(ctx, gwclient.Config{
		Addr: ln.Addr().String(), Token: "test", OnEvent: th.onEvent,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = client.Close(); <-done })
	return client
}

func TestInlineProgressMarkerStaysInReplyAndDoesNotTitleTool(t *testing.T) {
	st := newTurnState()
	log := discardLogger()
	startedAt := time.Unix(2_000, 0)
	st.progressStartedAt = startedAt

	marker := "[[slack-progress: Checking current service\nlogs]]"
	st.feed(textDelta(t, "Checking the service. [[slack-prog"), log)
	st.feed(textDelta(t, "ress: Checking current service\nlogs]]"), log)
	if got := st.takePending(); got != "Checking the service. "+marker {
		t.Fatalf("reply text = %q, want the inline marker preserved", got)
	}

	st.feed(toolEvent(t, "tool_execution_start", "call-1", "bash", false), log)
	updates := st.takeTaskUpdates()
	if len(updates) != 1 || updates[0].Title != "Running a command" || updates[0].Details != "" {
		t.Fatalf("start updates = %+v, want the tool fallback title", updates)
	}
	if got := st.text(); !strings.Contains(got, marker) {
		t.Fatalf("inline marker disappeared from reply text %q", got)
	}
	final := "Finished. [[slack-progress: hidden status]]"
	if got := st.authoritativeText(final); got != final {
		t.Fatalf("authoritative answer = %q, want the inline marker preserved", got)
	}

	st.feed(toolEvent(t, "tool_execution_end", "call-1", "bash", false), log)
	if updates := st.takeTaskUpdates(); len(updates) != 0 {
		t.Fatalf("tool completion should not close the turn progress task: %+v", updates)
	}
	st.finishProgressTaskAt(TaskComplete, startedAt.Add(75*time.Second))
	updates = st.takeTaskUpdates()
	if len(updates) != 1 || updates[0].ID != turnProgressTaskID || updates[0].Title != "Completed · 1 action · 1m15s" || updates[0].Details != "Completed actions:\n• Running a command" || updates[0].Status != TaskComplete {
		t.Fatalf("turn completion update = %+v", updates)
	}
}

func TestProgressClosingDelimiterCanSplitAcrossDeltas(t *testing.T) {
	st := newTurnState()
	log := discardLogger()

	st.feed(textDelta(t, "[[slack-progress: Checking service logs]"), log)
	st.feed(textDelta(t, "]"), log)
	st.feed(toolEvent(t, "tool_execution_start", "call-1", "bash", false), log)

	updates := st.takeTaskUpdates()
	if len(updates) != 1 || updates[0].Title != "Checking service logs" {
		t.Fatalf("task updates = %+v, want parsed title across split delimiter", updates)
	}
	if got := st.text(); strings.Contains(got, progressMarkerOpen) {
		t.Fatalf("status marker leaked into reply: %q", got)
	}
}

func TestLeadingProgressMarkerWithProseIsConsumed(t *testing.T) {
	st := newTurnState()
	log := discardLogger()

	st.feed(textDelta(t, "[[slack-progress: Checking service logs]] Looking at the logs"), log)
	if got := st.text(); got != "Looking at the logs" {
		t.Fatalf("reply text = %q, want only the prose", got)
	}
	if len(st.pendingProgress) != 1 || st.pendingProgress[0] != "Checking service logs" {
		t.Fatalf("queued titles = %v, want the leading marker", st.pendingProgress)
	}
	st.feed(toolEvent(t, "tool_execution_start", "call-1", "bash", false), log)
	updates := st.takeTaskUpdates()
	if len(updates) != 1 || updates[0].Title != "Checking service logs" {
		t.Fatalf("task update = %+v, want the leading marker title", updates)
	}
}

func TestSplitLeadingProgressMarkerWithProseIsConsumed(t *testing.T) {
	st := newTurnState()
	log := discardLogger()

	st.feed(textDelta(t, "[[slack-progress: Checking service"), log)
	st.feed(textDelta(t, " logs]] Looking at the logs"), log)
	if got := st.text(); got != "Looking at the logs" {
		t.Fatalf("reply text = %q, want the prose after a split marker", got)
	}
	if len(st.pendingProgress) != 1 || st.pendingProgress[0] != "Checking service logs" {
		t.Fatalf("queued titles = %v, want the completed split marker", st.pendingProgress)
	}
}

func TestLeadingProgressMarkersBeforeProseAreConsumedInOrder(t *testing.T) {
	st := newTurnState()
	log := discardLogger()

	st.feed(textDelta(t, "[[slack-progress: First action]][[slack-progress: Second action]] Doing work"), log)
	if got := st.text(); got != "Doing work" {
		t.Fatalf("reply text = %q, want only the prose", got)
	}
	if len(st.pendingProgress) != 2 || st.pendingProgress[0] != "First action" || st.pendingProgress[1] != "Second action" {
		t.Fatalf("queued titles = %v, want both leading markers in order", st.pendingProgress)
	}
}

func TestLeadingProgressMarkerIsStrippedFromTheAuthoritativeAnswerAndRecovery(t *testing.T) {
	st := newTurnState()
	marker := "[[slack-progress: Checking service logs]]"
	if got := st.authoritativeText(marker + " Looking at the logs"); got != "Looking at the logs" {
		t.Fatalf("authoritative answer = %q, want only the prose", got)
	}
	if got := st.authoritativeText(marker); got != "" {
		t.Fatalf("marker-only authoritative answer = %q, want empty", got)
	}
	st.replace(marker + " Looking at the logs")
	if got := st.text(); got != "Looking at the logs" {
		t.Fatalf("recovered text = %q, want only the prose", got)
	}
	inline := "Looking at the logs. [[slack-progress: quoted]]"
	if got := st.authoritativeText(inline); got != inline {
		t.Fatalf("inline marker authoritative answer = %q, want it preserved", got)
	}
}

func TestIncompleteProgressMarkerDoesNotSuppressFollowingReply(t *testing.T) {
	st := newTurnState()
	log := discardLogger()
	st.feed(textDelta(t, "Status [[slack-progress: incomplete"), log)

	// A new assistant message is a boundary: an unclosed marker must become
	// ordinary reply text rather than capturing the next message indefinitely.
	st.startMessage()
	st.feed(textDelta(t, "Final answer"), log)
	if got := st.text(); !strings.Contains(got, "incomplete") || !strings.Contains(got, "Final answer") {
		t.Fatalf("reply lost text around an incomplete marker: %q", got)
	}
	if got := st.authoritativeText("Answer [[slack-progress: dangling"); !strings.Contains(got, "dangling") {
		t.Fatalf("authoritative answer was truncated at incomplete marker: %q", got)
	}
}

func TestReplaceClearsPartialProgressMarkerState(t *testing.T) {
	st := newTurnState()
	st.feed(textDelta(t, "Partial [[slack-prog"), discardLogger())
	st.replace("Recovered answer")
	st.markDone()

	if got := st.text(); got != "Recovered answer" {
		t.Fatalf("recovered text = %q, want no stale marker suffix", got)
	}
	if got := st.takePending(); got != "Recovered answer" {
		t.Fatalf("recovered pending text = %q", got)
	}
}

func TestParallelProgressMarkersStayInToolStartOrder(t *testing.T) {
	st := newTurnState()
	log := discardLogger()
	startedAt := time.Unix(3_000, 0)
	st.progressStartedAt = startedAt
	titles := []string{"Check first task", "Check second task", "Check third task", "Check fourth task"}
	for _, title := range titles {
		st.feed(textDelta(t, "[[slack-progress: "+title+"]]"), log)
	}
	for i, title := range titles {
		id := "call-" + string(rune('1'+i))
		st.feed(toolEvent(t, "tool_execution_start", id, "bash", false), log)
		updates := st.takeTaskUpdates()
		if i == 0 {
			if len(updates) != 1 || updates[0].ID != turnProgressTaskID || updates[0].Title != title || updates[0].Status != TaskInProgress {
				t.Fatalf("first tool progress = %+v, want the compact rolling task", updates)
			}
		} else if step, ok := lastProgressTask(updates, "tool-step-"+string(rune('1'+i))); !ok || step.Title != title || step.Status != TaskInProgress {
			t.Fatalf("tool %d progress = %+v, want ordered in-progress step %q", i+1, updates, title)
		}
		st.feed(toolEvent(t, "tool_execution_end", id, "bash", false), log)
	}
	if len(st.activeTasks) != len(titles)+1 || len(st.progressTaskOrder) != len(titles) {
		t.Fatalf("active tasks=%d report rows=%d, want summary plus %d ordered steps", len(st.activeTasks), len(st.progressTaskOrder), len(titles))
	}

	st.finishProgressTaskAt(TaskComplete, startedAt.Add(2*time.Minute))
	updates := st.takeTaskUpdates()
	wantSummary := "Completed · 4 actions · 2m0s"
	wantDetails := "Completed actions:\n• Check first task\n• Check second task\n• Check third task\n• Check fourth task"
	summary, ok := lastProgressTask(updates, turnProgressTaskID)
	if !ok || summary.Title != wantSummary || summary.Details != wantDetails || summary.Status != TaskComplete {
		t.Fatalf("turn completion = %+v", updates)
	}
	for i, title := range titles {
		step, ok := lastProgressTask(updates, "tool-step-"+string(rune('1'+i)))
		if !ok || step.Title != title || step.Status != TaskComplete {
			t.Fatalf("completed step %d = %+v, want %q complete", i+1, step, title)
		}
	}
}

func TestProgressTitlesAreTrimmedAndRejectCommandLikeText(t *testing.T) {
	recovered := newTurnState()
	inline := "Answer [[slack-progress: hidden status]] done"
	recovered.replace(inline)
	if got := recovered.takePending(); got != inline {
		t.Fatalf("recovered text = %q, want the inline marker preserved", got)
	}

	long := strings.Repeat("Check ", maxProgressTitleRunes)
	if got := trimProgressTitle(long); len([]rune(got)) != maxProgressTitleRunes {
		t.Fatalf("trimmed title rune length = %d, want %d", len([]rune(got)), maxProgressTitleRunes)
	}
	if got := trimProgressTitle("Checking logs --unit private.service"); got != "" {
		t.Fatalf("command-like progress title = %q, want rejected", got)
	}
	for _, risky := range []string{
		"Checking AKIAIOSFODNN7EXAMPLE",
		"Using xoxb-123456789012-123456789012-123456789012",
		"Checking 4111111111111111",
		"Checking 123456789012",
		"Reading password=example",
		"Submit PIN 123456",
		"Enter OTP 654321",
		"Use passcode 1482",
		"Enter one-time code 172839",
		"Enter verification code 314159",
		"Enter code 123456",
		"Enter code\n123456",
	} {
		if got := trimProgressTitle(risky); got != "" {
			t.Errorf("credential-like progress title %q was accepted as %q", risky, got)
		}
	}
	for _, safe := range []string{"Spinning up a service", "Pinpointing the issue", "Reviewing the 2026 report", "Review the code changes"} {
		if got := trimProgressTitle(safe); got != safe {
			t.Errorf("ordinary title %q was rejected as %q", safe, got)
		}
	}
	if got := safeToolTitle("read token abc123456789"); got != "Using a tool" {
		t.Fatalf("unsafe tool-name fallback = %q, want generic title", got)
	}

	for _, marker := range []string{"Submit PIN 123456", "Enter code 123456"} {
		st := newTurnState()
		st.feed(textDelta(t, "[[slack-progress: "+marker+"]]"), discardLogger())
		st.feed(toolEvent(t, "tool_execution_start", "call-1", "bash", false), discardLogger())
		if got := st.takeTaskUpdates(); len(got) != 1 || got[0].Title != "Running a command" || got[0].Details != "" {
			t.Fatalf("short-code marker %q was not safely replaced in title or leaked details: %+v", marker, got)
		}
	}
}

func TestFallbackProgressTitleIsBounded(t *testing.T) {
	title := taskTitle(strings.Repeat("n", maxToolNameBytes))
	if got := len([]rune(title)); got > maxProgressTitleRunes {
		t.Fatalf("fallback title rune length = %d, want at most %d", got, maxProgressTitleRunes)
	}
}

func TestMarkDoneFinalizesActiveProgressTask(t *testing.T) {
	st := newTurnState()
	st.feed(toolEvent(t, "tool_execution_start", "call-1", "bash", false), discardLogger())
	st.markDone()
	updates := st.takeTaskUpdates()
	if len(updates) != 2 || updates[0].Status != TaskInProgress || updates[1].Status != TaskComplete {
		t.Fatalf("markDone updates = %+v, want in-progress then completion", updates)
	}
}

func TestProgressTerminalUpdateKeepsInitialStatusAndRejectsLateStarts(t *testing.T) {
	st := newTurnState()
	log := discardLogger()
	st.feed(toolEvent(t, "tool_execution_start", "call-1", "bash", false), log)
	st.finishOpenTasks()
	st.feed(toolEvent(t, "tool_execution_start", "call-2", "read", false), log)

	updates := st.takeTaskUpdates()
	if len(updates) != 2 || updates[0].Status != TaskInProgress || updates[1].Status != TaskError || updates[0].ID != turnProgressTaskID || updates[1].ID != turnProgressTaskID {
		t.Fatalf("terminal updates = %+v, want in-progress then error on the same task", updates)
	}
	if len(st.activeTasks) != 0 || len(st.takeTaskUpdates()) != 0 {
		t.Fatal("a late start reopened a terminal progress task")
	}
}

func TestAbortResponseWinsSettlementRace(t *testing.T) {
	st := newTurnState()
	log := discardLogger()
	st.feed(toolEvent(t, "tool_execution_start", "call-1", "bash", false), log)
	if got := st.takeTaskUpdates(); len(got) != 1 || got[0].Status != TaskInProgress {
		t.Fatalf("initial progress = %+v", got)
	}

	tracked := st.beginAbort()
	st.feed(gwclient.Event{Type: "agent_settled"}, log)
	if !st.done() {
		t.Fatal("settlement did not mark the turn boundary")
	}
	select {
	case <-st.doneCh:
		t.Fatal("renderer wait closed before the abort result arrived")
	default:
	}
	if len(st.takeTaskUpdates()) != 0 {
		t.Fatal("settlement published a terminal task before the abort result arrived")
	}

	st.resolveAbort(tracked, true)
	if !st.done() {
		t.Fatal("settled turn did not finish after abort resolution")
	}
	if got := st.takeTaskUpdates(); len(got) != 1 || got[0].Status != TaskError {
		t.Fatalf("aborted turn update = %+v, want error after confirmed abort", got)
	}
}

func TestAgentStartDuringAbortWaitCreatesNextTurn(t *testing.T) {
	log := discardLogger()
	lifetime, cancel := context.WithCancel(context.Background())
	cancel()
	th := &thread{b: &Bot{lifetime: lifetime, log: log}, log: log}
	previous := newTurnState()
	th.cur.Store(previous)
	previous.feed(gwclient.Event{Type: "agent_start", Seq: 10}, log)
	tracked := previous.beginAbort()
	previous.feed(gwclient.Event{Type: "agent_settled", Seq: 11}, log)

	// Hold the renderer lock so its goroutine cannot finish and release the new
	// turn reservation before the callback's routing decision is inspected.
	th.turnMu.Lock()
	th.onSessionEventWithClient("", gwclient.Event{Type: "agent_start", Seq: 12}, nil, th.sessionEpoch.Load())
	next := th.cur.Load()
	if next == nil || next == previous {
		th.turnMu.Unlock()
		t.Fatal("agent_start during abort wait was not assigned a new turn")
	}
	th.turnMu.Unlock()
	select {
	case <-next.finishedCh:
	case <-time.After(time.Second):
		t.Fatal("test renderer did not release the next turn")
	}
	previous.resolveAbort(tracked, true)
}

func TestReplayedAgentStartDoesNotReplaceDoneTurn(t *testing.T) {
	st := newTurnState()
	st.feed(gwclient.Event{Type: "agent_start", Seq: 10}, discardLogger())
	st.markDone()
	th := &thread{log: discardLogger()}
	th.cur.Store(st)

	th.onSessionEventWithClient("", gwclient.Event{Type: "agent_start", Seq: 10}, nil, th.sessionEpoch.Load())
	if th.cur.Load() != st {
		t.Fatal("replayed agent_start replaced the completed turn before sequence deduplication")
	}
}

func TestObservedCleanupWaitsForAbortBeforeFinalizingProgress(t *testing.T) {
	st := newTurnState()
	st.feed(toolEvent(t, "tool_execution_start", "call-1", "bash", false), discardLogger())
	st.takeTaskUpdates()
	tracked := st.beginAbort()
	st.markDone()

	cleanupStarted := make(chan struct{})
	cleanupDone := make(chan struct{})
	go func() {
		close(cleanupStarted)
		_ = st.waitAbortResolution(context.Background())
		st.finishOpenTasks()
		close(cleanupDone)
	}()
	<-cleanupStarted
	select {
	case <-cleanupDone:
		t.Fatal("observed cleanup passed an unresolved abort")
	default:
	}

	st.resolveAbort(tracked, true)
	select {
	case <-cleanupDone:
	case <-time.After(time.Second):
		t.Fatal("observed cleanup did not resume after abort resolution")
	}
	if got := st.takeTaskUpdates(); len(got) != 1 || got[0].Status != TaskError {
		t.Fatalf("final progress update = %+v, want an error before cleanup ends", got)
	}
}

func TestFailedAbortLeavesSettledProgressComplete(t *testing.T) {
	st := newTurnState()
	log := discardLogger()
	st.feed(toolEvent(t, "tool_execution_start", "call-1", "bash", false), log)
	st.takeTaskUpdates()

	tracked := st.beginAbort()
	st.feed(gwclient.Event{Type: "agent_settled"}, log)
	st.resolveAbort(tracked, false)
	if !st.done() {
		t.Fatal("settled turn did not finish after failed abort")
	}
	if got := st.takeTaskUpdates(); len(got) != 1 || got[0].Status != TaskComplete {
		t.Fatalf("non-aborted turn update = %+v, want complete", got)
	}
}

func TestReplayedEventsDoNotDuplicateProgressMarkers(t *testing.T) {
	st := newTurnState()
	log := discardLogger()
	marker := textDelta(t, "[[slack-progress: First action]]")
	marker.Seq = 10
	start := toolEvent(t, "tool_execution_start", "call-1", "bash", false)
	start.Seq = 11
	st.feed(marker, log)
	st.feed(start, log)
	if got := st.takeTaskUpdates(); len(got) != 1 || got[0].Title != "First action" {
		t.Fatalf("first action = %+v", got)
	}

	marker.Seq = 10
	start.Seq = 11
	st.feed(marker, log)
	st.feed(start, log)
	if got := st.takeTaskUpdates(); len(got) != 0 || len(st.pendingProgress) != 0 {
		t.Fatalf("replayed events produced updates or stale titles: updates=%+v queued=%v", got, st.pendingProgress)
	}

	nextMarker := textDelta(t, "[[slack-progress: Second action]]")
	nextMarker.Seq = 12
	nextStart := toolEvent(t, "tool_execution_start", "call-2", "bash", false)
	nextStart.Seq = 13
	st.feed(nextMarker, log)
	st.feed(nextStart, log)
	got := st.takeTaskUpdates()
	if summary, ok := lastProgressTask(got, turnProgressTaskID); !ok || summary.Title != "Second action" {
		t.Fatalf("second action summary = %+v, want no stale replayed title", got)
	}
	if first, ok := lastProgressTask(got, "tool-step-1"); !ok || first.Title != "First action" {
		t.Fatalf("first plan step = %+v, want the original observed title", first)
	}
	if second, ok := lastProgressTask(got, "tool-step-2"); !ok || second.Title != "Second action" {
		t.Fatalf("second plan step = %+v, want the next marker", second)
	}
}

func TestGatewaySnapshotResetsSequenceWatermark(t *testing.T) {
	st := newTurnState()
	log := discardLogger()
	marker := textDelta(t, "[[slack-progress: Before restart]]")
	marker.Seq = 100
	start := toolEvent(t, "tool_execution_start", "old-call", "bash", false)
	start.Seq = 101
	st.feed(marker, log)
	st.feed(start, log)
	st.takeTaskUpdates()

	st.feed(gwclient.Event{Type: "gw_snapshot", Raw: []byte(`{"type":"gw_snapshot","headSeq":5}`)}, log)
	nextMarker := textDelta(t, "[[slack-progress: After restart]]")
	nextMarker.Seq = 6
	nextStart := toolEvent(t, "tool_execution_start", "new-call", "bash", false)
	nextStart.Seq = 7
	st.feed(nextMarker, log)
	st.feed(nextStart, log)
	if got := st.takeTaskUpdates(); len(got) != 1 || got[0].Title != "After restart" {
		t.Fatalf("post-snapshot task update = %+v", got)
	}
	settled := gwclient.Event{Type: "agent_settled", Seq: 8}
	st.feed(settled, log)
	if !st.done() {
		t.Fatal("turn did not settle after sequence reset")
	}
}

func TestOversizedToolIdentifiersAreNotRetained(t *testing.T) {
	st := newTurnState()
	log := discardLogger()
	longID := strings.Repeat("x", maxToolCallIDBytes+1)
	st.feed(toolEvent(t, "tool_execution_start", longID, "bash", false), log)
	if len(st.activeTasks) != 0 || st.seenToolCalls.Len() != 0 || len(st.takeTaskUpdates()) != 0 {
		t.Fatal("oversized tool call ID was retained as progress state")
	}
	longName := strings.Repeat("x", maxToolNameBytes+1)
	st.feed(toolEvent(t, "tool_execution_start", "call-1", longName, false), log)
	if len(st.activeTasks) != 0 || st.seenToolCalls.Len() != 0 {
		t.Fatal("oversized tool name was retained as progress state")
	}
}

func TestOversizedProgressMarkerIsOnlyRemovedWhenItIsTheWholeMessage(t *testing.T) {
	log := discardLogger()
	content := strings.Repeat("x", maxProgressTitleRunes+1)
	marker := progressMarkerOpen + content + " tail" + progressMarkerClose
	inline := "Before " + marker + " after"

	streamed := newTurnState()
	streamed.feed(textDelta(t, "Before "+progressMarkerOpen+content), log)
	streamed.feed(textDelta(t, " tail"+progressMarkerClose+" after"), log)
	if got := streamed.text(); got != inline {
		t.Fatalf("streamed inline oversized marker = %q, want it preserved literally", got)
	}
	if got := streamed.authoritativeText(inline); got != inline {
		t.Fatalf("authoritative inline oversized marker = %q, want it preserved literally", got)
	}

	recovered := newTurnState()
	recovered.replace(inline)
	if got := recovered.text(); got != inline {
		t.Fatalf("recovered inline oversized marker = %q, want it preserved literally", got)
	}

	markerOnly := newTurnState()
	markerOnly.feed(textDelta(t, marker), log)
	markerOnly.finishMessageText()
	if got := markerOnly.text(); got != "" {
		t.Fatalf("marker-only oversized message = %q, want it removed", got)
	}
	if len(markerOnly.pendingProgress) != 0 {
		t.Fatalf("marker-only oversized message queued titles: %v", markerOnly.pendingProgress)
	}
}

func TestProgressTaskUsesBoundedPlanRowsForTheTurn(t *testing.T) {
	st := newTurnState()
	log := discardLogger()
	startedAt := time.Unix(4_000, 0)
	st.progressStartedAt = startedAt
	for i := range 9 {
		title := "Check step " + strings.Repeat("x", i+1)
		st.feed(textDelta(t, "[[slack-progress: "+title+"]]"), log)
	}
	for i := range 9 {
		id := "call-" + strings.Repeat("x", i+1)
		st.feed(toolEvent(t, "tool_execution_start", id, "bash", false), log)
		st.feed(toolEvent(t, "tool_execution_end", id, "bash", false), log)
	}

	updates := st.takeTaskUpdates()
	summaryStart, ok := lastProgressTask(updates, turnProgressTaskID)
	if !ok || summaryStart.Title != "Check step "+strings.Repeat("x", 9) || summaryStart.Status != TaskInProgress {
		t.Fatalf("coalesced summary update = %+v", summaryStart)
	}
	if len(updates) > maxPendingTaskUpdates || len(st.activeTasks) != 10 || st.seenToolCalls.Len() != 9 {
		t.Fatalf("updates=%d active tasks=%d seen calls=%d, want bounded rows for nine calls", len(updates), len(st.activeTasks), st.seenToolCalls.Len())
	}
	if len(st.progressActionTitles) != maxProgressDetailTitles || len(st.progressTaskOrder) != 9 {
		t.Fatalf("retained titles=%d report rows=%d, want %d titles and nine steps", len(st.progressActionTitles), len(st.progressTaskOrder), maxProgressDetailTitles)
	}

	st.finishProgressTaskAt(TaskComplete, startedAt.Add(45*time.Second))
	updates = st.takeTaskUpdates()
	wantDetails := "Completed actions:\n• Check step x\n• Check step xx\n• Check step xxx\n• Check step xxxx\n• Check step xxxxx\n• Check step xxxxxx\n• 3 additional action(s) not listed"
	summary, ok := lastProgressTask(updates, turnProgressTaskID)
	if !ok || summary.Title != "Completed · 9 actions · 45s" || summary.Details != wantDetails || summary.Status != TaskComplete {
		t.Fatalf("completed summary = %+v", summary)
	}
	for i := range 9 {
		step, ok := lastProgressTask(updates, "tool-step-"+string(rune('1'+i)))
		if !ok || step.Status != TaskComplete {
			t.Fatalf("completed step %d = %+v, want complete", i+1, step)
		}
	}

	st.feed(gwclient.Event{Type: "agent_settled"}, log)
	st.feed(toolEvent(t, "tool_execution_start", "late-call", "bash", false), log)
	if len(st.activeTasks) != 0 || len(st.takeTaskUpdates()) != 0 {
		t.Fatal("a late tool event reopened progress after the turn settled")
	}
}

func TestProgressSummaryFormatsActionCountAndDuration(t *testing.T) {
	tests := []struct {
		name        string
		status      TaskProgressStatus
		actions     int
		elapsed     time.Duration
		wantSummary string
	}{
		{name: "singular under one second", status: TaskComplete, actions: 1, elapsed: 500 * time.Millisecond, wantSummary: "Completed · 1 action · <1s"},
		{name: "plural rounded duration", status: TaskComplete, actions: 2, elapsed: 65 * time.Second, wantSummary: "Completed · 2 actions · 1m5s"},
		{name: "incomplete turn", status: TaskError, actions: 0, elapsed: time.Second, wantSummary: "Incomplete · 0 actions · 1s"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := summarizeProgressMetrics(tt.status, tt.actions, tt.elapsed); got != tt.wantSummary {
				t.Fatalf("summary = %q, want %q", got, tt.wantSummary)
			}
		})
	}
}

func TestProgressStepTitleLeavesRoomForTheCount(t *testing.T) {
	got := progressStepTitle(strings.Repeat("x", maxProgressTitleRunes), 12)
	if runes := []rune(got); len(runes) > maxProgressTitleRunes {
		t.Fatalf("grouped title has %d runes, want at most %d: %q", len(runes), maxProgressTitleRunes, got)
	}
	if !strings.HasSuffix(got, " (×12)") {
		t.Fatalf("grouped title = %q, want the count suffix", got)
	}
}

func TestCompletedActionCountUsesUniqueToolEnds(t *testing.T) {
	st := newTurnState()
	log := discardLogger()
	startedAt := time.Unix(5_000, 0)
	st.progressStartedAt = startedAt

	st.feed(toolEvent(t, "tool_execution_start", "completed-1", "bash", false), log)
	st.takeTaskUpdates()
	end := toolEvent(t, "tool_execution_end", "completed-1", "bash", false)
	st.feed(end, log)
	st.feed(end, log)

	st.feed(textDelta(t, "[[slack-progress: Second action]]"), log)
	st.feed(toolEvent(t, "tool_execution_start", "completed-2", "bash", false), log)
	st.takeTaskUpdates()
	st.feed(toolEvent(t, "tool_execution_end", "completed-2", "bash", true), log)

	st.feed(textDelta(t, "[[slack-progress: Third action]]"), log)
	st.feed(toolEvent(t, "tool_execution_start", "still-running", "bash", false), log)
	st.takeTaskUpdates()
	st.finishProgressTaskAt(TaskComplete, startedAt.Add(125*time.Second))

	updates := st.takeTaskUpdates()
	summary, ok := lastProgressTask(updates, turnProgressTaskID)
	if !ok || summary.Title != "Completed · 2 actions · 2m5s" {
		t.Fatalf("completion summary = %+v, want two returned calls and elapsed time", updates)
	}
	if step, ok := lastProgressTask(updates, "tool-step-2"); !ok || step.Status != TaskError {
		t.Fatalf("failed tool step = %+v, want error status", step)
	}
}

func TestProgressDurationStartsAtFirstTurnStart(t *testing.T) {
	st := newTurnState()
	log := discardLogger()
	st.feed(gwclient.Event{Type: "agent_start"}, log)
	startedAt := st.progressStartedAt
	if startedAt.IsZero() {
		t.Fatal("agent_start did not establish the progress start time")
	}

	st.feed(gwclient.Event{Type: "turn_start"}, log)
	if !st.progressStartedAt.Equal(startedAt) {
		t.Fatal("a later turn_start reset the original progress start time")
	}
}

func TestFirstToolStartProvidesDurationFallback(t *testing.T) {
	st := newTurnState()
	log := discardLogger()
	st.feed(toolEvent(t, "tool_execution_start", "first", "read", false), log)
	startedAt := st.progressStartedAt
	if startedAt.IsZero() {
		t.Fatal("first valid tool start did not establish the fallback start time")
	}
	st.takeTaskUpdates()

	st.feed(toolEvent(t, "tool_execution_end", "first", "read", false), log)
	st.finishProgressTaskAt(TaskComplete, startedAt.Add(3*time.Second))
	updates := st.takeTaskUpdates()
	if len(updates) != 1 || updates[0].Title != "Completed · 1 action · 3s" {
		t.Fatalf("completion summary = %+v, want duration from first tool start", updates)
	}
}

func TestDuplicateToolStartDoesNotConsumeNextProgressTitle(t *testing.T) {
	st := newTurnState()
	log := discardLogger()
	st.feed(textDelta(t, "[[slack-progress: First action]]"), log)
	st.feed(toolEvent(t, "tool_execution_start", "call-1", "bash", false), log)
	if got := st.takeTaskUpdates(); len(got) != 1 || got[0].Title != "First action" {
		t.Fatalf("first action = %+v", got)
	}

	st.feed(textDelta(t, "[[slack-progress: Second action]]"), log)
	st.feed(toolEvent(t, "tool_execution_start", "call-1", "bash", false), log)
	if got := st.takeTaskUpdates(); len(got) != 0 {
		t.Fatalf("duplicate start emitted an update: %+v", got)
	}
	if len(st.pendingProgress) != 1 {
		t.Fatalf("queued titles after duplicate start = %d, want one", len(st.pendingProgress))
	}

	st.feed(toolEvent(t, "tool_execution_start", "call-2", "bash", false), log)
	got := st.takeTaskUpdates()
	if summary, ok := lastProgressTask(got, turnProgressTaskID); !ok || summary.Title != "Second action" {
		t.Fatalf("second action summary = %+v, want marker preserved for next call", got)
	}
	if first, ok := lastProgressTask(got, "tool-step-1"); !ok || first.Title != "First action" {
		t.Fatalf("first plan step = %+v, want original marker", first)
	}
	if second, ok := lastProgressTask(got, "tool-step-2"); !ok || second.Title != "Second action" {
		t.Fatalf("second plan step = %+v, want next marker", second)
	}
}

func TestProgressMarkerBoundariesAndTitleQueueAreBounded(t *testing.T) {
	log := discardLogger()
	st := newTurnState()
	overlong := strings.Repeat("x", maxProgressTitleRunes+1)
	st.feed(textDelta(t, progressMarkerOpen+overlong), log)
	st.startMessage()
	if got := st.text(); got != "" {
		t.Fatalf("an unfinished oversized marker leaked into the reply: %q", got)
	}
	if len(st.pendingProgress) != 0 {
		t.Fatalf("unfinished oversized marker queued titles: %v", st.pendingProgress)
	}
	st.feed(textDelta(t, "Final answer"), log)
	if got := st.text(); got != "Final answer" {
		t.Fatalf("text after an oversized marker boundary = %q, want final answer", got)
	}

	st = newTurnState()
	for i := range maxPendingProgressTitles + 1 {
		title := fmt.Sprintf("Check service %d", i+1)
		st.feed(textDelta(t, "[[slack-progress: "+title+"]]"), log)
	}
	st.finishMessageText()
	if len(st.pendingProgress) != maxPendingProgressTitles {
		t.Fatalf("queued titles = %d, want cap %d", len(st.pendingProgress), maxPendingProgressTitles)
	}
	for i := range maxPendingProgressTitles {
		id := "queued-" + strings.Repeat("x", i+1)
		st.feed(toolEvent(t, "tool_execution_start", id, "bash", false), log)
		got := st.takeTaskUpdates()
		wantTitle := fmt.Sprintf("Check service %d", i+1)
		if i >= maxProgressReportTasks {
			wantTitle = fmt.Sprintf("Additional actions (%d)", i+1-maxProgressReportTasks)
		}
		if summary, ok := lastProgressTask(got, turnProgressTaskID); !ok || summary.Title != wantTitle {
			t.Fatalf("queued progress %d = %+v, want title %q", i, got, wantTitle)
		}
	}
	st.feed(toolEvent(t, "tool_execution_start", "overflow", "bash", false), log)
	got := st.takeTaskUpdates()
	if summary, ok := lastProgressTask(got, turnProgressTaskID); !ok || summary.Title != "Additional actions (25)" {
		t.Fatalf("overflow should aggregate the hidden calls: %+v", got)
	}
	if len(st.progressTaskOrder) != maxProgressReportTasks || st.activeTasks[progressOverflowTaskID].Title == "" {
		t.Fatalf("visible steps=%d overflow=%+v, want %d steps and one aggregate", len(st.progressTaskOrder), st.activeTasks[progressOverflowTaskID], maxProgressReportTasks)
	}
	if st.seenToolCalls.Len() != maxSeenToolCallIDs {
		t.Fatalf("seen tool-call IDs = %d, want bounded cap %d", st.seenToolCalls.Len(), maxSeenToolCallIDs)
	}
}

func TestRepeatedToolTitlesStayOneGroupedRow(t *testing.T) {
	st := newTurnState()
	log := discardLogger()
	startedAt := time.Unix(6_000, 0)
	st.progressStartedAt = startedAt

	for i := 1; i <= 50; i++ {
		id := fmt.Sprintf("call-%d", i)
		st.feed(toolEvent(t, "tool_execution_start", id, "bash", false), log)
		st.feed(toolEvent(t, "tool_execution_end", id, "bash", false), log)
	}

	updates := st.takeTaskUpdates()
	summary, ok := lastProgressTask(updates, turnProgressTaskID)
	if !ok || summary.Title != "Running a command (×50)" {
		t.Fatalf("grouped summary = %+v, want the running count", updates)
	}
	if st.progressPlanStarted {
		t.Fatal("one repeated title promoted a plan")
	}
	if len(st.progressTaskOrder) != 1 {
		t.Fatalf("report rows = %d, want one grouped row", len(st.progressTaskOrder))
	}
	if step := st.activeTasks["tool-step-1"]; step.Title != "Running a command (×50)" || step.Status != TaskComplete {
		t.Fatalf("grouped step = %+v, want complete with the count", step)
	}

	st.finishProgressTaskAt(TaskComplete, startedAt.Add(10*time.Second))
	updates = st.takeTaskUpdates()
	summary, ok = lastProgressTask(updates, turnProgressTaskID)
	if !ok || summary.Title != "Completed · 50 actions · 10s" {
		t.Fatalf("completion summary = %+v, want fifty actions", updates)
	}
	for _, update := range updates {
		if strings.HasPrefix(update.ID, "tool-step-") {
			t.Fatalf("a single-title turn published a plan row: %+v", update)
		}
	}
}

func TestDistinctTitlePromotesGroupedPlan(t *testing.T) {
	st := newTurnState()
	log := discardLogger()

	for i := 1; i <= 3; i++ {
		id := fmt.Sprintf("bash-%d", i)
		st.feed(toolEvent(t, "tool_execution_start", id, "bash", false), log)
		st.feed(toolEvent(t, "tool_execution_end", id, "bash", false), log)
	}
	st.feed(textDelta(t, "[[slack-progress: Reading files]]"), log)
	st.feed(toolEvent(t, "tool_execution_start", "read-1", "read", false), log)

	updates := st.takeTaskUpdates()
	if !st.progressPlanStarted {
		t.Fatal("a second distinct title did not promote the plan")
	}
	first, ok := lastProgressTask(updates, "tool-step-1")
	if !ok || first.Title != "Running a command (×3)" {
		t.Fatalf("first grouped row = %+v, want three commands", updates)
	}
	second, ok := lastProgressTask(updates, "tool-step-2")
	if !ok || second.Title != "Reading files" {
		t.Fatalf("second row = %+v, want the distinct title", updates)
	}
}

func TestGroupedRowWaitsForEveryCallAndKeepsAnError(t *testing.T) {
	st := newTurnState()
	log := discardLogger()

	st.feed(toolEvent(t, "tool_execution_start", "call-1", "bash", false), log)
	st.feed(toolEvent(t, "tool_execution_start", "call-2", "bash", false), log)
	st.feed(toolEvent(t, "tool_execution_end", "call-1", "bash", false), log)
	if step := st.activeTasks["tool-step-1"]; step.Status != TaskInProgress {
		t.Fatalf("grouped row after one return = %+v, want in-progress", step)
	}
	st.feed(toolEvent(t, "tool_execution_end", "call-2", "bash", true), log)
	if step := st.activeTasks["tool-step-1"]; step.Status != TaskError {
		t.Fatalf("grouped row after all returns = %+v, want error", step)
	}
	if len(st.progressTaskOrder) != 1 {
		t.Fatalf("report rows = %d, want one grouped row", len(st.progressTaskOrder))
	}
}

func TestMissingProgressMarkerIsLoggedOnce(t *testing.T) {
	var logs bytes.Buffer
	log := slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	st := newTurnState()

	for i := 1; i <= 2; i++ {
		st.feed(toolEvent(t, "tool_execution_start", fmt.Sprintf("call-%d", i), "bash", false), log)
	}
	if got := strings.Count(logs.String(), "without a progress marker"); got != 1 {
		t.Fatalf("missing-marker log count = %d, want 1:\n%s", got, logs.String())
	}

	st.feed(textDelta(t, "[[slack-progress: Marked action]]"), log)
	st.feed(toolEvent(t, "tool_execution_start", "call-3", "read", false), log)
	if got := strings.Count(logs.String(), "without a progress marker"); got != 1 {
		t.Fatalf("a marker logged a fallback title: count = %d\n%s", got, logs.String())
	}
}

func TestTurnStateCanRecoverTheFinalTextAfterReconnect(t *testing.T) {
	st := newTurnState()
	st.append("partial")
	if got := st.takePending(); got != "partial" {
		t.Fatalf("pending text = %q, want partial", got)
	}

	st.replace("partial answer")
	if got := st.text(); got != "partial answer" {
		t.Fatalf("full text = %q, want partial answer", got)
	}
	if got := st.takePending(); got != " answer" {
		t.Fatalf("pending text = %q, want only the suffix not yet rendered", got)
	}

	st = newTurnState()
	st.append("old partial")
	st.takePending()
	st.replace("corrected answer")
	if got := st.takePending(); got != "" {
		t.Fatalf("pending text = %q, want no duplicate for a divergent correction", got)
	}
}

func TestReconcileObservedTextRecoversMissedDeltas(t *testing.T) {
	before := sha256.Sum256([]byte("previous answer"))

	st := newTurnState()
	if !reconcileObservedText(st, "completed while disconnected", before, true) {
		t.Fatal("a changed final answer should be recovered even when no deltas arrived")
	}
	if got := st.text(); got != "completed while disconnected" {
		t.Fatalf("recovered text = %q", got)
	}

	st = newTurnState()
	st.append("partial")
	if !reconcileObservedText(st, "partial plus missed text", before, true) {
		t.Fatal("the authoritative final answer should replace a partial stream")
	}
	if got := st.text(); got != "partial plus missed text" {
		t.Fatalf("reconciled text = %q", got)
	}

	st = newTurnState()
	if reconcileObservedText(st, "previous answer", before, true) {
		t.Fatal("a textless wake should not replay the previous answer")
	}
}

func TestTurnStateSeparatesAssistantMessages(t *testing.T) {
	st := newTurnState()
	log := discardLogger()

	st.feed(gwclient.Event{Type: "message_start"}, log)
	st.feed(textDelta(t, "First "), log)
	st.feed(textDelta(t, "response."), log)
	if got := st.takePending(); got != "First response." {
		t.Fatalf("first message = %q, want %q", got, "First response.")
	}

	st.feed(gwclient.Event{Type: "message_start"}, log)
	st.feed(textDelta(t, "Second"), log)
	if got := st.takePending(); got != "\n\nSecond" {
		t.Fatalf("next message delta = %q, want a paragraph break before it", got)
	}
	if got := st.text(); got != "First response.\n\nSecond" {
		t.Fatalf("full text = %q, want both messages separated by a paragraph", got)
	}
	if got := st.authoritativeText("Final second response."); got != "First response.\n\nFinal second response." {
		t.Fatalf("authoritative text = %q, want prior message and corrected final response", got)
	}
}

func TestTurnStateSkipsEmptyAssistantMessages(t *testing.T) {
	st := newTurnState()
	log := discardLogger()

	st.feed(gwclient.Event{Type: "message_start"}, log)
	st.feed(gwclient.Event{Type: "message_end"}, log)
	st.feed(gwclient.Event{Type: "message_start"}, log)
	st.feed(textDelta(t, "The answer."), log)

	if got := st.text(); got != "The answer." {
		t.Fatalf("text after an empty message = %q, want no leading separator", got)
	}
}

func TestTurnStateIgnoresThinkingDeltas(t *testing.T) {
	st := newTurnState()

	// Thinking arrives in the same shape with a different type; rendering it as
	// the answer would show the user the model's private reasoning.
	st.feed(gwclient.Event{
		Type: "message_update",
		Raw:  []byte(`{"type":"message_update","assistantMessageEvent":{"type":"thinking_delta","delta":"secret"}}`),
	}, discardLogger())

	if got := st.text(); got != "" {
		t.Fatalf("thinking leaked into the answer: %q", got)
	}
	if !st.sawWork() {
		t.Fatal("a thinking delta still means the turn is running")
	}
}

func TestTurnStateLifecycleEvents(t *testing.T) {
	log := discardLogger()

	t.Run("running and settled", func(t *testing.T) {
		st := newTurnState()
		st.feed(gwclient.Event{Type: "gw_turn", Raw: []byte(`{"type":"gw_turn","state":"running"}`)}, log)
		if !st.sawWork() {
			t.Fatal("gw_turn running should mark work")
		}
		select {
		case <-st.doneCh:
			t.Fatal("settled too early")
		default:
		}
		st.feed(gwclient.Event{Type: "gw_turn", Raw: []byte(`{"type":"gw_turn","state":"settled"}`)}, log)
		select {
		case <-st.doneCh:
		default:
			t.Fatal("gw_turn settled should mark the turn done")
		}
	})

	t.Run("agent settled", func(t *testing.T) {
		st := newTurnState()
		st.feed(gwclient.Event{Type: "agent_settled", Raw: []byte(`{"type":"agent_settled"}`)}, log)
		select {
		case <-st.doneCh:
		default:
			t.Fatal("agent_settled should mark the turn done")
		}
	})

	t.Run("a finished message is not a finished turn", func(t *testing.T) {
		// Observed against the real daemon: one prompt emitted two messages, the
		// first of them empty. Treating that message_end as the end of the turn
		// stopped the flusher before the answer had streamed at all.
		st := newTurnState()
		for _, typ := range []string{"message_start", "message_end", "turn_end", "agent_end"} {
			st.feed(gwclient.Event{Type: typ, Raw: []byte(`{"type":"` + typ + `"}`)}, log)
		}
		if !st.sawWork() {
			t.Fatal("these events should still count as the turn running")
		}
		select {
		case <-st.doneCh:
			t.Fatal("message_end and turn_end must not end the turn")
		default:
		}

		// Text arriving after them is still rendered.
		st.feed(textDelta(t, "late answer"), log)
		if got := st.takePending(); got != "late answer" {
			t.Fatalf("pending text = %q, want the late answer", got)
		}
	})
}

func TestWaitStarted(t *testing.T) {
	log := discardLogger()

	t.Run("work", func(t *testing.T) {
		st := newTurnState()
		go func() {
			time.Sleep(5 * time.Millisecond)
			st.feed(textDelta(t, "x"), log)
		}()
		if err := st.waitStarted(context.Background(), time.Second); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("done without work", func(t *testing.T) {
		st := newTurnState()
		st.markDone()
		if err := st.waitStarted(context.Background(), time.Second); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("grace expires", func(t *testing.T) {
		st := newTurnState()
		// A silent turn is not an error here: the caller decides what it means.
		if err := st.waitStarted(context.Background(), time.Millisecond); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("context canceled", func(t *testing.T) {
		st := newTurnState()
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		if err := st.waitStarted(ctx, time.Second); !errors.Is(err, context.Canceled) {
			t.Fatalf("err = %v, want context.Canceled", err)
		}
	})
}

// captureLogger returns a logger and the buffer it writes to.
func captureLogger() (*slog.Logger, *strings.Builder) {
	var buf strings.Builder
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	return logger, &buf
}

// uiEvent builds an extension_ui_request frame.
func uiEvent(t *testing.T, body string) gwclient.Event {
	t.Helper()
	raw := `{"type":"extension_ui_request",` + body + `}`
	if !json.Valid([]byte(raw)) {
		t.Fatalf("test built invalid JSON: %s", raw)
	}
	return gwclient.Event{Type: "extension_ui_request", Raw: []byte(raw)}
}

func TestFireAndForgetUINeedsNoAnswer(t *testing.T) {
	// pi-lens emits setStatus on every turn, `--approve` does not silence it,
	// and nothing waits for an answer. Warning about it would train the reader
	// to ignore warnings, while per-request debug records add no useful signal.
	log, logs := captureLogger()
	th := &thread{log: log}
	st := newTurnState()
	st.renderable.Store(true)
	th.cur.Store(st)
	th.onEvent(uiEvent(t, `"id":"1","method":"setStatus","statusKey":"pi-lens-lsp",`+
		`"statusText":"\u001b[38;5;241mLSP Inactive\u001b[39m"`))

	if got := logs.String(); got != "" {
		t.Fatalf("an ignored fire-and-forget request should not be logged: %s", got)
	}
	// It is not a turn boundary either.
	select {
	case <-st.doneCh:
		t.Fatal("a notification must not end the turn")
	default:
	}
}

func TestMessageUpdateDebugRecordsAreSuppressed(t *testing.T) {
	log, logs := captureLogger()
	th := &thread{log: log}
	st := newTurnState()
	st.renderable.Store(true)
	th.cur.Store(st)

	th.onEvent(textDelta(t, "streamed answer"))
	if got := st.text(); got != "streamed answer" {
		t.Fatalf("message_update was not applied: %q", got)
	}
	if got := logs.String(); got != "" {
		t.Fatalf("message_update emitted a debug record: %s", got)
	}

	th.onEvent(toolEvent(t, "tool_execution_start", "call-1", "bash", false))
	if got := logs.String(); !strings.Contains(got, "gateway event") || !strings.Contains(got, "tool_execution_start") {
		t.Fatalf("actionable tool lifecycle event should remain visible: %s", got)
	}
}

func TestBlockingUIDialogIsRecognizedWithoutLoggingItsContents(t *testing.T) {
	log, logs := captureLogger()
	st := newTurnState()
	st.feed(uiEvent(t, `"id":"q1","method":"confirm","title":"Run this?",`+
		`"message":"\u001b[1mrm -rf build\u001b[0m"`), log)

	got := logs.String()
	if strings.Contains(got, "level=WARN") || !strings.Contains(got, "method=confirm") {
		t.Fatalf("a blocking dialog should be recognized at debug level: %s", got)
	}
	if strings.Contains(got, "Run this?") || strings.Contains(got, "rm -rf build") || strings.Contains(got, "\\u001b") {
		t.Fatalf("the extension-supplied dialog content should not be logged: %s", got)
	}
}

func TestReplayedBlockingUIDialogIsFedOnlyOnce(t *testing.T) {
	log := discardLogger()
	st := newTurnState()
	ev := uiEvent(t, `"id":"q2","method":"confirm"`)
	ev.Seq = 17
	if !st.feed(ev, log) {
		t.Fatal("the first UI event was marked as a replay")
	}
	if st.feed(ev, log) {
		t.Fatal("a replayed UI event was accepted twice")
	}
}

func TestNotifyWarningIsSurfaced(t *testing.T) {
	log, logs := captureLogger()
	st := newTurnState()
	st.feed(uiEvent(t, `"id":"1","method":"notify","notifyType":"warning","text":"disk nearly full"`), log)

	if got := logs.String(); !strings.Contains(got, "level=WARN") || !strings.Contains(got, "disk nearly full") {
		t.Fatalf("a warning notification should reach the journal: %s", got)
	}
}

func TestStripANSI(t *testing.T) {
	tests := []struct{ in, want string }{
		{"plain", "plain"},
		{"\x1b[38;5;241mLSP Inactive\x1b[39m", "LSP Inactive"},
		{"\x1b[1mbold\x1b[0m and \x1b[4munderline\x1b[0m", "bold and underline"},
		{"trailing\x1b", "trailing\x1b"}, // a lone ESC is not a sequence
		{"", ""},
	}
	for _, test := range tests {
		if got := stripANSI(test.in); got != test.want {
			t.Errorf("stripANSI(%q) = %q, want %q", test.in, got, test.want)
		}
	}
}

// recordingRenderer records what a turn renders.
type recordingRenderer struct {
	mu                 sync.Mutex
	starts             int
	startErr           error
	deltas             []string
	tasks              []TaskProgress
	finals             []string
	failure            string
	publicFailure      string
	failureContextErr  error
	failureHasDeadline bool
	signal             chan struct{} // if set, receives a value on every Delta
}

func (r *recordingRenderer) Start(context.Context) error {
	r.mu.Lock()
	r.starts++
	err := r.startErr
	r.mu.Unlock()
	return err
}

func (r *recordingRenderer) UpdateTasks(_ context.Context, tasks []TaskProgress) error {
	r.mu.Lock()
	r.tasks = append(r.tasks, tasks...)
	r.mu.Unlock()
	return nil
}

func (r *recordingRenderer) Delta(_ context.Context, text string) error {
	r.mu.Lock()
	r.deltas = append(r.deltas, text)
	r.mu.Unlock()
	if r.signal != nil {
		select {
		case r.signal <- struct{}{}:
		default:
		}
	}
	return nil
}

func (r *recordingRenderer) Finish(_ context.Context, final string) error {
	r.mu.Lock()
	r.finals = append(r.finals, final)
	r.mu.Unlock()
	return nil
}

func (r *recordingRenderer) Fail(ctx context.Context, cause error) error {
	_, hasDeadline := ctx.Deadline()
	r.mu.Lock()
	r.failure = cause.Error()
	r.publicFailure = PublicMessage(cause)
	r.failureContextErr = ctx.Err()
	r.failureHasDeadline = hasDeadline
	r.mu.Unlock()
	return nil
}

func (r *recordingRenderer) failureContext() (error, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.failureContextErr, r.failureHasDeadline
}

func (r *recordingRenderer) failureText() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.failure
}

func (r *recordingRenderer) failurePublic() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.publicFailure
}

func (r *recordingRenderer) finalAnswers() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.finals...)
}

func (r *recordingRenderer) rendered() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return strings.Join(r.deltas, "")
}

func (r *recordingRenderer) taskProgress() []TaskProgress {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]TaskProgress(nil), r.tasks...)
}

func (r *recordingRenderer) startCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.starts
}

func TestFlushRetainsTextWhenReplyStartFails(t *testing.T) {
	cfg := config.Defaults()
	cfg.Render.FlushMS = 60000
	b := &Bot{cfg: cfg, log: discardLogger()}
	th := &thread{key: "test", b: b, log: b.log}
	r := &recordingRenderer{startErr: errors.New("Slack unavailable")}
	st := newTurnState()

	stop := th.flush(context.Background(), st, r)
	st.feed(textDelta(t, "partial answer"), discardLogger())
	stop()

	if got := r.rendered(); got != "partial answer" {
		t.Fatalf("buffered text = %q, want partial answer available to Fail", got)
	}
}

func TestObservedTurnRendersTaskProgressWithoutAssistantText(t *testing.T) {
	b, platform, st := newTestBot(t)
	b.lifetime = context.Background()
	th := testThread(t, b, st)
	r := &recordingRenderer{}
	platform.renderer = r
	turn := newTurnState()
	turn.feed(toolEvent(t, "tool_execution_start", "call-1", "read", false), discardLogger())
	turn.feed(toolEvent(t, "tool_execution_end", "call-1", "read", false), discardLogger())
	turn.feed(gwclient.Event{Type: "agent_settled"}, discardLogger())

	th.renderObservedTurn(turn, nil)

	if got := platform.startedTurns(); got != 1 {
		t.Fatalf("renderer starts = %d, want a task-only reply", got)
	}
	if got := r.taskProgress(); len(got) != 2 || got[0].ID != turnProgressTaskID || got[0].Status != TaskInProgress || got[1].ID != turnProgressTaskID || got[1].Status != TaskComplete {
		t.Fatalf("task updates = %+v, want in-progress followed by completion", got)
	}
	if got := r.startCount(); got != 0 {
		t.Fatalf("empty text reply started %d times", got)
	}
	if got := r.finalAnswers(); len(got) != 1 || got[0] != "" {
		t.Fatalf("task-only stream finalization = %q, want an empty final answer", got)
	}
}

func TestFlushForwardsTaskProgressBeforeAnyAssistantText(t *testing.T) {
	cfg := config.Defaults()
	cfg.Render.FlushMS = 60000
	b := &Bot{cfg: cfg, log: discardLogger()}
	th := &thread{key: "test", b: b, log: b.log}
	r := &recordingRenderer{}
	st := newTurnState()

	stop := th.flush(context.Background(), st, r)
	st.feed(toolEvent(t, "tool_execution_start", "call-1", "read", false), discardLogger())
	waitFor(t, "task progress", func() bool { return len(r.taskProgress()) == 1 })
	if r.startCount() != 0 {
		t.Fatalf("task progress started an empty text reply %d times", r.startCount())
	}

	st.feed(textDelta(t, "answer"), discardLogger())
	waitFor(t, "assistant text", func() bool { return r.rendered() == "answer" })
	stop()
	if got := r.taskProgress()[0]; got.Title != "Reading files" || got.Status != TaskInProgress {
		t.Fatalf("reported task = %+v", got)
	}
}

func TestFlushThinkingOnlyDoesNotStartReply(t *testing.T) {
	cfg := config.Defaults()
	cfg.Render.FlushMS = 60000
	b := &Bot{cfg: cfg, log: discardLogger()}
	th := &thread{key: "test", b: b, log: b.log}
	r := &recordingRenderer{}
	st := newTurnState()

	stop := th.flush(context.Background(), st, r)
	st.feed(gwclient.Event{
		Type: "message_update",
		Raw:  []byte(`{"type":"message_update","assistantMessageEvent":{"type":"thinking_delta","delta":"private reasoning"}}`),
	}, discardLogger())
	stop()

	if got := r.startCount(); got != 0 {
		t.Fatalf("Start called %d times while only thinking was emitted", got)
	}
}

func TestFlushStartsImmediatelyOnFirstText(t *testing.T) {
	cfg := config.Defaults()
	cfg.Render.FlushMS = 60000
	b := &Bot{cfg: cfg, log: discardLogger()}
	th := &thread{key: "test", b: b, log: b.log}
	r := &recordingRenderer{signal: make(chan struct{}, 8)}
	st := newTurnState()

	stop := th.flush(context.Background(), st, r)
	st.feed(textDelta(t, "visible answer"), discardLogger())
	defer stop()

	select {
	case <-r.signal:
	case <-time.After(2 * time.Second):
		t.Fatalf("first text was not rendered immediately; got %q", r.rendered())
	}
	if got := r.startCount(); got != 1 {
		t.Fatalf("Start called %d times for the first text chunk, want 1", got)
	}
}

// tickingRenderer exercises the optional TurnTicker capability.
type tickingRenderer struct {
	recordingRenderer
	ticked chan struct{}
}

func (r *tickingRenderer) Tick(ctx context.Context) error {
	if _, hasDeadline := ctx.Deadline(); !hasDeadline {
		return errors.New("renderer tick has no deadline")
	}
	select {
	case r.ticked <- struct{}{}:
	default:
	}
	return nil
}

func TestFlushTicksRenderersThatAskForIt(t *testing.T) {
	cfg := config.Defaults()
	cfg.Render.FlushMS = 5
	b := &Bot{cfg: cfg, log: discardLogger()}
	th := &thread{key: "test", b: b, log: b.log}
	r := &tickingRenderer{ticked: make(chan struct{}, 8)}
	st := newTurnState()

	stop := th.flush(context.Background(), st, r)
	defer stop()
	select {
	case <-r.ticked:
	case <-time.After(2 * time.Second):
		t.Fatal("renderer did not receive a heartbeat while the turn runs")
	}
}

// tickHandleRenderer changes its message handle when the core ticks it, the way
// the Slack renderer rotates a stream without producing text or a task update.
type tickHandleRenderer struct {
	recordingRenderer
	handleMu sync.Mutex
	ts       string
}

func (r *tickHandleRenderer) Progress() string {
	r.handleMu.Lock()
	defer r.handleMu.Unlock()
	return r.ts
}

func (r *tickHandleRenderer) Tick(context.Context) error {
	r.handleMu.Lock()
	r.ts = "1700000001.000001"
	r.handleMu.Unlock()
	return nil
}

// deltaHandleRenderer changes its handle while rendering text, the way a stream
// may rotate just before a delta is appended.
type deltaHandleRenderer struct {
	recordingRenderer
	handleMu sync.Mutex
	ts       string
}

func (r *deltaHandleRenderer) Progress() string {
	r.handleMu.Lock()
	defer r.handleMu.Unlock()
	return r.ts
}

func (r *deltaHandleRenderer) Delta(ctx context.Context, text string) error {
	r.handleMu.Lock()
	r.ts = "1700000002.000001"
	r.handleMu.Unlock()
	return r.recordingRenderer.Delta(ctx, text)
}

func TestFlushPersistsAHandleChangedByATick(t *testing.T) {
	b, _, st := newTestBot(t)
	b.cfg.Render.FlushMS = 5
	th := seedStopThread(t, b, st)
	r := &tickHandleRenderer{}
	turn := newTurnState()

	stop := th.flush(context.Background(), turn, r)
	defer stop()
	waitFor(t, "the rotated handle", func() bool {
		row, ok, err := st.Thread(context.Background(), th.key)
		return err == nil && ok && row.ProgressTS == "1700000001.000001"
	})
}

func TestFlushPersistsAHandleChangedWhileRenderingText(t *testing.T) {
	b, _, st := newTestBot(t)
	b.cfg.Render.FlushMS = 5
	th := seedStopThread(t, b, st)
	r := &deltaHandleRenderer{}
	turn := newTurnState()

	stop := th.flush(context.Background(), turn, r)
	defer stop()
	turn.append("hello")
	waitFor(t, "the rotated handle", func() bool {
		row, ok, err := st.Thread(context.Background(), th.key)
		return err == nil && ok && row.ProgressTS == "1700000002.000001"
	})
}

func TestFlushRendersPendingText(t *testing.T) {
	cfg := config.Defaults()
	cfg.Render.FlushMS = 5
	b := &Bot{cfg: cfg, log: discardLogger()}
	th := &thread{key: "test", b: b, log: b.log}
	r := &recordingRenderer{signal: make(chan struct{}, 8)}
	st := newTurnState()

	stop := th.flush(context.Background(), st, r)
	st.append("hello ")
	st.append("world")

	// Wait for the flusher rather than for the clock.
	timeout := time.After(2 * time.Second)
	for r.rendered() != "hello world" {
		select {
		case <-r.signal:
		case <-timeout:
			t.Fatalf("rendered %q, want %q", r.rendered(), "hello world")
		}
	}
	stop()

	// Stopping twice must be safe: a turn stops the flusher before it finishes
	// and again on the way out.
	stop()
}

func TestQueuedRequestIsMarkedQueuedThenStarted(t *testing.T) {
	b, platform, st := newTestBot(t)
	th := testThread(t, b, st)
	message := newTestMessage("queued-request", "hello")
	message.TS = "1700000000.000200"
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	platform.mu.Lock()
	platform.onStart = cancel
	platform.queueError = errors.New("queue reaction failed")
	platform.requestStartError = errors.New("start reaction failed")
	platform.mu.Unlock()

	active := newPromptTurnState()
	th.cur.Store(active)
	th.turnMu.Lock()
	done := make(chan error, 1)
	go func() { done <- th.runTurn(ctx, message, &recordingRenderer{}) }()

	waitFor(t, "queued request reaction", func() bool { return len(platform.queuedRequests()) == 1 })
	if got := platform.startedRequests(); len(got) != 0 {
		t.Fatalf("request started before the active turn finished: %+v", got)
	}
	th.cur.CompareAndSwap(active, nil)
	active.finish()
	th.turnMu.Unlock()
	select {
	case err := <-done:
		if err == nil || strings.Contains(err.Error(), "reaction failed") {
			t.Errorf("runTurn error = %v, want the gateway setup failure, not a reaction callback failure", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("queued turn did not return after its start callback")
	}

	if got := platform.queuedRequests(); len(got) != 1 || got[0].TS != message.TS {
		t.Errorf("queued requests = %+v, want the waiting message", got)
	}
	if got := platform.startedRequests(); len(got) != 1 || got[0].message.TS != message.TS || !got[0].queued {
		t.Errorf("started requests = %+v, want the queued message marked as started", got)
	}
}

// A request that cannot get its thread's turn slot within the admission budget
// is answered instead of held: a turn whose tool call never returns must not
// swallow every later message in the thread without a trace.
func TestQueuedRequestIsAnsweredWhenTheTurnSlotNeverFrees(t *testing.T) {
	b, platform, st := newTestBotWith(t, func(cfg *config.Config) {
		cfg.Concurrency.AdmissionWaitSeconds = 1
	})
	th := testThread(t, b, st)
	message := newTestMessage("stuck-request", "what is the progress?")
	message.TS = "1700000000.000300"

	active := newPromptTurnState()
	active.feed(toolEvent(t, "tool_execution_start", "call-1", "bash", false), discardLogger())
	th.cur.Store(active)
	// A running turn holds the slot for its whole life; this one never ends.
	th.turnMu.Lock()
	defer func() {
		th.cur.CompareAndSwap(active, nil)
		active.finish()
		th.turnMu.Unlock()
	}()

	err := th.runTurn(context.Background(), message, &recordingRenderer{})
	var refusal *busyTurnError
	if !errors.As(err, &refusal) {
		t.Fatalf("runTurn error = %v, want a busy refusal", err)
	}
	for _, want := range []string{"still running", "running a command", "@pi /abort"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal %q does not mention %q", err, want)
		}
	}
	if got := platform.queuedRequests(); len(got) != 1 || got[0].TS != message.TS {
		t.Errorf("queued requests = %+v, want the waiting message", got)
	}
	if got := platform.startedRequests(); len(got) != 0 {
		t.Errorf("started requests = %+v, want none for a refused request", got)
	}
}

func TestBusySummaryNamesTheOpenToolCallAndForgetsItWhenItReturns(t *testing.T) {
	st := newTurnState()
	st.feed(toolEvent(t, "tool_execution_start", "call-1", "bash", false), discardLogger())
	if got := st.runningSummary(time.Now()); !strings.Contains(got, "running a command") {
		t.Errorf("summary while a tool is open = %q, want the open tool call", got)
	}
	st.feed(toolEvent(t, "tool_execution_end", "call-1", "bash", false), discardLogger())
	if got := st.runningSummary(time.Now()); strings.Contains(got, "command") {
		t.Errorf("summary after the tool returned = %q, want no tool call", got)
	}
}

func TestCanceledTurnMarksOriginalRequestFailed(t *testing.T) {
	b, platform, st := newTestBot(t)
	th := testThread(t, b, st)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	message := newTestMessage("failed-turn", "hello")

	b.startTurn(ctx, th, message)
	waitFor(t, "failed request reaction", func() bool { return len(platform.failedRequests()) == 1 })
	if got := platform.failedRequests()[0].TS; got != message.TS {
		t.Errorf("failed request timestamp = %q, want %q", got, message.TS)
	}
	if got := len(platform.completedRequests()); got != 0 {
		t.Errorf("completed %d requests after the turn failed", got)
	}
	assertFailureReporterUsesLiveBoundedContext(t, platform)
	platform.mu.Lock()
	renderer := platform.renderer
	platform.mu.Unlock()
	if err, hasDeadline := renderer.failureContext(); err != nil || !hasDeadline {
		t.Fatalf("renderer failure context err/deadline = %v/%v, want live and bounded", err, hasDeadline)
	}
}

func TestStartTurnErrorMarksOriginalRequestFailed(t *testing.T) {
	b, platform, st := newTestBot(t)
	platform.startErr = errors.New("cannot start renderer")
	th := testThread(t, b, st)
	message := newTestMessage("start-failure", "hello")

	b.startTurn(context.Background(), th, message)
	waitFor(t, "failed request reaction", func() bool { return len(platform.failedRequests()) == 1 })
	if got := platform.startedTurns(); got != 1 {
		t.Errorf("StartTurn calls = %d, want 1", got)
	}
	if got := platform.failedRequests()[0].TS; got != message.TS {
		t.Errorf("failed request timestamp = %q, want %q", got, message.TS)
	}
	if got := len(platform.completedRequests()); got != 0 {
		t.Errorf("completed %d requests after StartTurn failed", got)
	}
	notices := platform.postedNotices()
	if len(notices) != 1 || !strings.Contains(notices[0].Text, "I couldn't start the reply") {
		t.Fatalf("notices = %+v, want a user-facing start failure", notices)
	}
	if strings.Contains(notices[0].Text, "cannot start renderer") {
		t.Fatalf("the internal cause leaked into the notice: %q", notices[0].Text)
	}
	assertFailureReporterUsesLiveBoundedContext(t, platform)
}

func assertFailureReporterUsesLiveBoundedContext(t *testing.T, platform *fakePlatform) {
	t.Helper()
	reports := platform.requestFailureReports()
	if len(reports) != 1 {
		t.Fatalf("failure reports = %d, want 1", len(reports))
	}
	if reports[0].contextErr != nil {
		t.Errorf("failure reporter context error = %v, want a live context", reports[0].contextErr)
	}
	if !reports[0].hasDeadline {
		t.Fatal("failure reporter context has no deadline")
	}
	if remaining := time.Until(reports[0].deadline); remaining <= 0 || remaining > requestReactionTimeout {
		t.Errorf("failure reporter deadline has %v remaining, want >0 and <=%v", remaining, requestReactionTimeout)
	}
}

func TestRunTurnSetupFailureDoesNotCompleteRequest(t *testing.T) {
	b, platform, st := newTestBot(t)
	th := testThread(t, b, st)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	r := &recordingRenderer{}

	err := th.runTurn(ctx, newTestMessage("setup-failure", "hello"), r)
	if err == nil {
		t.Fatal("runTurn with a canceled context unexpectedly succeeded")
	}
	if got := len(platform.completedRequests()); got != 0 {
		t.Errorf("completed %d requests after setup failed before any output", got)
	}
	if got := r.startCount(); got != 0 {
		t.Errorf("Start called %d times despite failure before output", got)
	}
}

func TestFlushRendersBeforeExiting(t *testing.T) {
	// A short answer can arrive entirely between two ticks. Dropping it would
	// mean a thread that shows nothing until the turn ends, so the flusher
	// renders once on the way out.
	cfg := config.Defaults()
	cfg.Render.FlushMS = 60000
	b := &Bot{cfg: cfg, log: discardLogger()}
	th := &thread{key: "test", b: b, log: b.log}
	r := &recordingRenderer{}
	st := newTurnState()

	stop := th.flush(context.Background(), st, r)
	st.append("P")
	st.append("ONG")
	st.markDone() // the turn ends before the first tick is due
	stop()

	if got := r.rendered(); got != "PONG" {
		t.Fatalf("rendered %q, want the pending text to be flushed on exit", got)
	}
}
