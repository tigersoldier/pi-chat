package chattools

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/tigersoldier/pi-chat/internal/store"
)

type recordingProvider struct {
	mu      sync.Mutex
	calls   int
	binding store.SessionToolBinding
	tool    string
	args    json.RawMessage
	err     error
}

func (p *recordingProvider) Call(_ context.Context, binding store.SessionToolBinding, tool string, args json.RawMessage) (ToolResult, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.calls++
	p.binding, p.tool, p.args = binding, tool, args
	if p.err != nil {
		return ToolResult{}, p.err
	}
	return ToolResult{Text: "UNTRUSTED chat text", Truncated: true}, nil
}

type waitingProvider struct {
	started   chan struct{}
	cancelled chan struct{}
	once      sync.Once
}

func (p *waitingProvider) Call(ctx context.Context, _ store.SessionToolBinding, _ string, _ json.RawMessage) (ToolResult, error) {
	p.once.Do(func() { close(p.started) })
	<-ctx.Done()
	close(p.cancelled)
	return ToolResult{}, ctx.Err()
}

func testBroker(t *testing.T, provider Provider) (*Broker, *store.Store, string) {
	t.Helper()
	st, _ := storeTestStore(t)
	socket := filepath.Join(t.TempDir(), "run", "chat-tools.sock")
	b, err := Listen(BrokerConfig{
		SocketPath:     socket,
		Timeout:        time.Second,
		MaxResultBytes: 1024,
		Store:          st,
		Providers: map[ProviderKey]Provider{
			{Provider: "slack", Surface: "app", WorkspaceID: "T1"}: provider,
		},
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		if err := b.Close(ctx); err != nil {
			t.Errorf("Close: %v", err)
		}
	})
	return b, st, socket
}

// storeTestStore opens the real sqlite store in an isolated test directory.
func storeTestStore(t *testing.T) (*store.Store, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "state", "chat.db")
	st, err := store.Open(t.Context(), path)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() {
		if err := st.Close(); err != nil {
			t.Errorf("store.Close: %v", err)
		}
	})
	return st, path
}

func boundSession(t *testing.T, st *store.Store, path string) {
	t.Helper()
	if err := st.BindSessionToolBinding(t.Context(), store.SessionToolBinding{
		SessionPath: path, Provider: "slack", Surface: "app", WorkspaceID: "T1",
		ChannelID: "C1", ThreadTS: "1700000000.000100",
		Capabilities:     []string{"read_thread", "read_message", "read_user"},
		ExtensionVersion: "v1", State: store.ToolBindingActive,
	}, false); err != nil {
		t.Fatalf("BindSessionToolBinding: %v", err)
	}
}

func brokerRequest(t *testing.T, socket string, body string) (*http.Response, []byte) {
	t.Helper()
	tr := &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", socket)
	}}
	client := &http.Client{Transport: tr, Timeout: 3 * time.Second}
	t.Cleanup(tr.CloseIdleConnections)
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, "http://unix/v1/tool-call", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("POST broker: %v", err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return resp, data
}

func TestBrokerLooksUpBindingAndCallsSurfaceProvider(t *testing.T) {
	provider := &recordingProvider{}
	_, st, socket := testBroker(t, provider)
	session := filepath.Join(t.TempDir(), "session.jsonl")
	boundSession(t, st, session)
	request := `{"version":1,"sessionPath":"` + session + `","callId":"call-1","tool":"chat_read_thread","arguments":{"reference":"C1:1700000000.000100"}}`
	resp, data := brokerRequest(t, socket, request)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d: %s", resp.StatusCode, data)
	}
	var got response
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatal(err)
	}
	if !got.OK || got.CallID != "call-1" || !got.Truncated || len(got.Content) != 1 || got.Content[0].Text != "UNTRUSTED chat text" {
		t.Fatalf("response = %+v", got)
	}
	if provider.calls != 1 || provider.binding.SessionPath != session || provider.tool != ToolReadThread {
		t.Fatalf("provider call = %d binding=%+v tool=%s", provider.calls, provider.binding, provider.tool)
	}
}

func TestBrokerFailsClosedForMissingOrRevokedBinding(t *testing.T) {
	provider := &recordingProvider{}
	_, st, socket := testBroker(t, provider)
	session := filepath.Join(t.TempDir(), "missing.jsonl")
	request := `{"version":1,"sessionPath":"` + session + `","callId":"c1","tool":"chat_read_thread","arguments":{"reference":"C1:1.1"}}`
	_, data := brokerRequest(t, socket, request)
	var got response
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatal(err)
	}
	if got.OK || got.Error == nil || got.Error.Code != "no_binding" {
		t.Fatalf("missing binding response = %+v", got)
	}
	boundSession(t, st, session)
	if err := st.RevokeSessionToolBinding(t.Context(), session); err != nil {
		t.Fatal(err)
	}
	_, data = brokerRequest(t, socket, request)
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatal(err)
	}
	if got.OK || got.Error == nil || got.Error.Code != "no_binding" || provider.calls != 0 {
		t.Fatalf("revoked binding response=%+v provider calls=%d", got, provider.calls)
	}
}

