package slack

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/tigersoldier/pi-chat/internal/bot"
)

func TestPresentApprovalPostsPlainTextWithAllChoiceBlocks(t *testing.T) {
	stub := &stubSlack{}
	platform := newTestPlatform(t, stub, "patch")
	thread := bot.Thread{Workspace: "T1", Channel: "C1", ThreadTS: "1.1"}
	buttons := make([]bot.Button, approvalActionsPerBlock+1)
	for i := range buttons {
		buttons[i] = bot.Button{
			ActionID: bot.ActionUISelect,
			Text:     "choice",
			Value:    `{"route_id":"d1","value":"choice"}`,
		}
	}
	ts, err := platform.PresentApproval(context.Background(), thread, bot.ApprovalRequest{
		ID: "d1", Method: "select", Title: "@everyone", Message: "Choose *one*.",
	}, buttons)
	if err != nil {
		t.Fatalf("PresentApproval: %v", err)
	}
	if ts == "" {
		t.Fatal("PresentApproval returned an empty message timestamp")
	}
	calls := stub.recorded()
	if len(calls) != 1 || calls[0].method != "chat.postMessage" {
		t.Fatalf("calls = %+v, want one chat.postMessage", calls)
	}
	if calls[0].params["thread_ts"] != thread.ThreadTS {
		t.Errorf("thread_ts = %v, want %q", calls[0].params["thread_ts"], thread.ThreadTS)
	}
	var blocks []struct {
		Type string `json:"type"`
		Text struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"text"`
		Elements []struct {
			ActionID string `json:"action_id"`
			Value    string `json:"value"`
		} `json:"elements"`
	}
	jsonParam(t, calls[0].params, "blocks", &blocks)
	if len(blocks) != 3 || blocks[0].Type != "section" || blocks[0].Text.Type != "plain_text" {
		t.Fatalf("approval blocks = %+v, want one plain-text prompt and two action blocks", blocks)
	}
	if blocks[0].Text.Text != "@everyone\n\nChoose *one*." {
		t.Errorf("approval text = %q", blocks[0].Text.Text)
	}
	if len(blocks[1].Elements) != approvalActionsPerBlock || len(blocks[2].Elements) != 1 {
		t.Fatalf("choice blocks contain %d and %d elements, want %d and 1", len(blocks[1].Elements), len(blocks[2].Elements), approvalActionsPerBlock)
	}
	if blocks[1].Elements[0].ActionID != bot.ActionUISelect+":0" || blocks[2].Elements[0].ActionID != bot.ActionUISelect+":0" {
		t.Errorf("unexpected action ids across blocks: %q and %q", blocks[1].Elements[0].ActionID, blocks[2].Elements[0].ActionID)
	}
}

func TestMaxApprovalChoicesFitsSlackMessageBudget(t *testing.T) {
	if got, want := (&Platform{}).MaxApprovalChoices(), (approvalMaxBlocks-approvalPromptBlocks)*approvalActionsPerBlock-approvalCancelButtons; got != want {
		t.Fatalf("MaxApprovalChoices = %d, want %d", got, want)
	}
	buttons := make([]bot.Button, approvalMaxChoices+1) // choices plus Cancel
	blocks := approvalBlocksFor(bot.ApprovalRequest{Method: "select"}, buttons)
	if len(blocks) != approvalMaxBlocks {
		t.Fatalf("rendered %d blocks, want Slack's %d-block maximum", len(blocks), approvalMaxBlocks)
	}
	for i, approvalBlock := range blocks[1:] {
		elements := approvalBlock["elements"].([]block)
		if len(elements) > approvalActionsPerBlock {
			t.Errorf("actions block %d has %d elements, max %d", i, len(elements), approvalActionsPerBlock)
		}
	}
}

func TestOpenApprovalModalUsesTriggerAndPrivateDialogID(t *testing.T) {
	stub := &stubSlack{}
	platform := newTestPlatform(t, stub, "patch")
	err := platform.OpenApprovalModal(context.Background(), bot.Thread{}, "trigger-1", "dialog-1", bot.ApprovalRequest{
		ID: "dialog-1", Method: "editor", Title: "Edit", Message: "Replace the plan.",
		Placeholder: "New plan", Prefill: "keep this",
	})
	if err != nil {
		t.Fatalf("OpenApprovalModal: %v", err)
	}
	calls := stub.recorded()
	if len(calls) != 1 || calls[0].method != "views.open" {
		t.Fatalf("calls = %+v, want views.open", calls)
	}
	if calls[0].params["trigger_id"] != "trigger-1" {
		t.Errorf("trigger_id = %v", calls[0].params["trigger_id"])
	}
	var view map[string]any
	jsonParam(t, calls[0].params, "view", &view)
	if view["callback_id"] != approvalViewCallbackID || view["private_metadata"] != "dialog-1" || view["notify_on_close"] != true {
		t.Errorf("view metadata = %#v", view)
	}
	blocks, ok := view["blocks"].([]any)
	if !ok || len(blocks) != 2 {
		t.Fatalf("view blocks = %#v", view["blocks"])
	}
	inputBlock, _ := blocks[1].(map[string]any)
	element, _ := inputBlock["element"].(map[string]any)
	if element["multiline"] != true || element["initial_value"] != "keep this" {
		t.Errorf("editor input = %#v", element)
	}
}

func TestParseApprovalButtonAndModalActions(t *testing.T) {
	receivedAt := time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC)
	button := Envelope{EnvelopeID: "Ev-button", Type: "interactive", ReceivedAt: receivedAt, Payload: []byte(
		`{"type":"block_actions","team":{"id":"T1"},"user":{"id":"U1"},` +
			`"channel":{"id":"C1"},"message":{"ts":"2","thread_ts":"1.1"},` +
			`"trigger_id":"trigger-1","actions":[{"action_id":"ui_select:0",` +
			`"value":"{\"route_id\":\"d1\",\"value\":\"selected\"}"}]}`)}
	action, ok := parseAction(button)
	if !ok || action.ActionID != bot.ActionUISelect || action.RouteID != "d1" || action.Value != "selected" || action.ApprovalHandle != "trigger-1" {
		t.Fatalf("parsed button = %+v, ok=%v", action, ok)
	}
	if want := receivedAt.Add(approvalHandleBudget); !action.ApprovalHandleExpiresAt.Equal(want) {
		t.Errorf("approval handle expires at %s, want %s", action.ApprovalHandleExpiresAt, want)
	}
	if action.Thread == nil || action.Thread.ThreadTS != "1.1" {
		t.Fatalf("button thread = %+v", action.Thread)
	}

	for _, tc := range []struct {
		name      string
		typ       string
		want      string
		state     string
		wantValue string
	}{
		{name: "submit", typ: "view_submission", want: bot.ActionUISubmit, state: `"state":{"values":{"pi_chat_approval_input":{"value":{"value":"answer"}}}}`, wantValue: "answer"},
		{name: "close", typ: "view_closed", want: bot.ActionUICancel},
	} {
		t.Run(tc.name, func(t *testing.T) {
			payload := `{"type":"` + tc.typ + `","team":{"id":"T1"},"user":{"id":"U1"},` +
				`"view":{"callback_id":"` + approvalViewCallbackID + `","private_metadata":"d2"`
			if tc.state != "" {
				payload += "," + tc.state
			}
			payload += "}}"
			if !json.Valid([]byte(payload)) {
				t.Fatalf("invalid test payload: %s", payload)
			}
			action, ok := parseAction(Envelope{EnvelopeID: "Ev-" + tc.name, Type: "interactive", Payload: []byte(payload)})
			if !ok || action.ActionID != tc.want || action.RouteID != "d2" || action.Value != tc.wantValue {
				t.Fatalf("parsed modal action = %+v, ok=%v", action, ok)
			}
		})
	}
}
