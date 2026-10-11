package chattools

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"
	"unicode/utf8"

	"github.com/tigersoldier/pi-chat/internal/store"
)

const (
	maxRequestBytes = 32 * 1024
	rateWindow      = time.Minute
	perSessionLimit = 20
	perAppLimit     = 120
)

// BrokerConfig wires the socket, metadata store and surface providers.
type BrokerConfig struct {
	SocketPath     string
	Timeout        time.Duration
	MaxResultBytes int
	Store          *store.Store
	Providers      map[ProviderKey]Provider
	Logger         *slog.Logger
}

// Broker is one local HTTP-over-Unix-socket listener. It owns no session
// identity: each request resolves its exact canonical session path in Store.
type Broker struct {
	path      string
	fileInfo  os.FileInfo
	store     *store.Store
	providers map[ProviderKey]Provider
	timeout   time.Duration
	maxBytes  int
	log       *slog.Logger
	limiter   *rateLimiter
	server    *http.Server
	listener  net.Listener
	serveDone chan error
	closeOnce sync.Once
	closeErr  error
}

// Listen creates the private Unix socket and starts serving requests.
func Listen(cfg BrokerConfig) (*Broker, error) {
	if cfg.Store == nil {
		return nil, errors.New("chat tools: store is required")
	}
	if !filepath.IsAbs(cfg.SocketPath) {
		return nil, errors.New("chat tools: socket path must be absolute")
	}
	if cfg.Timeout <= 0 {
		cfg.Timeout = 15 * time.Second
	}
	if cfg.MaxResultBytes <= 0 || cfg.MaxResultBytes > 16*1024 {
		cfg.MaxResultBytes = 16 * 1024
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}

	parent := filepath.Dir(cfg.SocketPath)
	if err := os.MkdirAll(parent, 0o700); err != nil {
		return nil, fmt.Errorf("chat tools: create socket directory: %w", err)
	}
	if err := validatePrivateDirectory(parent); err != nil {
		return nil, err
	}
	if err := removeStaleSocket(cfg.SocketPath); err != nil {
		return nil, err
	}

	listener, err := net.Listen("unix", cfg.SocketPath)
	if err != nil {
		return nil, fmt.Errorf("chat tools: listen on Unix socket: %w", err)
	}
	cleanup := func() { _ = listener.Close(); _ = os.Remove(cfg.SocketPath) }
	if err := os.Chmod(cfg.SocketPath, 0o600); err != nil {
		cleanup()
		return nil, fmt.Errorf("chat tools: secure Unix socket: %w", err)
	}
	fileInfo, err := os.Lstat(cfg.SocketPath)
	if err != nil {
		cleanup()
		return nil, fmt.Errorf("chat tools: inspect bound socket: %w", err)
	}

	b := &Broker{
		path:      cfg.SocketPath,
		fileInfo:  fileInfo,
		store:     cfg.Store,
		providers: cloneProviders(cfg.Providers),
		timeout:   cfg.Timeout,
		maxBytes:  cfg.MaxResultBytes,
		log:       cfg.Logger,
		limiter:   newRateLimiter(),
		listener:  listener,
		serveDone: make(chan error, 1),
	}
	b.server = &http.Server{
		Handler:           http.HandlerFunc(b.serveHTTP),
		ReadHeaderTimeout: 2 * time.Second,
		ReadTimeout:       5 * time.Second,
		WriteTimeout:      cfg.Timeout + 2*time.Second,
		IdleTimeout:       time.Second,
		MaxHeaderBytes:    8 * 1024,
	}
	go func() {
		err := b.server.Serve(listener)
		if errors.Is(err, http.ErrServerClosed) || errors.Is(err, net.ErrClosed) {
			err = nil
		}
		b.serveDone <- err
		if err != nil {
			b.log.Error("chat-tools broker stopped", "error", err)
		}
	}()
	return b, nil
}

func removeStaleSocket(path string) error {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("chat tools: inspect socket path: %w", err)
	}
	if info.Mode()&os.ModeSocket == 0 {
		return errors.New("chat tools: refusing to replace a non-socket at the configured path")
	}
	conn, err := net.DialTimeout("unix", path, 100*time.Millisecond)
	if err == nil {
		_ = conn.Close()
		return errors.New("chat tools: a live broker already uses the configured socket path")
	}
	if !errors.Is(err, syscall.ECONNREFUSED) && !errors.Is(err, syscall.ENOENT) {
		return fmt.Errorf("chat tools: cannot determine whether the existing socket is stale: %w", err)
	}
	current, err := os.Lstat(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("chat tools: recheck socket path: %w", err)
	}
	if !os.SameFile(info, current) {
		return errors.New("chat tools: socket path changed while checking for a stale listener")
	}
	if err := os.Remove(path); err != nil {
		return fmt.Errorf("chat tools: remove stale socket: %w", err)
	}
	return nil
}

