// Package slack is pi-chat's Slack adapter: Socket Mode ingress, the slice of
// the Web API needed to answer, message parsing, and rendering into threads
// (DESIGN.md §5, §12).
package slack

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// APIBase is Slack's Web API root. It is a variable so tests can point the
// client at a local server.
var APIBase = "https://slack.com/api/"

// API is the slice of Slack's Web API that pi-chat uses. Every method needs
// the bot token: the app-level token only opens the socket.
type API struct {
	token string
	http  *http.Client
	log   *slog.Logger
}

// NewAPI builds a client for the given bot token.
func NewAPI(token string, log *slog.Logger) *API {
	return &API{
		token: token,
		http:  &http.Client{Timeout: 30 * time.Second},
		log:   log,
	}
}

// Error is a Slack API error response, such as `missing_scope`.
type Error struct {
	Method string
	Code   string
}

func (e *Error) Error() string { return "slack " + e.Method + ": " + e.Code }

// IsCode reports whether err is a Slack error with this code, so callers can
// react to specific ones (`invalid_auth`, `not_in_channel`, …).
func IsCode(err error, code string) bool {
	var se *Error
	return errors.As(err, &se) && se.Code == code
}

// call posts one Web API method and decodes the response into out. A nil out
// discards the response body beyond its status.
//
// Arguments travel form-encoded rather than in a JSON body, and that is not a
// style choice: Slack parses a JSON body for some methods and ignores it for
// others, where the arguments then look absent rather than malformed.
// conversations.replies answered `invalid_arguments` and users.info answered
// `user_not_found` — for the bot's own user ID — for as long as this sent JSON.
// Every method takes a form body; only some take JSON.
func (a *API) call(ctx context.Context, method string, params map[string]any, out any) error {
	// The request body is kept as bytes rather than as a reader: an http.Request
	// consumes its body, so a retry built on the same reader would send no
	// parameters at all — which is exactly how the rate-limit retry used to fail.
	raw, err := encodeParams(params)
	if err != nil {
		return fmt.Errorf("slack %s: encode request: %w", method, err)
	}

	for attempt := 0; ; attempt++ {
		var body io.Reader = strings.NewReader("")
		if raw != nil {
			body = bytes.NewReader(raw)
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, APIBase+method, body)
		if err != nil {
			return fmt.Errorf("slack %s: %w", method, err)
		}
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.Header.Set("Authorization", "Bearer "+a.token)

		resp, err := a.http.Do(req)
		if err != nil {
			return fmt.Errorf("slack %s: %w", method, err)
		}
		respBody, readErr := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		resp.Body.Close()
		if readErr != nil {
			return fmt.Errorf("slack %s: read response: %w", method, readErr)
		}

		// A 429 is worth one bounded wait: streaming appends are rate limited
		// and a dropped append is a visibly truncated answer.
		if resp.StatusCode == http.StatusTooManyRequests && attempt < 2 {
			wait := retryDelay(resp.Header.Get("Retry-After"))
			a.log.Warn("Slack rate limited us; retrying", "method", method, "wait", wait)
			select {
			case <-time.After(wait):
				continue
			case <-ctx.Done():
				return ctx.Err()
			}
		}

		var status struct {
			OK    bool   `json:"ok"`
			Error string `json:"error"`
		}
		if err := json.Unmarshal(respBody, &status); err != nil {
			return fmt.Errorf("slack %s: HTTP %d: %w", method, resp.StatusCode, err)
		}
		if !status.OK {
			if status.Error == "" {
				status.Error = "http_" + strconv.Itoa(resp.StatusCode)
			}
			return &Error{Method: method, Code: status.Error}
		}
		if out == nil {
			return nil
		}
		if err := json.Unmarshal(respBody, out); err != nil {
			return fmt.Errorf("slack %s: decode response: %w", method, err)
		}
		return nil
	}
}

// encodeParams renders arguments as the form body of a Web API request. No
// parameters is no body at all, which is what methods like auth.test receive.
func encodeParams(params map[string]any) ([]byte, error) {
	if len(params) == 0 {
		return nil, nil
	}
	form := url.Values{}
	for key, value := range params {
		text, err := paramText(value)
		if err != nil {
			return nil, fmt.Errorf("parameter %s: %w", key, err)
		}
		form.Set(key, text)
	}
	return []byte(form.Encode()), nil
}

