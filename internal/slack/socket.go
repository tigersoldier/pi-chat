package slack

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math/rand"
	"time"

	"github.com/coder/websocket"
)

// readLimit bounds one socket frame. Slack's envelopes are small; file
// payloads carry metadata, not contents.
const readLimit = 1 << 20

// errLinkDisabled reports that Socket Mode was turned off for the app, which
// reconnecting cannot fix.
var errLinkDisabled = errors.New("slack: socket mode is disabled for this app")

// Envelope is one Socket Mode frame: an inbound payload plus the connection's
// own control messages (`hello`, `disconnect`).
type Envelope struct {
	EnvelopeID             string          `json:"envelope_id"`
	Type                   string          `json:"type"`
	Payload                json.RawMessage `json:"payload"`
	AcceptsResponsePayload bool            `json:"accepts_response_payload"`
	Reason                 string          `json:"reason"`
	NumConnections         int             `json:"num_connections"`

	ConnectionInfo struct {
		AppID string `json:"app_id"`
	} `json:"connection_info"`

	DebugInfo struct {
		Host                      string `json:"host"`
		ApproximateConnectionTime int    `json:"approximate_connection_time"`
	} `json:"debug_info"`
}

// Socket is the Socket Mode transport: it holds one WebSocket open, acks every
// envelope, and hands payloads to a handler.
//
// Socket Mode splits deliveries across connections, so exactly one pi-chatd may
// run against an app (DESIGN.md §12). Overlap during a refresh is avoided by
// reconnecting sequentially.
type Socket struct {
	appToken string
	api      *API
	log      *slog.Logger
	handle   func(ctx context.Context, env Envelope)
}

// NewSocket builds the transport. handle runs on its own goroutine per
// envelope and may block for as long as a turn takes.
func NewSocket(appToken string, api *API, log *slog.Logger, handle func(context.Context, Envelope)) *Socket {
	return &Socket{appToken: appToken, api: api, log: log, handle: handle}
}

// Run connects, reconnects, and returns when ctx is canceled or the failure is
// one that retrying cannot fix.
func (s *Socket) Run(ctx context.Context) error {
	backoff := time.Second
	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		started := time.Now()
		err := s.session(ctx)
		if errors.Is(err, errLinkDisabled) {
			return err
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if fatalAuthError(err) {
			return err
		}
		// A connection that lasted a while was healthy; start the backoff over
		// so an hourly refresh does not inherit a grown delay.
		if time.Since(started) > time.Minute {
			backoff = time.Second
		}
		wait := backoff/2 + time.Duration(rand.Int63n(int64(backoff)))
		s.log.Warn("socket disconnected; reconnecting", "error", err, "in", wait)
		select {
		case <-time.After(wait):
		case <-ctx.Done():
			return ctx.Err()
		}
		if backoff < 30*time.Second {
			backoff *= 2
		}
	}
}

// session opens one WebSocket and reads envelopes until it fails.
func (s *Socket) session(ctx context.Context) error {
	url, err := s.api.ConnectionsOpen(ctx, s.appToken)
	if err != nil {
		return fmt.Errorf("apps.connections.open: %w", err)
	}

	sessCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	conn, resp, err := websocket.Dial(sessCtx, url, nil)
	if err != nil {
		if resp != nil {
			// The upgrade's status code is the useful part when Slack refuses
			// the ticket: 401 for a stale or already-used URL.
			return fmt.Errorf("connect the websocket: HTTP %d: %w", resp.StatusCode, err)
		}
		return fmt.Errorf("connect the websocket: %w", err)
	}
	defer func() { _ = conn.CloseNow() }()
	conn.SetReadLimit(readLimit)

	for {
		frameType, data, err := conn.Read(sessCtx)
		if err != nil {
			if sessCtx.Err() != nil {
				// We closed it ourselves (refresh or shutdown).
				return err
			}
			return fmt.Errorf("read from the websocket: %w", err)
		}
		if frameType != websocket.MessageText {
			s.log.Warn("ignoring a non-text socket frame", "type", frameType)
			continue
		}
		var env Envelope
		if err := json.Unmarshal(data, &env); err != nil {
			s.log.Warn("ignoring an unparseable socket frame", "error", err)
			continue
		}

		switch env.Type {
		case "hello":
			refresh := refreshAfter(env.DebugInfo.ApproximateConnectionTime)
			s.log.Info("socket connected",
				"host", env.DebugInfo.Host,
				"app", env.ConnectionInfo.AppID,
				"connections", env.NumConnections,
				"refresh_after", refresh)
			go s.keepalive(sessCtx, cancel, conn, refresh)

		case "disconnect":
			s.log.Info("Slack asked for a disconnect", "reason", env.Reason)
			if env.Reason == "link_disabled" {
				return errLinkDisabled
			}
			// warning, refresh_requested, too_many_connections: the URL is
			// stale, so return and let Run open a fresh one.
			return fmt.Errorf("slack asked for a reconnect: %s", env.Reason)

		default:
			// Acknowledge before doing anything else: Slack retries an
			// envelope it has not seen acknowledged within three seconds.
			if err := ack(sessCtx, conn, env.EnvelopeID); err != nil {
				return fmt.Errorf("acknowledge %s: %w", env.EnvelopeID, err)
			}
			// The handler outlives this connection: a socket refresh must not
			// cancel a turn that is already running.
			go s.handle(ctx, env)
		}
	}
}

// keepalive proves the connection is alive and refreshes it before Slack's own
// lifetime limit, rather than racing a disconnect.
func (s *Socket) keepalive(ctx context.Context, cancel context.CancelFunc, conn *websocket.Conn, refresh time.Duration) {
	pings := time.NewTicker(30 * time.Second)
	defer pings.Stop()
	refreshes := time.NewTimer(refresh)
	defer refreshes.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-refreshes.C:
			s.log.Info("refreshing the socket connection", "after", refresh)
			cancel()
			return
		case <-pings.C:
			pingCtx, cancelPing := context.WithTimeout(ctx, 15*time.Second)
			err := conn.Ping(pingCtx)
			cancelPing()
			if err != nil {
				s.log.Warn("the socket did not answer a ping; reconnecting", "error", err)
				cancel()
				return
			}
		}
	}
}

// refreshAfter is when to replace a connection, given Slack's estimate of how
// long it will last.
func refreshAfter(seconds int) time.Duration {
	const (
		earliest = 5 * time.Minute
		latest   = 50 * time.Minute
	)
	if seconds <= 0 {
		return latest
	}
	switch delay := time.Duration(float64(seconds) * 0.9 * float64(time.Second)); {
	case delay < earliest:
		return earliest
	case delay > latest:
		return latest
	default:
		return delay
	}
}

// ack acknowledges one envelope. Nothing else is required: the socket is
// pre-authenticated, so payloads need no signature check (unlike HTTP mode).
func ack(ctx context.Context, conn *websocket.Conn, envelopeID string) error {
	if envelopeID == "" {
		return nil
	}
	raw, err := json.Marshal(struct {
		EnvelopeID string `json:"envelope_id"`
	}{EnvelopeID: envelopeID})
	if err != nil {
		return err
	}
	return conn.Write(ctx, websocket.MessageText, raw)
}

// fatalAuthError reports errors that retrying cannot fix: a revoked token or an
// app whose Socket Mode was switched off.
func fatalAuthError(err error) bool {
	for _, code := range []string{"invalid_auth", "not_authed", "account_inactive", "token_revoked", "missing_scope"} {
		if IsCode(err, code) {
			return true
		}
	}
	return false
}
