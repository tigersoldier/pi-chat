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
func (a *API) call(ctx context.Context, method string, params map[string]any, out any) error {
	// The request body is kept as bytes rather than as a reader: an http.Request
	// consumes its body, so a retry built on the same reader would send no
	// parameters at all — which is exactly how the rate-limit retry used to fail.
	contentType := "application/x-www-form-urlencoded"
	var raw []byte
	if params != nil {
		encoded, err := json.Marshal(params)
		if err != nil {
			return fmt.Errorf("slack %s: encode request: %w", method, err)
		}
		raw, contentType = encoded, "application/json; charset=utf-8"
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
		req.Header.Set("Content-Type", contentType)
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
