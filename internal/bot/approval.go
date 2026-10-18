package bot

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/tigersoldier/pi-gateway/gwclient"
	"github.com/tigersoldier/pi-gateway/protocol"

	"github.com/tigersoldier/pi-chat/internal/store"
)

const approvalActionTimeout = 10 * time.Second

type uiResponder interface {
	RespondUI(context.Context, string, map[string]any) error
}

type approvalButtonValue struct {
	RouteID string `json:"route_id"`
	Value   string `json:"value,omitempty"`
}

func (th *thread) approvalContext() (context.Context, context.CancelFunc) {
	parent := th.b.lifetime
	if parent == nil {
		parent = context.Background()
	}
	return context.WithTimeout(parent, approvalActionTimeout)
}

func approvalRouteID(threadKey, sessionPath string, eventSeq uint64, requestID string) string {
	material := threadKey + "\x00" + sessionPath + "\x00" + strconv.FormatUint(eventSeq, 10) + "\x00" + requestID
	sum := sha256.Sum256([]byte(material))
	return hex.EncodeToString(sum[:])
}

func isApprovalAction(actionID string) bool {
	switch actionID {
	case ActionUIConfirmYes, ActionUIConfirmNo, ActionUISelect, ActionUIOpenModal, ActionUISubmit, ActionUICancel:
		return true
	default:
		return false
	}
}

func approvalButtonValueJSON(routeID, value string) string {
	raw, err := json.Marshal(approvalButtonValue{RouteID: routeID, Value: value})
	if err != nil {
		return ""
	}
	return string(raw)
}

func approvalButtons(request ApprovalRequest, routeID string, maxChoices int) ([]Button, bool) {
	value := approvalButtonValueJSON(routeID, "")
	switch request.Method {
	case "confirm":
		return []Button{
			{ActionID: ActionUIConfirmYes, Text: "Yes", Value: value, Style: "primary"},
			{ActionID: ActionUIConfirmNo, Text: "No", Value: value, Style: "danger"},
		}, false
	case "select":
		maxChoices = max(0, maxChoices)
		buttons := make([]Button, 0, min(len(request.Options), maxChoices)+1)
		for _, option := range request.Options {
			optionValue := cleanApprovalValue(option, 2000)
			label := strings.Join(strings.Fields(cleanApprovalText(optionValue, 75)), " ")
			if label == "" {
				continue
			}
			choice := approvalButtonValueJSON(routeID, optionValue)
			if len(choice) > 2000 {
				continue
			}
			if len(buttons) == maxChoices {
				return nil, true
			}
			buttons = append(buttons, Button{ActionID: ActionUISelect, Text: label, Value: choice})
		}
		return append(buttons, Button{ActionID: ActionUICancel, Text: "Cancel", Value: value}), false
	case "input", "editor":
		return []Button{
			{ActionID: ActionUIOpenModal, Text: "Respond", Value: value, Style: "primary"},
			{ActionID: ActionUICancel, Text: "Cancel", Value: value},
		}, false
	default:
		return nil, false
	}
}

// cleanApprovalValue strips terminal controls from opaque select values before
// they reach Slack, while preserving ordinary whitespace used by the option.
func cleanApprovalValue(value string, maxRunes int) string {
	value = stripANSI(value)
	var out strings.Builder
	count := 0
	for _, r := range value {
		if unicode.IsControl(r) {
			continue
		}
		if count == maxRunes {
			break
		}
		out.WriteRune(r)
		count++
	}
	return out.String()
}

// cleanApprovalText strips extension-provided terminal controls before they
// reach Slack. Plain-text blocks prevent markup from becoming a mention or link.
func cleanApprovalText(value string, maxRunes int) string {
	value = stripANSI(value)
	var out strings.Builder
	count := 0
	for _, r := range value {
		if unicode.IsControl(r) && r != '\n' && r != '\t' {
			continue
		}
		if count == maxRunes {
			break
		}
		out.WriteRune(r)
		count++
	}
	return strings.TrimSpace(out.String())
}

// approvalTimeout returns zero when Pi did not provide a deadline or when its
// millisecond value cannot fit in time.Duration. In either case the route must
// remain available rather than expire before Pi stops waiting.
func approvalTimeout(timeoutMS int) time.Duration {
	if timeoutMS <= 0 || int64(timeoutMS) > int64(time.Duration(1<<63-1)/time.Millisecond) {
		return 0
	}
	return time.Duration(timeoutMS) * time.Millisecond
}

