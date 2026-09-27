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
	contentType := "application/json; charset=utf-8"
	var body io.Reader = strings.NewReader("")
	if params != nil {
		raw, err := json.Marshal(params)
		if err != nil {
			return fmt.Errorf("slack %s: encode request: %w", method, err)
		}
		body = bytes.NewReader(raw)
	} else {
		// Methods that take no arguments are documented as form requests; an
		// empty form body is what the reference curl invocations send.
		contentType = "application/x-www-form-urlencoded"
	}

	for attempt := 0; ; attempt++ {
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
		raw, readErr := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
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
		if err := json.Unmarshal(raw, &status); err != nil {
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
		if err := json.Unmarshal(raw, out); err != nil {
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
	params := map[string]any{"channel": channel, "text": text}
	if threadTS != "" {
		params["thread_ts"] = threadTS
	}
	var out struct {
		TS string `json:"ts"`
	}
	if err := a.call(ctx, "chat.postMessage", params, &out); err != nil {
		return "", err
	}
	return out.TS, nil
}

// UpdateMessage replaces a posted message's text.
func (a *API) UpdateMessage(ctx context.Context, channel, ts, text string) error {
	return a.call(ctx, "chat.update", map[string]any{
		"channel": channel,
		"ts":      ts,
		"text":    text,
	}, nil)
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
