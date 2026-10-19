package bot

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"strings"
	"testing"
	"time"

	"github.com/tigersoldier/pi-chat/internal/store"
)

type approvalPromptRecord struct {
	thread  Thread
	request ApprovalRequest
	buttons []Button
}

type approvalModalRecord struct {
	thread    Thread
	triggerID string
	dialogID  string
	request   ApprovalRequest
	deadline  time.Time
}

type approvalFakePlatform struct {
	*fakePlatform
	prompts    []approvalPromptRecord
	modals     []approvalModalRecord
	presentErr error
	modalErr   error
}

func (p *approvalFakePlatform) PresentApproval(_ context.Context, thread Thread, request ApprovalRequest, buttons []Button) (string, error) {
	p.prompts = append(p.prompts, approvalPromptRecord{thread: thread, request: request, buttons: append([]Button(nil), buttons...)})
	if p.presentErr != nil {
		return "", p.presentErr
	}
	return "approval-message-ts", nil
}

func (p *approvalFakePlatform) OpenApprovalModal(ctx context.Context, thread Thread, handle, dialogID string, request ApprovalRequest) error {
	deadline, _ := ctx.Deadline()
	p.modals = append(p.modals, approvalModalRecord{thread: thread, triggerID: handle, dialogID: dialogID, request: request, deadline: deadline})
	return p.modalErr
}

func (p *approvalFakePlatform) MaxApprovalChoices() int { return 1224 }

type uiResponseRecord struct {
	id       string
	response map[string]any
}

type recordingUIResponder struct {
	calls []uiResponseRecord
	err   error
}

func (r *recordingUIResponder) RespondUI(_ context.Context, id string, response map[string]any) error {
	copy := maps.Clone(response)
	r.calls = append(r.calls, uiResponseRecord{id: id, response: copy})
	return r.err
}

func approvalTestThread() Thread {
	return Thread{Workspace: "T1", Channel: "C1", ThreadTS: "1700000000.000100"}
}

func approvalTestBot(t *testing.T) (*Bot, *approvalFakePlatform, *store.Store, *thread) {
	t.Helper()
	b, platform, st := newTestBot(t)
	fake := &approvalFakePlatform{fakePlatform: platform}
	b.plat = fake
	b.cfg.Slack.Access.AllowedUsers = []string{"U1"}
	th, err := b.threadFor(approvalTestThread())
	if err != nil {
		t.Fatalf("threadFor: %v", err)
	}
	th.update(func(row *store.ThreadRow) { row.SessionPath = "/sessions/approval.jsonl" })
	return b, fake, st, th
}

func TestPresentApprovalPersistsItsRouteAndSanitizesDisplayText(t *testing.T) {
	_, platform, st, th := approvalTestBot(t)
	client := &recordingUIResponder{}
	request := ApprovalRequest{
		ID: "dialog-1", Method: "confirm", Title: "\x1b[31mRun this?\x1b[0m",
		Message: "Check\x1b[1m build output\x1b[0m", Options: []string{"one"},
	}
	th.presentApproval(client, "/sessions/approval.jsonl", 1, "U1", request, 60000)

	if len(platform.prompts) != 1 {
		t.Fatalf("presented %d prompts, want 1", len(platform.prompts))
	}
	prompt := platform.prompts[0]
	if prompt.request.Title != "Run this?" || prompt.request.Message != "Check build output" {
		t.Errorf("untrusted prompt text was not cleaned: %+v", prompt.request)
	}
	if len(prompt.buttons) != 2 || prompt.buttons[0].ActionID != ActionUIConfirmYes || prompt.buttons[1].ActionID != ActionUIConfirmNo {
		t.Errorf("confirm buttons = %+v", prompt.buttons)
	}
	routeID := approvalRouteID(th.key, "/sessions/approval.jsonl", 1, request.ID)
	pending, found, err := st.PendingUI(context.Background(), routeID)
	if err != nil || !found {
		t.Fatalf("PendingUI = found %v, err %v", found, err)
	}
	if pending.ThreadKey != th.key || pending.RequesterUserID != "U1" || pending.SessionPath != "/sessions/approval.jsonl" || pending.RequestID != request.ID ||
		pending.Method != "confirm" || pending.MessageTS != "approval-message-ts" {
		t.Errorf("pending route = %+v", pending)
	}
	if !pending.ExpiresAt.After(time.Now()) {
		t.Errorf("dialog expires at %s, want a future deadline", pending.ExpiresAt)
	}
	if len(client.calls) != 0 {
		t.Errorf("a shown dialog was answered before a user action: %+v", client.calls)
	}
	if statuses := platform.statuses; len(statuses) == 0 || statuses[len(statuses)-1] != StatusWaiting {
		t.Errorf("statuses = %v, want waiting", statuses)
	}
}