func cloneProviders(in map[ProviderKey]Provider) map[ProviderKey]Provider {
	out := make(map[ProviderKey]Provider, len(in))
	for key, provider := range in {
		if provider != nil {
			out[key] = provider
		}
	}
	return out
}

// Close stops the listener and removes only the socket this Broker created.
func (b *Broker) Close(ctx context.Context) error {
	if b == nil {
		return nil
	}
	b.closeOnce.Do(func() {
		if err := b.server.Shutdown(ctx); err != nil {
			b.closeErr = err
			_ = b.listener.Close()
		}
		if serveErr := <-b.serveDone; serveErr != nil && b.closeErr == nil {
			b.closeErr = serveErr
		}
		if info, err := os.Lstat(b.path); err == nil && os.SameFile(info, b.fileInfo) {
			if err := os.Remove(b.path); err != nil && b.closeErr == nil {
				b.closeErr = err
			}
		}
	})
	return b.closeErr
}

func (b *Broker) serveHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Connection", "close")
	if r.Method != http.MethodPost || r.URL.Path != "/v1/tool-call" {
		writeJSON(w, http.StatusNotFound, response{OK: false, Error: &wireError{
			Code: "not_found", Message: "Unknown chat-tools broker endpoint.",
		}})
		return
	}

	started := time.Now()
	callCtx, cancel := context.WithTimeout(r.Context(), b.timeout)
	defer cancel()
	body := http.MaxBytesReader(w, r.Body, maxRequestBytes)
	defer body.Close()
	dec := json.NewDecoder(body)
	dec.DisallowUnknownFields()
	var req request
	if err := dec.Decode(&req); err != nil {
		if _, ok := errors.AsType[*http.MaxBytesError](err); ok {
			writeJSON(w, http.StatusRequestEntityTooLarge, response{OK: false, Error: &wireError{
				Code: "invalid_request", Message: "Chat tool request exceeds the 32 KiB limit.",
			}})
			return
		}
		writeJSON(w, http.StatusBadRequest, response{OK: false, Error: &wireError{
			Code: "invalid_request", Message: "Malformed chat tool request.",
		}})
		return
	}
	var extra any
	if err := dec.Decode(&extra); !errors.Is(err, io.EOF) {
		writeJSON(w, http.StatusBadRequest, response{OK: false, Error: &wireError{
			Code: "invalid_request", Message: "Malformed chat tool request.",
		}})
		return
	}
	if req.Version != ProtocolVersion || !validCallID(req.CallID) ||
		!jsonObject(req.Arguments) || strings.TrimSpace(req.Tool) == "" {
		writeJSON(w, http.StatusBadRequest, response{OK: false, Error: &wireError{
			Code: "invalid_request", Message: "Malformed chat tool request.",
		}})
		return
	}
	capability, supported := toolCapability(req.Tool)
	if !supported {
		b.writeToolError(w, req.CallID, req.Tool, "unsupported_tool", "This chat tool is not available.", "unsupported_tool", "")
		return
	}
	sessionPath, err := store.CanonicalSessionPath(req.SessionPath)
	if err != nil {
		b.writeToolError(w, req.CallID, req.Tool, "invalid_request", "The Pi session identity is unavailable.", "invalid_request", "")
		return
	}
	sessionHash := hashPath(sessionPath)
	binding, found, err := b.store.SessionToolBinding(callCtx, sessionPath)
	if err != nil {
		b.writeToolError(w, req.CallID, req.Tool, "broker_unavailable", "Chat tools are unavailable right now.", "store_error", sessionHash)
		return
	}
	if !found || binding.State != store.ToolBindingActive {
		b.writeToolError(w, req.CallID, req.Tool, "no_binding", "This Pi session has no active chat-tools binding.", "no_binding", sessionHash)
		return
	}
	if binding.ExtensionVersion != ExtensionVersion {
		b.writeToolError(w, req.CallID, req.Tool, "unsupported_tool", "This session's chat-tools extension version is not supported.", "unsupported_version", sessionHash)
		return
	}
	if !bindingAllows(binding, capability) {
		b.writeToolError(w, req.CallID, req.Tool, "unsupported_tool", "This chat tool is not allowed for the session.", "unsupported_tool", sessionHash)
		return
	}
	key := ProviderKey{Provider: binding.Provider, Surface: binding.Surface, WorkspaceID: binding.WorkspaceID}
	provider := b.providers[key]
	if provider == nil {
		b.writeToolError(w, req.CallID, req.Tool, "auth_scope", "The bound chat platform is unavailable.", "provider_unavailable", sessionHash)
		return
	}
	if !b.limiter.allow(sessionHash, key, time.Now()) {
		b.writeToolError(w, req.CallID, req.Tool, "rate_limited", "Chat tool rate limit reached; try again shortly.", "rate_limited", sessionHash)
		return
	}

	result, err := provider.Call(callCtx, binding, req.Tool, bytes.Clone(req.Arguments))
	if err != nil {
		code, message, class := publicError(err)
		b.writeToolError(w, req.CallID, req.Tool, code, message, class, sessionHash)
		return
	}
	text, truncated := limitText(result.Text, b.maxBytes)
	truncated = truncated || result.Truncated
	resultBytes := len(text)
	b.log.Info("chat tool call",
		"session_hash", sessionHash,
		"tool", req.Tool,
		"duration_ms", time.Since(started).Milliseconds(),
		"result_bytes", resultBytes,
		"truncated", truncated)
	writeJSON(w, http.StatusOK, response{
		OK:        true,
		CallID:    req.CallID,
		Content:   []textContent{{Type: "text", Text: text}},
		Truncated: truncated,
	})
}