// presentApproval persists the routing identity before exposing any action to
// Slack. It runs off the gateway reader so posting a prompt cannot stall event
// delivery for the session.
func (th *thread) presentApproval(client uiResponder, sessionPath string, eventSeq uint64, requesterUserID string, request ApprovalRequest, timeoutMS int) {
	if client == nil || request.ID == "" {
		th.log.Warn("pi sent an incomplete blocking UI request", "method", request.Method)
		return
	}
	if requesterUserID == "" {
		th.log.Warn("pi sent a blocking dialog without a Slack requester; canceling it", "method", request.Method)
		ctx, cancel := th.approvalContext()
		defer cancel()
		if err := client.RespondUI(ctx, request.ID, map[string]any{"cancelled": true}); err != nil {
			th.log.Warn("cannot cancel pi's unowned UI request", "method", request.Method, "error", err)
		}
		return
	}
	routeID := approvalRouteID(th.key, sessionPath, eventSeq, request.ID)
	if !validApprovalMethod(request.Method) {
		th.log.Warn("pi sent an unsupported blocking UI request", "method", request.Method)
		ctx, cancel := th.approvalContext()
		defer cancel()
		if err := client.RespondUI(ctx, request.ID, map[string]any{"cancelled": true}); err != nil {
			th.log.Warn("cannot cancel pi's unsupported UI request", "error", err)
		}
		return
	}
	if th.snapshot().SessionPath != sessionPath || th.sessionChanging.Load() {
		return
	}
	request.Title = cleanApprovalText(request.Title, 1000)
	request.Message = cleanApprovalText(request.Message, 2500)
	request.Placeholder = cleanApprovalText(request.Placeholder, 150)
	request.Prefill = cleanApprovalText(request.Prefill, 3000)

	ctx, cancel := th.approvalContext()
	defer cancel()

	now := time.Now()
	var expiresAt time.Time
	if timeout := approvalTimeout(timeoutMS); timeout > 0 {
		expiresAt = now.Add(timeout)
	}
	pending := store.PendingUI{
		RouteID: routeID, RequestID: request.ID, ThreadKey: th.key, RequesterUserID: requesterUserID,
		SessionPath: sessionPath, Method: request.Method, CreatedAt: now, ExpiresAt: expiresAt,
	}
	inserted, err := th.b.store.PutPendingUI(ctx, pending)
	if err != nil {
		th.log.Error("cannot persist a pending pi dialog", "method", request.Method, "error", err)
		th.cancelApproval(client, routeID, request, "I couldn't save this approval, so I canceled it.")
		return
	}
	if !inserted {
		previous, found, readErr := th.b.store.PendingUI(ctx, routeID)
		if readErr != nil {
			th.log.Error("cannot read a pending pi dialog", "error", readErr)
			return
		}
		if !found {
			return
		}
		if previous.MessageTS != "" {
			th.b.setStatus(ctx, th, StatusWaiting)
			return
		}
	}
	th.b.rememberApproval(routeID, request)
	th.b.setStatus(ctx, th, StatusWaiting)

	reporter, ok := th.b.plat.(ApprovalReporter)
	if !ok {
		th.cancelApproval(client, routeID, request, "This chat surface can't show pi's approval prompt, so I canceled it.")
		return
	}
	buttons, tooManyChoices := approvalButtons(request, routeID, reporter.MaxApprovalChoices())
	if tooManyChoices {
		th.cancelApproval(client, routeID, request, "Pi sent more choices than this chat can display, so I canceled the request. Ask it to narrow the choices and try again.")
		return
	}
	messageTS, err := reporter.PresentApproval(ctx, th.t, request, buttons)
	if err != nil {
		th.log.Warn("cannot show pi's approval prompt", "method", request.Method, "error", err)
		th.cancelApproval(client, routeID, request, "I couldn't show this approval prompt, so I canceled it.")
		return
	}
	if messageTS == "" {
		th.log.Warn("the approval surface returned no message timestamp", "method", request.Method)
		th.cancelApproval(client, routeID, request, "I couldn't track this approval prompt, so I canceled it.")
		return
	}
	if err := th.b.store.SetPendingUIMessage(ctx, routeID, messageTS); err != nil && !errors.Is(err, store.ErrNoPendingUI) {
		th.log.Warn("cannot save the approval message timestamp", "error", err)
		th.cancelApproval(client, routeID, request, "I couldn't track this approval prompt, so I canceled it.")
		pending.MessageTS = messageTS
		cleanupCtx, cleanupCancel := th.approvalContext()
		th.b.finishApprovalMessage(cleanupCtx, th.t, pending, "", "This approval could not be tracked and was canceled.")
		cleanupCancel()
	}
}