func TestPresentApprovalWithoutRequesterIsCanceledWithoutBeingShown(t *testing.T) {
	_, platform, st, th := approvalTestBot(t)
	client := &recordingUIResponder{}
	request := ApprovalRequest{ID: "dialog-unowned", Method: "input"}
	th.presentApproval(client, "/sessions/approval.jsonl", 20, "", request, 30000)

	if len(platform.prompts) != 0 {
		t.Fatalf("presented %d unowned dialogs, want none", len(platform.prompts))
	}
	if len(client.calls) != 1 || client.calls[0].id != request.ID || client.calls[0].response["cancelled"] != true {
		t.Fatalf("RespondUI calls = %+v, want cancellation", client.calls)
	}
	routeID := approvalRouteID(th.key, "/sessions/approval.jsonl", 20, request.ID)
	if _, found, err := st.PendingUI(context.Background(), routeID); err != nil || found {
		t.Errorf("unowned dialog was persisted: found=%v err=%v", found, err)
	}
}

func TestPresentApprovalWithoutPiTimeoutDoesNotSetLocalExpiry(t *testing.T) {
	_, _, st, th := approvalTestBot(t)
	request := ApprovalRequest{ID: "dialog-no-timeout", Method: "confirm"}
	th.presentApproval(&recordingUIResponder{}, "/sessions/approval.jsonl", 21, "U1", request, 0)
	routeID := approvalRouteID(th.key, "/sessions/approval.jsonl", 21, request.ID)
	pending, found, err := st.PendingUI(context.Background(), routeID)
	if err != nil || !found {
		t.Fatalf("PendingUI = found %v, err %v", found, err)
	}
	if !pending.ExpiresAt.IsZero() {
		t.Fatalf("dialog without a Pi deadline expires locally at %s", pending.ExpiresAt)
	}
}

func TestPresentApprovalCancelsWhenSlackCannotShowIt(t *testing.T) {
	_, platform, st, th := approvalTestBot(t)
	platform.presentErr = errors.New("missing_scope")
	client := &recordingUIResponder{}
	request := ApprovalRequest{ID: "dialog-2", Method: "input"}
	th.presentApproval(client, "/sessions/approval.jsonl", 2, "U1", request, 30000)

	if len(client.calls) != 1 || client.calls[0].id != request.ID || client.calls[0].response["cancelled"] != true {
		t.Fatalf("RespondUI calls = %+v, want a cancellation", client.calls)
	}
	routeID := approvalRouteID(th.key, "/sessions/approval.jsonl", 2, request.ID)
	if _, found, err := st.PendingUI(context.Background(), routeID); err != nil || found {
		t.Errorf("failed dialog remains pending: found=%v err=%v", found, err)
	}
	if statuses := platform.statuses; len(statuses) < 2 || statuses[len(statuses)-1] != StatusBusy {
		t.Errorf("statuses = %v, want busy after cancellation", statuses)
	}
}