func TestBrokerRejectsUnsupportedExtensionVersion(t *testing.T) {
	provider := &recordingProvider{}
	_, st, socket := testBroker(t, provider)
	session := filepath.Join(t.TempDir(), "version.jsonl")
	if err := st.BindSessionToolBinding(t.Context(), store.SessionToolBinding{
		SessionPath: session, Provider: "slack", Surface: "app", WorkspaceID: "T1",
		ChannelID: "C1", ThreadTS: "1700000000.000100", Capabilities: []string{"read_thread"},
		ExtensionVersion: "v2", State: store.ToolBindingActive,
	}, false); err != nil {
		t.Fatal(err)
	}
	request := `{"version":1,"sessionPath":"` + session + `","callId":"c1","tool":"chat_read_thread","arguments":{"reference":"C1:1700000000.000100"}}`
	_, data := brokerRequest(t, socket, request)
	var got response
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatal(err)
	}
	if got.OK || got.Error == nil || got.Error.Code != "unsupported_tool" || provider.calls != 0 {
		t.Fatalf("response = %+v, provider calls = %d", got, provider.calls)
	}
}

func TestBrokerRejectsMalformedAndOversizedRequests(t *testing.T) {
	_, _, socket := testBroker(t, &recordingProvider{})
	for _, tc := range []struct {
		name string
		body string
		code int
	}{
		{"malformed", "{", http.StatusBadRequest},
		{"unknown field", `{"version":1,"sessionPath":"/s/a","callId":"c","tool":"chat_read_thread","arguments":{},"token":"secret"}`, http.StatusBadRequest},
		{"oversized", `{"version":1,"sessionPath":"/s/a","callId":"c","tool":"chat_read_thread","arguments":{"reference":"` + strings.Repeat("x", maxRequestBytes) + `"}}`, http.StatusRequestEntityTooLarge},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resp, _ := brokerRequest(t, socket, tc.body)
			if resp.StatusCode != tc.code {
				t.Errorf("status = %d, want %d", resp.StatusCode, tc.code)
			}
		})
	}
}

func TestBrokerSocketPermissionsAndReplacementSafety(t *testing.T) {
	provider := &recordingProvider{}
	b, _, socket := testBroker(t, provider)
	info, err := os.Stat(socket)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Errorf("socket mode = %o, want 600", info.Mode().Perm())
	}
	parent, err := os.Stat(filepath.Dir(socket))
	if err != nil {
		t.Fatal(err)
	}
	if parent.Mode().Perm() != 0o700 {
		t.Errorf("parent mode = %o, want 700", parent.Mode().Perm())
	}
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	if err := b.Close(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(socket); !os.IsNotExist(err) {
		t.Errorf("socket after Close: %v, want removed", err)
	}
}

func TestListenRejectsInsecureSocketParentWithoutChangingIt(t *testing.T) {
	parent := filepath.Join(t.TempDir(), "shared")
	if err := os.Mkdir(parent, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(parent, 0o755); err != nil {
		t.Fatal(err)
	}
	st, _ := storeTestStore(t)
	_, err := Listen(BrokerConfig{SocketPath: filepath.Join(parent, "chat-tools.sock"), Store: st})
	if err == nil {
		t.Fatal("Listen accepted a socket in a shared directory")
	}
	info, statErr := os.Stat(parent)
	if statErr != nil {
		t.Fatal(statErr)
	}
	if info.Mode().Perm() != 0o755 {
		t.Fatalf("Listen changed shared parent permissions to %04o", info.Mode().Perm())
	}
}

func TestListenRejectsAReplaceableAncestor(t *testing.T) {
	root := t.TempDir()
	if err := os.Chmod(root, 0o777); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "run", "chat-tools.sock")
	st, _ := storeTestStore(t)
	if _, err := Listen(BrokerConfig{SocketPath: path, Store: st}); err == nil {
		t.Fatal("Listen accepted an owner-only directory under a replaceable ancestor")
	}
	if _, err := os.Lstat(path); !os.IsNotExist(err) {
		t.Fatalf("socket path after rejection = %v, want absent", err)
	}
}

func TestListenDoesNotUnlinkAnotherLiveBroker(t *testing.T) {
	path := filepath.Join(t.TempDir(), "run", "chat-tools.sock")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	st, _ := storeTestStore(t)
	if _, err := Listen(BrokerConfig{SocketPath: path, Store: st}); err == nil {
		t.Fatal("Listen replaced an active socket")
	}
	conn, err := net.DialTimeout("unix", path, time.Second)
	if err != nil {
		t.Fatalf("the original listener was unlinked: %v", err)
	}
	_ = conn.Close()
}