func validApprovalMethod(method string) bool {
	switch method {
	case "select", "confirm", "input", "editor":
		return true
	default:
		return false
	}
}

func (th *thread) cancelApproval(client uiResponder, routeID string, request ApprovalRequest, notice string) {
	ctx, cancel := th.approvalContext()
	defer cancel()
	if err := client.RespondUI(ctx, request.ID, map[string]any{"cancelled": true}); err != nil {
		th.log.Warn("cannot cancel pi's unavailable approval prompt", "method", request.Method, "error", err)
	}
	if _, _, err := th.b.store.TakePendingUI(ctx, routeID); err != nil {
		th.log.Warn("cannot remove a canceled approval route", "error", err)
	}
	th.b.forgetApproval(routeID)
	th.b.setStatus(ctx, th, StatusBusy)
	th.b.reply(ctx, Notice{Thread: &th.t, Text: notice})
}

func (b *Bot) rememberApproval(routeID string, request ApprovalRequest) {
	b.uiMu.Lock()
	defer b.uiMu.Unlock()
	if b.uiRequests == nil {
		b.uiRequests = make(map[string]ApprovalRequest)
	}
	b.uiRequests[routeID] = request
}

func (b *Bot) forgetApproval(routeID string) {
	b.uiMu.Lock()
	defer b.uiMu.Unlock()
	delete(b.uiRequests, routeID)
}

func (b *Bot) approvalRequest(pending store.PendingUI) ApprovalRequest {
	b.uiMu.Lock()
	defer b.uiMu.Unlock()
	request, ok := b.uiRequests[pending.RouteID]
	if !ok {
		return ApprovalRequest{ID: pending.RequestID, Method: pending.Method}
	}
	return request
}

func (b *Bot) beginUIResponse(routeID string) bool {
	b.uiMu.Lock()
	defer b.uiMu.Unlock()
	if b.uiResponding == nil {
		b.uiResponding = make(map[string]struct{})
	}
	if _, exists := b.uiResponding[routeID]; exists {
		return false
	}
	b.uiResponding[routeID] = struct{}{}
	return true
}

func (b *Bot) endUIResponse(routeID string) {
	b.uiMu.Lock()
	delete(b.uiResponding, routeID)
	b.uiMu.Unlock()
}

// sendApprovalResponse leaves the route available when Pi did not accept the
// answer. A successful remote response is followed by durable route cleanup;
// after a crash in between, a retry receives ui_stale and retires the route.
func (b *Bot) sendApprovalResponse(ctx context.Context, pending store.PendingUI, responder uiResponder, response map[string]any) (bool, error) {
	if err := responder.RespondUI(ctx, pending.RequestID, response); err != nil {
		return false, err
	}
	cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), storeTimeout)
	defer cancel()
	retired, found, err := b.store.TakePendingUI(cleanupCtx, pending.RouteID)
	if err != nil {
		return true, fmt.Errorf("store: retire answered approval route %s: %w", pending.RouteID, err)
	}
	if found && (retired.RequestID != pending.RequestID || retired.SessionPath != pending.SessionPath || retired.RequesterUserID != pending.RequesterUserID) {
		return true, fmt.Errorf("store: answered approval route %s changed identity", pending.RouteID)
	}
	b.forgetApproval(pending.RouteID)
	return true, nil
}

func (b *Bot) retireApproval(ctx context.Context, thread Thread, pending store.PendingUI, fallbackTS, notice string) {
	consumed, found, err := b.store.TakePendingUI(ctx, pending.RouteID)
	if err != nil {
		b.log.Warn("cannot retire an inactive approval", "error", err)
		return
	}
	if !found {
		return
	}
	b.forgetApproval(consumed.RouteID)
	b.finishApprovalMessage(ctx, thread, consumed, fallbackTS, notice)
}