// paramText renders one argument the way a form body carries it: a scalar as
// its text — an int would otherwise be the JSON number 200, which Slack's
// argument parser is entitled to read as absent — and anything structured, such
// as a message's blocks or a suggestion's prompts, as the JSON string Slack
// documents for a form body.
func paramText(value any) (string, error) {
	switch v := value.(type) {
	case nil:
		return "", nil
	case string:
		return v, nil
	case bool:
		return strconv.FormatBool(v), nil
	case int:
		return strconv.Itoa(v), nil
	case int64:
		return strconv.FormatInt(v, 10), nil
	case float64:
		return strconv.FormatFloat(v, 'f', -1, 64), nil
	default:
		encoded, err := json.Marshal(v)
		if err != nil {
			return "", err
		}
		return string(encoded), nil
	}
}

// retryDelay reads a Retry-After header, clamped to something a user waiting
// for an answer will tolerate.
func retryDelay(header string) time.Duration {
	seconds, err := strconv.Atoi(strings.TrimSpace(header))
	switch {
	case err != nil, seconds <= 0:
		return time.Second
	case seconds > 5:
		return 5 * time.Second
	default:
		return time.Duration(seconds) * time.Second
	}
}

// AuthInfo is the useful part of auth.test: it identifies the install and the
// bot user whose mention has to be stripped from messages.
type AuthInfo struct {
	URL    string `json:"url"`
	Team   string `json:"team"`
	User   string `json:"user"`
	UserID string `json:"user_id"`
	TeamID string `json:"team_id"`
	BotID  string `json:"bot_id"`
}

// AuthTest checks the bot token and reports the install's identity.
func (a *API) AuthTest(ctx context.Context) (AuthInfo, error) {
	var info AuthInfo
	err := a.call(ctx, "auth.test", nil, &info)
	return info, err
}

// ConnectionsOpen asks for a Socket Mode WebSocket URL. The app-level token,
// not the bot token, authorizes this call.
func (a *API) ConnectionsOpen(ctx context.Context, appToken string) (string, error) {
	var out struct {
		URL string `json:"url"`
	}
	if err := a.callWith(ctx, appToken, "apps.connections.open", nil, &out); err != nil {
		return "", err
	}
	if out.URL == "" {
		return "", errors.New("slack apps.connections.open: response carried no url")
	}
	return out.URL, nil
}

// callWith is call with an explicit token, for the one method that needs the
// app-level token instead of the bot token.
func (a *API) callWith(ctx context.Context, token, method string, params map[string]any, out any) error {
	clone := *a
	clone.token = token
	return clone.call(ctx, method, params, out)
}

// PostMessage posts text into a thread and returns the message's timestamp.
// An empty threadTS posts at the top level.
func (a *API) PostMessage(ctx context.Context, channel, threadTS, text string) (string, error) {
	return a.PostBlocks(ctx, channel, threadTS, text, nil)
}

// PostBlocks posts text with optional Block Kit blocks and returns the
// message's timestamp.
func (a *API) PostBlocks(ctx context.Context, channel, threadTS, text string, blocks []block) (string, error) {
	params := map[string]any{"channel": channel, "text": text}
	if threadTS != "" {
		params["thread_ts"] = threadTS
	}
	if len(blocks) > 0 {
		params["blocks"] = blocks
	}
	var out struct {
		TS string `json:"ts"`
	}
	if err := a.call(ctx, "chat.postMessage", params, &out); err != nil {
		return "", err
	}
	return out.TS, nil
}

// PostEphemeral posts text only the named user can see: refusals, hints and
// command answers should not appear to everyone else in the channel. An empty
// threadTS posts at the channel root.
func (a *API) PostEphemeral(ctx context.Context, channel, user, threadTS, text string, blocks []block) (string, error) {
	params := map[string]any{"channel": channel, "user": user, "text": text}
	if threadTS != "" {
		params["thread_ts"] = threadTS
	}
	if len(blocks) > 0 {
		params["blocks"] = blocks
	}
	var out struct {
		TS string `json:"ts"`
	}
	if err := a.call(ctx, "chat.postEphemeral", params, &out); err != nil {
		return "", err
	}
	return out.TS, nil
}