func (b *Broker) writeToolError(w http.ResponseWriter, callID, tool, code, message, class, sessionHash string) {
	if _, supported := toolCapability(tool); !supported {
		tool = "unsupported"
	}
	attrs := []any{"tool_error", class, "tool", tool}
	if sessionHash != "" {
		attrs = append(attrs, "session_hash", sessionHash)
	}
	b.log.Warn("chat tool call denied or failed", attrs...)
	writeJSON(w, http.StatusOK, response{
		OK:     false,
		CallID: callID,
		Error:  &wireError{Code: code, Message: message},
	})
}

func publicError(err error) (code, message, class string) {
	if toolErr, ok := errors.AsType[*CallError](err); ok {
		return toolErr.Code, toolErr.Message, toolErr.Code
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return "timeout", "The chat platform did not respond before the deadline.", "timeout"
	}
	if errors.Is(err, context.Canceled) {
		return "timeout", "The chat tool call was cancelled.", "cancelled"
	}
	return "broker_unavailable", "The chat platform could not complete this read.", "provider_error"
}

func validCallID(id string) bool {
	if len(id) == 0 || len(id) > 256 {
		return false
	}
	for _, r := range id {
		if r < 0x20 || r == 0x7f {
			return false
		}
	}
	return true
}

func jsonObject(raw json.RawMessage) bool {
	if len(raw) == 0 || raw[0] != '{' || !json.Valid(raw) {
		return false
	}
	return true
}

func hashPath(path string) string {
	sum := sha256.Sum256([]byte(path))
	return hex.EncodeToString(sum[:8])
}

func writeJSON(w http.ResponseWriter, status int, payload response) {
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(payload)
}

func limitText(text string, maxBytes int) (string, bool) {
	if maxBytes <= 0 || len(text) <= maxBytes {
		return text, false
	}
	const suffix = "\n[Output truncated to the configured chat-tools limit.]"
	budget := maxBytes - len(suffix)
	if budget < 0 {
		budget = maxBytes
	}
	text = text[:budget]
	for len(text) > 0 && !utf8.ValidString(text) {
		text = text[:len(text)-1]
	}
	if len(text)+len(suffix) <= maxBytes {
		text += suffix
	}
	return text, true
}

type rateLimiter struct {
	mu       sync.Mutex
	sessions map[string][]time.Time
	apps     map[ProviderKey][]time.Time
	calls    uint64
}

func newRateLimiter() *rateLimiter {
	return &rateLimiter{sessions: make(map[string][]time.Time), apps: make(map[ProviderKey][]time.Time)}
}

func (l *rateLimiter) allow(session string, app ProviderKey, now time.Time) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.calls++
	if l.calls%256 == 0 {
		for key, events := range l.sessions {
			if events = prune(events, now); len(events) == 0 {
				delete(l.sessions, key)
			} else {
				l.sessions[key] = events
			}
		}
		for key, events := range l.apps {
			if events = prune(events, now); len(events) == 0 {
				delete(l.apps, key)
			} else {
				l.apps[key] = events
			}
		}
	}
	sessions := prune(l.sessions[session], now)
	apps := prune(l.apps[app], now)
	if len(sessions) >= perSessionLimit || len(apps) >= perAppLimit {
		if len(sessions) == 0 {
			delete(l.sessions, session)
		} else {
			l.sessions[session] = sessions
		}
		if len(apps) == 0 {
			delete(l.apps, app)
		} else {
			l.apps[app] = apps
		}
		return false
	}
	l.sessions[session] = append(sessions, now)
	l.apps[app] = append(apps, now)
	return true
}

func prune(events []time.Time, now time.Time) []time.Time {
	first := 0
	for first < len(events) && now.Sub(events[first]) >= rateWindow {
		first++
	}
	if first == 0 {
		return events
	}
	return append([]time.Time(nil), events[first:]...)
}