func TestApprovalButtonOpensModalWithoutConsumingDialog(t *testing.T) {
	b, platform, st, th := approvalTestBot(t)
	request := ApprovalRequest{ID: "input-1", Method: "editor", Title: "Edit plan", Prefill: "keep this"}
	routeID := approvalRouteID(th.key, "/sessions/approval.jsonl", 3, request.ID)
	b.rememberApproval(routeID, request)
	pending := store.PendingUI{
		RouteID: routeID, RequestID: request.ID, ThreadKey: th.key, RequesterUserID: "U1", SessionPath: "/sessions/approval.jsonl",
		Method: request.Method, MessageTS: "approval-message-ts",
		CreatedAt: time.Now(), ExpiresAt: time.Now().Add(time.Minute),
	}
	if _, err := st.PutPendingUI(context.Background(), pending); err != nil {
		t.Fatalf("PutPendingUI: %v", err)
	}
	handleExpiresAt := time.Now().Add(1500 * time.Millisecond)
	action := Action{
		EventID: "Ev-modal", Channel: "C1", Thread: ptr(approvalTestThread()), UserID: "U1", Workspace: "T1",
		ActionID: ActionUIOpenModal, RouteID: routeID, ApprovalHandle: "trigger-1",
		ApprovalHandleExpiresAt: handleExpiresAt,
	}
	b.HandleAction(context.Background(), action)

	if len(platform.modals) != 1 {
		t.Fatalf("opened %d modals, want one", len(platform.modals))
	}
	modal := platform.modals[0]
	if modal.triggerID != "trigger-1" || modal.dialogID != routeID || modal.request.Prefill != "keep this" {
		t.Errorf("modal request = %+v", modal)
	}
	if !modal.deadline.Equal(handleExpiresAt) {
		t.Errorf("modal context deadline = %s, want trigger deadline %s", modal.deadline, handleExpiresAt)
	}
	if _, found, err := st.PendingUI(context.Background(), routeID); err != nil || !found {
		t.Errorf("opening a form consumed the dialog: found=%v err=%v", found, err)
	}
}

func TestApprovalActionIsRestrictedToTheInitiatingRequester(t *testing.T) {
	b, platform, st, th := approvalTestBot(t)
	b.cfg.Slack.Access.AllowedUsers = []string{"U1", "U2"}
	for i, testCase := range []struct {
		actionID string
		method   string
	}{
		{ActionUIOpenModal, "input"},
		{ActionUISubmit, "input"},
		{ActionUIConfirmYes, "confirm"},
	} {
		routeID := fmt.Sprintf("requester-route-%d", i)
		pending := store.PendingUI{
			RouteID: routeID, RequestID: fmt.Sprintf("dialog-%d", i), ThreadKey: th.key,
			RequesterUserID: "U1", SessionPath: "/sessions/approval.jsonl", Method: testCase.method,
			CreatedAt: time.Now(), ExpiresAt: time.Now().Add(time.Minute),
		}
		if _, err := st.PutPendingUI(context.Background(), pending); err != nil {
			t.Fatalf("PutPendingUI(%s): %v", routeID, err)
		}
		action := Action{
			EventID: fmt.Sprintf("Ev-nonowner-%d", i), Channel: "C1", Thread: ptr(approvalTestThread()),
			UserID: "U2", Workspace: "T1", ActionID: testCase.actionID, RouteID: routeID,
			ApprovalHandle: "untrusted-trigger", Value: "SECRET-MUST-NOT-LEAK",
		}
		b.HandleAction(context.Background(), action)
		if _, found, err := st.PendingUI(context.Background(), routeID); err != nil || !found {
			t.Errorf("non-owner action %s consumed the route: found=%v err=%v", testCase.actionID, found, err)
		}
	}
	if len(platform.modals) != 0 {
		t.Errorf("non-requester opened %d modals", len(platform.modals))
	}
	if len(platform.notices) != 3 {
		t.Fatalf("denial notices = %+v, want one ephemeral notice per unauthorized action", platform.notices)
	}
	for _, notice := range platform.notices {
		if !notice.Ephemeral || notice.Text != "Only the person who started this request can respond." ||
			strings.Contains(notice.Text, "SECRET-MUST-NOT-LEAK") {
			t.Errorf("unauthorized response notice is not safe/ephemeral: %+v", notice)
		}
	}
}