// cancelUnownedApproval retires a pre-migration route only after Pi accepts a
// cancellation (or reports it stale). On transient gateway errors it leaves the
// route unanswerable but retryable by the maintenance sweep.
func (b *Bot) cancelUnownedApproval(ctx context.Context, thread Thread, pending store.PendingUI, fallbackTS string) {
	if pending.RequesterUserID != "" || !b.beginUIResponse(pending.RouteID) {
		return
	}
	defer b.endUIResponse(pending.RouteID)

	cancelCtx, cancel := context.WithTimeout(ctx, approvalActionTimeout)
	defer cancel()
	th, err := b.threadFor(thread)
	if err != nil {
		b.log.Warn("cannot attach the thread to cancel an unowned approval", "route_id", pending.RouteID, "error", err)
		return
	}
	if th.snapshot().SessionPath != pending.SessionPath {
		b.retireApproval(cancelCtx, thread, pending, fallbackTS, "This older approval could not be verified and was canceled. Ask the original requester to retry.")
		return
	}
	client, err := th.bind(cancelCtx, "", false)
	if err != nil {
		if errors.Is(err, errSessionGone) || errors.Is(err, errNoSession) {
			b.retireApproval(cancelCtx, thread, pending, fallbackTS, "This older approval is no longer active. Ask the original requester to retry.")
			return
		}
		b.log.Warn("cannot reconnect to cancel an unowned approval", "route_id", pending.RouteID, "error", err)
		return
	}
	if session := client.Session(); session == nil || session.Path != pending.SessionPath {
		b.retireApproval(cancelCtx, thread, pending, fallbackTS, "This older approval could not be verified and was canceled. Ask the original requester to retry.")
		return
	}

	sent, err := b.sendApprovalResponse(cancelCtx, pending, client, map[string]any{"cancelled": true})
	if err != nil && !sent {
		var responseErr *gwclient.ResponseError
		if errors.As(err, &responseErr) && responseErr.Code == protocol.CodeUIStale {
			b.retireApproval(cancelCtx, thread, pending, fallbackTS, "This older approval is no longer active. Ask the original requester to retry.")
			return
		}
		b.log.Warn("cannot cancel an unowned approval yet", "route_id", pending.RouteID, "error", err)
		return
	}
	if err != nil {
		b.log.Error("Pi canceled an unowned approval but its route could not be retired", "route_id", pending.RouteID, "error", err)
	}
	b.finishApprovalMessage(cancelCtx, thread, pending, fallbackTS,
		"This older approval was canceled because its requester could not be verified. Ask the original requester to try again.")
}