func TestBrokerReturnsBoundedTimeoutAndPropagatesCancellation(t *testing.T) {
	t.Run("deadline", func(t *testing.T) {
		provider := &waitingProvider{started: make(chan struct{}), cancelled: make(chan struct{})}
		_, st, socket := testBroker(t, provider)
		session := filepath.Join(t.TempDir(), "timeout.jsonl")
		boundSession(t, st, session)
		request := `{"version":1,"sessionPath":"` + session + `","callId":"timeout-1","tool":"chat_read_thread","arguments":{"reference":"C1:1700000000.000100"}}`
		_, data := brokerRequest(t, socket, request)
		var got response
		if err := json.Unmarshal(data, &got); err != nil {
			t.Fatal(err)
		}
		if got.OK || got.Error == nil || got.Error.Code != "timeout" {
			t.Fatalf("timeout response = %+v", got)
		}
		select {
		case <-provider.cancelled:
		case <-time.After(time.Second):
			t.Fatal("provider context did not receive the deadline")
		}
	})

	t.Run("client abort", func(t *testing.T) {
		provider := &waitingProvider{started: make(chan struct{}), cancelled: make(chan struct{})}
		_, st, socket := testBroker(t, provider)
		session := filepath.Join(t.TempDir(), "cancelled.jsonl")
		boundSession(t, st, session)
		tr := &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", socket)
		}}
		t.Cleanup(tr.CloseIdleConnections)
		client := &http.Client{Transport: tr}
		ctx, cancel := context.WithCancel(t.Context())
		request := `{"version":1,"sessionPath":"` + session + `","callId":"cancel-1","tool":"chat_read_thread","arguments":{"reference":"C1:1700000000.000100"}}`
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://unix/v1/tool-call", strings.NewReader(request))
		if err != nil {
			t.Fatal(err)
		}
		done := make(chan struct{})
		go func() {
			resp, _ := client.Do(req)
			if resp != nil {
				_ = resp.Body.Close()
			}
			close(done)
		}()
		select {
		case <-provider.started:
		case <-time.After(time.Second):
			cancel()
			t.Fatal("provider was not called")
		}
		cancel()
		select {
		case <-provider.cancelled:
		case <-time.After(time.Second):
			t.Fatal("client abort did not cancel the provider")
		}
		select {
		case <-done:
		case <-time.After(time.Second):
			t.Fatal("client request did not finish after abort")
		}
	})
}

func TestRateLimiterEnforcesSessionAndAppWindows(t *testing.T) {
	app := ProviderKey{Provider: "slack", Surface: "app", WorkspaceID: "T1"}
	now := time.Now()
	perSession := newRateLimiter()
	for i := range perSessionLimit {
		if !perSession.allow("one-session", app, now) {
			t.Fatalf("session call %d was unexpectedly denied", i+1)
		}
	}
	if perSession.allow("one-session", app, now) {
		t.Fatal("the per-session limit was not enforced")
	}

	perApp := newRateLimiter()
	for session := range perAppLimit / perSessionLimit {
		for call := range perSessionLimit {
			if !perApp.allow(fmt.Sprintf("session-%d", session), app, now) {
				t.Fatalf("app call %d was unexpectedly denied", session*perSessionLimit+call+1)
			}
		}
	}
	if perApp.allow("next-session", app, now) {
		t.Fatal("the per-app limit was not enforced")
	}
	if !perApp.allow("next-session", app, now.Add(rateWindow)) {
		t.Fatal("the app rate limit did not reset after its window")
	}
}

func TestRateLimiterPrunesExpiredSessionEntries(t *testing.T) {
	app := ProviderKey{Provider: "slack", Surface: "app", WorkspaceID: "T1"}
	rate := newRateLimiter()
	now := time.Now()
	for i := range perAppLimit {
		if !rate.allow(fmt.Sprintf("old-session-%d", i), app, now) {
			t.Fatalf("initial call %d was unexpectedly denied", i+1)
		}
	}
	for i := range 32 {
		if rate.allow(fmt.Sprintf("blocked-session-%d", i), app, now) {
			t.Fatal("app rate limit was not enforced")
		}
	}
	if got := len(rate.sessions); got != perAppLimit {
		t.Fatalf("rate-limited calls retained empty session entries: %d, want %d", got, perAppLimit)
	}

	later := now.Add(rateWindow)
	for i := range 256 {
		rate.allow(fmt.Sprintf("new-session-%d", i), app, later)
	}
	for key := range rate.sessions {
		if strings.HasPrefix(key, "old-session-") {
			t.Errorf("expired session entry %q was retained", key)
		}
	}
}

func TestBrokerLimitsResultAtUTF8Boundary(t *testing.T) {
	got, truncated := limitText(strings.Repeat("é", 60), 100)
	if !truncated || len(got) > 100 || !utf8.ValidString(got) || !strings.Contains(got, "Output truncated") {
		t.Errorf("limitText = %q, truncated=%v", got, truncated)
	}
}