// UpdateMessage replaces a posted message's text.
func (a *API) UpdateMessage(ctx context.Context, channel, ts, text string) error {
	return a.UpdateBlocks(ctx, channel, ts, text, nil)
}

// UpdateBlocks replaces a posted message's text and blocks. Passing no blocks
// clears them, which is how a picker stops offering choices it has spent.
func (a *API) UpdateBlocks(ctx context.Context, channel, ts, text string, blocks []block) error {
	params := map[string]any{"channel": channel, "ts": ts, "text": text, "blocks": blocks}
	if blocks == nil {
		// Slack treats an absent blocks field as "keep the existing blocks",
		// and an empty array as "remove them".",
		params["blocks"] = []block{}
	}
	return a.call(ctx, "chat.update", params, nil)
}

// Respond posts an answer to an interaction's response_url. It is how a slash
// command or a button answers without a second API scope, and with replace set
// it replaces the message the interaction came from (which is the only way to
// update an ephemeral message).
//
// response_url is pre-signed and carries its own authorization, so this call
// sends no token.
func (a *API) Respond(ctx context.Context, responseURL, text string, blocks []block, replace bool) error {
	if responseURL == "" {
		return errors.New("slack: no response_url to answer")
	}
	params := map[string]any{"text": text, "response_type": "ephemeral"}
	if len(blocks) > 0 {
		params["blocks"] = blocks
	}
	if replace {
		params["replace_original"] = true
	}
	raw, err := json.Marshal(params)
	if err != nil {
		return fmt.Errorf("slack response_url: encode request: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, responseURL, bytes.NewReader(raw))
	if err != nil {
		return fmt.Errorf("slack response_url: %w", err)
	}
	req.Header.Set("Content-Type", "application/json; charset=utf-8")

	resp, err := a.http.Do(req)
	if err != nil {
		return fmt.Errorf("slack response_url: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 4096))
	if err != nil {
		return fmt.Errorf("slack response_url: read response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("slack response_url: HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	// Slack documents the answer as the bare text "ok", and a failure comes
	// back as the usual JSON envelope. Both shapes are accepted — guessing wrong
	// would turn every answer into an error — but a failure is never read as a
	// success, which a substring match would do for `{"ok":false,…}`.
	answer := strings.TrimSpace(string(body))
	if answer == "ok" {
		return nil
	}
	var status struct {
		OK    bool   `json:"ok"`
		Error string `json:"error"`
	}
	if json.Unmarshal([]byte(answer), &status) == nil {
		switch {
		case status.OK:
			return nil
		case status.Error != "":
			return &Error{Method: "response_url", Code: status.Error}
		}
	}
	return fmt.Errorf("slack response_url: unexpected answer %q", answer)
}

// Reply is one message `conversations.replies` returned.
type Reply struct {
	TS      string `json:"ts"`
	User    string `json:"user"`
	BotID   string `json:"bot_id"`
	Subtype string `json:"subtype"`
	Text    string `json:"text"`
}

// Bounds on reading a thread back. The page size is Slack's maximum, and the
// fetch limit is a sanity cap: a thread longer than this is a conversation the
// prompt cannot carry anyway, and the newest messages are the ones that matter.
const (
	replyPageSize   = 200
	replyFetchLimit = 600
)

// Replies reads a thread's conversation in order, following Slack's cursor
// until the thread ends or replyFetchLimit messages have arrived.
//
// `oldest` is the exclusive lower bound in intent — it is the message a previous
// prompt already carried — but Slack treats it as the start of a range, so the
// caller filters the boundary rather than trusting it.
func (a *API) Replies(ctx context.Context, channel, threadTS, oldest string) ([]Reply, error) {
	var out []Reply
	cursor := ""
	for {
		params := map[string]any{"channel": channel, "ts": threadTS, "limit": replyPageSize}
		if oldest != "" {
			params["oldest"] = oldest
		}
		if cursor != "" {
			params["cursor"] = cursor
		}
		var page struct {
			Messages []Reply `json:"messages"`
			HasMore  bool    `json:"has_more"`
			Metadata struct {
				NextCursor string `json:"next_cursor"`
			} `json:"response_metadata"`
		}
		if err := a.call(ctx, "conversations.replies", params, &page); err != nil {
			return nil, err
		}
		out = append(out, page.Messages...)
		if !page.HasMore || page.Metadata.NextCursor == "" || len(out) >= replyFetchLimit {
			return out, nil
		}
		cursor = page.Metadata.NextCursor
	}
}

// UserInfo resolves a user ID to the name to show beside their messages in a
// transcript. It needs `users:read`; an install without the scope gets an error
// here and bare IDs in the transcript, which is the deliberate fallback.
func (a *API) UserInfo(ctx context.Context, userID string) (string, error) {
	var out struct {
		User struct {
			Name     string `json:"name"`
			RealName string `json:"real_name"`
			Profile  struct {
				DisplayName string `json:"display_name"`
				RealName    string `json:"real_name"`
			} `json:"profile"`
		} `json:"user"`
	}
	if err := a.call(ctx, "users.info", map[string]any{"user": userID}, &out); err != nil {
		return "", err
	}
	// Slack's own preference order: what the person chose to be called, then
	// their account name.
	for _, name := range []string{
		out.User.Profile.DisplayName,
		out.User.Profile.RealName,
		out.User.RealName,
		out.User.Name,
	} {
		if name = strings.TrimSpace(name); name != "" {
			return name, nil
		}
	}
	return "", nil
}

// suggestion is one suggested prompt, in the shape Slack takes.
type suggestion struct {
	Title   string `json:"title"`
	Message string `json:"message"`
}

// SetSuggestedPrompts offers prompts at the top of the app's Messages tab in
// Slack's agent messaging experience.
//
// `thread_ts` is deliberately absent, and must stay absent: Slack documents that
// in an agent app including it makes the call fail **silently** — a failure this
// would return no error for — and the agent surface has moved suggestions out of
// threads and into the Messages tab anyway, so there is no thread to name.
func (a *API) SetSuggestedPrompts(ctx context.Context, channelID, title string, prompts []suggestion) error {
	params := map[string]any{"channel_id": channelID, "prompts": prompts}
	if title != "" {
		params["title"] = title
	}
	return a.call(ctx, "assistant.threads.setSuggestedPrompts", params, nil)
}

// SetAgentStatus sets the lifecycle status of the agent session for a thread
// (Slack's agent messaging experience). Slack creates the session if it does
// not exist yet, which is also what makes writing this the thing that opens the
// thread when a user replies.
//
// It needs `chat:write` alone — not the legacy `assistant:write` that the older
// `assistant.threads.setStatus` required. Where the workspace has no agent
// feature, it answers `feature_disabled`.
func (a *API) SetAgentStatus(ctx context.Context, channel, threadTS, status string) error {
	params := map[string]any{"channel_id": channel, "status": status}
	if threadTS != "" {
		params["thread_ts"] = threadTS
	}
	return a.call(ctx, "agents.sessions.setStatus", params, nil)
}

// StartStream opens a streaming message and returns its timestamp. Slack
// requires the recipient for streams that are not thread replies, so pi-chat
// passes whoever asked.
func (a *API) StartStream(ctx context.Context, channel, threadTS, recipientUser, recipientTeam string) (string, error) {
	params := map[string]any{"channel": channel}
	if threadTS != "" {
		params["thread_ts"] = threadTS
	}
	if recipientUser != "" {
		params["recipient_user_id"] = recipientUser
	}
	if recipientTeam != "" {
		params["recipient_team_id"] = recipientTeam
	}
	var out struct {
		TS string `json:"ts"`
	}
	if err := a.call(ctx, "chat.startStream", params, &out); err != nil {
		return "", err
	}
	return out.TS, nil
}

// AppendStream adds the next piece of a streaming message.
func (a *API) AppendStream(ctx context.Context, channel, ts, text string) error {
	return a.call(ctx, "chat.appendStream", map[string]any{
		"channel":       channel,
		"ts":            ts,
		"markdown_text": text,
	}, nil)
}

// StopStream finalizes a streaming message. The text is not repeated here: it
// was already appended, and whether stopStream's own markdown_text replaces or
// appends the streamed content is not documented, so relying on it could
// duplicate the answer.
func (a *API) StopStream(ctx context.Context, channel, ts string) error {
	return a.call(ctx, "chat.stopStream", map[string]any{
		"channel": channel,
		"ts":      ts,
	}, nil)
}