func (b *Bot) handleApprovalAction(ctx context.Context, action Action) {
	if action.RouteID == "" {
		b.log.Warn("ignoring an approval action without a route id", "action", action.ActionID)
		return
	}
	pending, found, err := b.store.PendingUI(ctx, action.RouteID)
	if err != nil {
		b.log.Error("cannot read a pending approval", "error", err)
		return
	}
	if !found {
		b.log.Debug("ignoring an action for a dialog that is no longer pending", "action", action.ActionID)
		return
	}
	row, found, err := b.store.Thread(ctx, pending.ThreadKey)
	if err != nil {
		b.log.Error("cannot read the thread for a pending approval", "error", err)
		return
	}
	if !found {
		b.log.Warn("the thread for a pending approval no longer exists", "route_id", pending.RouteID)
		return
	}
	thread := Thread{Workspace: row.WorkspaceID, Channel: row.ChannelID, ThreadTS: row.ThreadTS, Surface: row.Surface}
	if action.Channel != "" && action.Channel != thread.Channel ||
		action.Workspace != "" && action.Workspace != thread.Workspace ||
		action.Thread != nil && action.Thread.Key() != thread.Key() {
		b.log.Warn("ignoring an approval action from a different conversation", "route_id", pending.RouteID)
		return
	}
	if !b.allowed(action.UserID, thread.Channel) {
		b.refuse(ctx, action.UserID, thread.Channel, &thread, action.ReplyTo)
		return
	}
	if pending.RequesterUserID == "" {
		b.cancelUnownedApproval(ctx, thread, pending, action.MessageTS)
		b.reply(ctx, Notice{Thread: &thread, UserID: action.UserID, Ephemeral: true,
			Text: "This older approval has no verified requester and cannot be answered. Ask the original requester to try again."})
		return
	}
	if action.UserID != pending.RequesterUserID {
		b.reply(ctx, Notice{Thread: &thread, UserID: action.UserID, Ephemeral: true,
			Text: "Only the person who started this request can respond."})
		return
	}
	if !pending.ExpiresAt.IsZero() && !time.Now().Before(pending.ExpiresAt) {
		consumed, ok, takeErr := b.store.TakePendingUI(ctx, pending.RouteID)
		if takeErr != nil {
			b.log.Warn("cannot retire an expired approval", "error", takeErr)
			return
		}
		if ok {
			b.forgetApproval(consumed.RouteID)
			b.finishApprovalMessage(ctx, thread, consumed, action.MessageTS, "This approval expired before it was answered.")
		}
		return
	}
	if row.State == store.StateDeleted || row.SessionPath != pending.SessionPath {
		consumed, ok, takeErr := b.store.TakePendingUI(ctx, pending.RouteID)
		if takeErr != nil {
			b.log.Warn("cannot retire a stale approval", "error", takeErr)
			return
		}
		if ok {
			b.forgetApproval(consumed.RouteID)
			b.finishApprovalMessage(ctx, thread, consumed, action.MessageTS, "This request belongs to an older pi session and is no longer active.")
		}
		return
	}

	if action.ActionID == ActionUIOpenModal {
		reporter, ok := b.plat.(ApprovalReporter)
		if !ok {
			b.reply(ctx, Notice{Thread: &thread, UserID: action.UserID, Ephemeral: true,
				Text: "This chat surface can't open pi's response form."})
			return
		}
		if pending.Method != "input" && pending.Method != "editor" {
			b.log.Warn("ignoring a modal request for a non-text pi dialog", "method", pending.Method)
			return
		}
		modalDeadline := time.Now().Add(2 * time.Second)
		if !action.ApprovalHandleExpiresAt.IsZero() && action.ApprovalHandleExpiresAt.Before(modalDeadline) {
			modalDeadline = action.ApprovalHandleExpiresAt
		}
		modalCtx, cancel := context.WithDeadline(ctx, modalDeadline)
		err := reporter.OpenApprovalModal(modalCtx, thread, action.ApprovalHandle, pending.RouteID, b.approvalRequest(pending))
		cancel()
		if err != nil {
			b.log.Warn("cannot open pi's response form", "method", pending.Method, "error", err)
			b.reply(ctx, Notice{Thread: &thread, UserID: action.UserID, Ephemeral: true,
				Text: "I couldn't open the response form. Try the Respond button again."})
		}
		return
	}

	response, confirmation, ok := approvalResponse(pending.Method, action)
	if !ok {
		b.log.Warn("ignoring an approval action that does not match its dialog", "action", action.ActionID, "method", pending.Method)
		return
	}
	if !b.beginUIResponse(pending.RouteID) {
		b.reply(ctx, Notice{Thread: &thread, UserID: action.UserID, Ephemeral: true,
			Text: "This approval response is already being processed."})
		return
	}
	defer b.endUIResponse(pending.RouteID)

	th, err := b.threadFor(thread)
	if err != nil {
		b.log.Error("cannot attach the thread for a pending approval", "error", err)
		b.reply(ctx, Notice{Thread: &thread, UserID: action.UserID, Ephemeral: true,
			Text: "I couldn't reconnect to the session. If this approval is still active, try again."})
		return
	}
	if th.snapshot().SessionPath != pending.SessionPath {
		b.retireApproval(ctx, thread, pending, action.MessageTS, "This request belongs to an older pi session and is no longer active.")
		return
	}
	client, err := th.bind(ctx, "", false)
	if err == nil {
		session := client.Session()
		if session == nil || session.Path != pending.SessionPath {
			b.retireApproval(ctx, thread, pending, action.MessageTS, "This request belongs to an older pi session and is no longer active.")
			return
		}
	}
	if err != nil {
		b.log.Warn("cannot reconnect to the session waiting for approval", "error", err)
		b.reply(ctx, Notice{Thread: &thread, UserID: action.UserID, Ephemeral: true,
			Text: "I couldn't reconnect to the session. If this approval is still active, try again."})
		return
	}
	responseCtx, cancel := context.WithTimeout(ctx, approvalActionTimeout)
	defer cancel()
	sent, err := b.sendApprovalResponse(responseCtx, pending, client, response)
	if err != nil {
		if !sent {
			var responseErr *gwclient.ResponseError
			if errors.As(err, &responseErr) && responseErr.Code == protocol.CodeUIStale {
				b.retireApproval(ctx, thread, pending, action.MessageTS, "Pi no longer needs an answer to this request.")
				return
			}
			_, stillPending, readErr := b.store.PendingUI(ctx, pending.RouteID)
			if readErr != nil {
				b.log.Warn("cannot verify a failed approval response route", "error", readErr)
			} else if !stillPending {
				b.forgetApproval(pending.RouteID)
				b.finishApprovalMessage(ctx, thread, pending, action.MessageTS, "Pi no longer needs an answer to this request.")
				return
			}
			b.log.Warn("cannot send pi's approval response; leaving it retryable", "method", pending.Method, "error", err)
			b.reply(ctx, Notice{Thread: &thread, UserID: action.UserID, Ephemeral: true,
				Text: "I couldn't send that answer. If this approval is still active, try again."})
			return
		}
		b.log.Error("pi accepted an approval response but its route could not be retired", "route_id", pending.RouteID, "error", err)
	}
	b.setStatus(responseCtx, th, StatusBusy)
	text := "Response sent to pi."
	if confirmation != "" {
		text = confirmation
	}
	b.finishApprovalMessage(responseCtx, thread, pending, action.MessageTS, text)
}