func TestApprovalResponseShapes(t *testing.T) {
	for _, tc := range []struct {
		name   string
		method string
		action Action
		key    string
		want   any
	}{
		{"confirm yes", "confirm", Action{ActionID: ActionUIConfirmYes}, "confirmed", true},
		{"confirm no", "confirm", Action{ActionID: ActionUIConfirmNo}, "confirmed", false},
		{"select", "select", Action{ActionID: ActionUISelect, Value: "option"}, "value", "option"},
		{"input", "input", Action{ActionID: ActionUISubmit, Value: "text"}, "value", "text"},
		{"editor", "editor", Action{ActionID: ActionUISubmit, Value: "edited"}, "value", "edited"},
		{"cancel", "confirm", Action{ActionID: ActionUICancel}, "cancelled", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, _, ok := approvalResponse(tc.method, tc.action)
			if !ok || got[tc.key] != tc.want {
				t.Fatalf("approvalResponse = %#v, valid=%v; want %s=%#v", got, ok, tc.key, tc.want)
			}
		})
	}
	if _, _, ok := approvalResponse("select", Action{ActionID: ActionUIConfirmYes}); ok {
		t.Fatal("a confirm button was accepted for a select request")
	}
}

func TestApprovalSelectButtonsSanitizeOptionValues(t *testing.T) {
	buttons, tooMany := approvalButtons(ApprovalRequest{
		Method: "select", Options: []string{"\x1b[31mChoice\x1b[0m"},
	}, "route-1", 1)
	if tooMany {
		t.Fatal("one valid choice exceeded a one-choice limit")
	}
	if len(buttons) != 2 || buttons[0].Text != "Choice" ||
		buttons[0].Value != `{"route_id":"route-1","value":"Choice"}` {
		t.Fatalf("select buttons = %+v", buttons)
	}
}

func TestPresentApprovalCancelsWhenChoicesExceedSurfaceLimit(t *testing.T) {
	_, platform, st, th := approvalTestBot(t)
	client := &recordingUIResponder{}
	request := ApprovalRequest{ID: "dialog-too-many", Method: "select", Options: make([]string, 1225)}
	for i := range request.Options {
		request.Options[i] = "choice"
	}
	th.presentApproval(client, "/sessions/approval.jsonl", 22, "U1", request, 60000)
	if len(platform.prompts) != 0 {
		t.Fatalf("presented %d partial-choice prompts; want cancellation", len(platform.prompts))
	}
	if len(client.calls) != 1 || client.calls[0].response["cancelled"] != true {
		t.Fatalf("RespondUI calls = %+v, want a cancellation", client.calls)
	}
	routeID := approvalRouteID(th.key, "/sessions/approval.jsonl", 22, request.ID)
	if _, found, err := st.PendingUI(context.Background(), routeID); err != nil || found {
		t.Errorf("overflow route remains pending: found=%v err=%v", found, err)
	}
}

func TestApprovalSelectButtonsRejectOverflowInsteadOfDroppingChoices(t *testing.T) {
	buttons, tooMany := approvalButtons(ApprovalRequest{Method: "select", Options: []string{"one", "two"}}, "route-1", 1)
	if !tooMany || buttons != nil {
		t.Fatalf("approvalButtons = %+v, tooMany=%v; want explicit overflow", buttons, tooMany)
	}
}

func TestApprovalResponseClaimSerializesClicksPerRoute(t *testing.T) {
	b, _, _, _ := approvalTestBot(t)
	if !b.beginUIResponse("route-claim") {
		t.Fatal("first response claim failed")
	}
	if b.beginUIResponse("route-claim") {
		t.Fatal("a concurrent click claimed the route twice")
	}
	b.endUIResponse("route-claim")
	if !b.beginUIResponse("route-claim") {
		t.Fatal("route stayed locked after the first attempt ended")
	}
	b.endUIResponse("route-claim")
}

func TestApprovalDeliveryKeepsRouteUntilPiAcceptsResponse(t *testing.T) {
	b, _, st, th := approvalTestBot(t)
	ctx := context.Background()
	pending := store.PendingUI{
		RouteID: "route-delivery", RequestID: "dialog-delivery", ThreadKey: th.key, RequesterUserID: "U1",
		SessionPath: "/sessions/approval.jsonl", Method: "confirm",
		CreatedAt: time.Now(), ExpiresAt: time.Now().Add(time.Minute),
	}
	if inserted, err := st.PutPendingUI(ctx, pending); err != nil || !inserted {
		t.Fatalf("PutPendingUI = inserted %v, err %v", inserted, err)
	}

	responder := &recordingUIResponder{err: errors.New("temporary gateway failure")}
	sent, err := b.sendApprovalResponse(ctx, pending, responder, map[string]any{"confirmed": true})
	if sent || err == nil {
		t.Fatalf("failed response = sent %v, err %v; want a retryable failure", sent, err)
	}
	if _, found, err := st.PendingUI(ctx, pending.RouteID); err != nil || !found {
		t.Fatalf("failed delivery consumed route: found=%v err=%v", found, err)
	}

	responder.err = nil
	sent, err = b.sendApprovalResponse(ctx, pending, responder, map[string]any{"confirmed": true})
	if !sent || err != nil {
		t.Fatalf("successful response = sent %v, err %v", sent, err)
	}
	if _, found, err := st.PendingUI(ctx, pending.RouteID); err != nil || found {
		t.Fatalf("accepted response left route pending: found=%v err=%v", found, err)
	}
}