func approvalResponse(method string, action Action) (map[string]any, string, bool) {
	switch action.ActionID {
	case ActionUIConfirmYes:
		if method == "confirm" {
			return map[string]any{"confirmed": true}, "Approved.", true
		}
	case ActionUIConfirmNo:
		if method == "confirm" {
			return map[string]any{"confirmed": false}, "Declined.", true
		}
	case ActionUISelect:
		if method == "select" {
			return map[string]any{"value": action.Value}, "Choice sent to pi.", true
		}
	case ActionUISubmit:
		if method == "input" || method == "editor" {
			return map[string]any{"value": action.Value}, "Response sent to pi.", true
		}
	case ActionUICancel:
		return map[string]any{"cancelled": true}, "Canceled.", true
	}
	return nil, "", false
}

func (b *Bot) finishApprovalMessage(ctx context.Context, thread Thread, pending store.PendingUI, fallbackTS, text string) {
	messageTS := pending.MessageTS
	if messageTS == "" {
		messageTS = fallbackTS
	}
	if messageTS == "" {
		return
	}
	b.reply(ctx, Notice{Thread: &thread, Update: messageTS, Text: text})
}

// expirePendingUI marks dialogs whose pi-side timeout has elapsed. The request
// itself times out in pi; this removes stale buttons and modal metadata after a
// restart as well as during normal operation.
func (b *Bot) expirePendingUI(ctx context.Context) {
	if b.store == nil {
		return
	}
	unowned, err := b.store.UnownedPendingUI(ctx)
	if err != nil {
		b.log.Warn("cannot list pending dialogs without a requester", "error", err)
	} else {
		for _, pending := range unowned {
			row, found, err := b.store.Thread(ctx, pending.ThreadKey)
			if err != nil {
				b.log.Warn("cannot read the thread of an unowned approval", "route_id", pending.RouteID, "error", err)
				continue
			}
			if !found {
				b.log.Warn("the thread of an unowned approval no longer exists", "route_id", pending.RouteID)
				b.forgetApproval(pending.RouteID)
				if _, _, takeErr := b.store.TakePendingUI(ctx, pending.RouteID); takeErr != nil {
					b.log.Warn("cannot retire an unowned approval without a thread", "route_id", pending.RouteID, "error", takeErr)
				}
				continue
			}
			thread := Thread{Workspace: row.WorkspaceID, Channel: row.ChannelID, ThreadTS: row.ThreadTS, Surface: row.Surface}
			b.cancelUnownedApproval(ctx, thread, pending, "")
		}
	}

	expired, err := b.store.TakeExpiredPendingUI(ctx, time.Now())
	if err != nil {
		b.log.Warn("cannot expire pending pi dialogs", "error", err)
		return
	}
	for _, pending := range expired {
		b.forgetApproval(pending.RouteID)
		row, found, err := b.store.Thread(ctx, pending.ThreadKey)
		if err != nil || !found {
			if err != nil {
				b.log.Warn("cannot read the thread of an expired approval", "error", err)
			}
			continue
		}
		thread := Thread{Workspace: row.WorkspaceID, Channel: row.ChannelID, ThreadTS: row.ThreadTS, Surface: row.Surface}
		b.finishApprovalMessage(ctx, thread, pending, "", "This approval expired before it was answered.")
	}
}