func TestCleanApprovalTextDropsTerminalControls(t *testing.T) {
	got := cleanApprovalText("hello\x1b[31m red\x1b[0m\x00\nworld", 30)
	if strings.ContainsAny(got, "\x1b\x00") || got != "hello red\nworld" {
		t.Fatalf("cleanApprovalText = %q", got)
	}
}

func TestApprovalRouteIDIsStableAcrossReplayAndScopedBySession(t *testing.T) {
	first := approvalRouteID("T1:C1:1.1", "/sessions/a.jsonl", 42, "ui-1")
	if replay := approvalRouteID("T1:C1:1.1", "/sessions/a.jsonl", 42, "ui-1"); replay != first {
		t.Fatalf("replayed event route = %q, want %q", replay, first)
	}
	otherSession := approvalRouteID("T1:C2:2.2", "/sessions/b.jsonl", 42, "ui-1")
	if otherSession == first {
		t.Fatal("same session-local gateway ID collided across sessions")
	}
	otherEvent := approvalRouteID("T1:C1:1.1", "/sessions/a.jsonl", 43, "ui-1")
	if otherEvent == first {
		t.Fatal("reused gateway ID collided across event sequence numbers")
	}
}

func TestApprovalTimeoutNeverExpiresEarlierThanPi(t *testing.T) {
	maxDurationMS := int64(time.Duration(1<<63-1) / time.Millisecond)
	maxIntMS := int(^uint(0) >> 1)
	maxIntWant := time.Duration(0)
	if int64(maxIntMS) <= maxDurationMS {
		maxIntWant = time.Duration(maxIntMS) * time.Millisecond
	}
	for _, tc := range []struct {
		ms   int
		want time.Duration
	}{
		{ms: 0, want: 0},
		{ms: -1, want: 0},
		{ms: 100, want: 100 * time.Millisecond},
		{ms: 5000, want: 5 * time.Second},
		{ms: 2 * 60 * 60 * 1000, want: 2 * time.Hour},
		{ms: int(maxDurationMS), want: time.Duration(int64(maxDurationMS) * int64(time.Millisecond))},
		{ms: maxIntMS, want: maxIntWant},
	} {
		if got := approvalTimeout(tc.ms); got != tc.want {
			t.Errorf("approvalTimeout(%d) = %s, want %s", tc.ms, got, tc.want)
		}
	}
}

func TestExpirePendingUIUpdatesTheSlackMessage(t *testing.T) {
	b, platform, st, th := approvalTestBot(t)
	now := time.Now().UTC()
	pending := store.PendingUI{
		RouteID: "expired-route-1", RequestID: "expired-1", ThreadKey: th.key, RequesterUserID: "U1", SessionPath: "/sessions/approval.jsonl",
		Method: "confirm", MessageTS: "expired-message", CreatedAt: now.Add(-time.Minute), ExpiresAt: now.Add(-time.Second),
	}
	if _, err := st.PutPendingUI(context.Background(), pending); err != nil {
		t.Fatalf("PutPendingUI: %v", err)
	}
	b.expirePendingUI(context.Background())
	if _, found, err := st.PendingUI(context.Background(), pending.RouteID); err != nil || found {
		t.Errorf("expired dialog remains: found=%v err=%v", found, err)
	}
	if len(platform.notices) != 1 || platform.notices[0].Update != pending.MessageTS || !strings.Contains(platform.notices[0].Text, "expired") {
		t.Errorf("expiry notices = %+v", platform.notices)
	}
}

func ptr[T any](value T) *T { return &value }
