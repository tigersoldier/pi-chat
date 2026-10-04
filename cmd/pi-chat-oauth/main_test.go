package main

import (
	"bytes"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
)

func discardLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// stubSlack stands in for the `oauth.v2.access` endpoint.
type stubSlack struct {
	mu    sync.Mutex
	calls []url.Values
	reply string
}

func newStubSlack(reply string) *stubSlack {
	return &stubSlack{reply: reply}
}

func (s *stubSlack) handler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		form, _ := url.ParseQuery(string(body))
		s.mu.Lock()
		s.calls = append(s.calls, form)
		s.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, s.reply)
	}
}

// last is the most recent exchange request, or an empty form.
func (s *stubSlack) last() url.Values {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.calls) == 0 {
		return url.Values{}
	}
	return s.calls[len(s.calls)-1]
}

func (s *stubSlack) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.calls)
}

// newTestServer builds a broker pointed at a stub Slack, with logging the test
// can inspect.
func newTestServer(t *testing.T, log *slog.Logger) (*server, *stubSlack) {
	t.Helper()
	stub := newStubSlack(`{"ok":false,"error":"not_configured"}`)
	api := httptest.NewServer(stub.handler())
	t.Cleanup(api.Close)

	srv, err := newServer(settings{
		ClientID:     "123.456",
		ClientSecret: "shhh",
		SlackAPI:     api.URL,
		AuthorizeURL: "https://slack.example/oauth/v2/authorize",
	}, log)
	if err != nil {
		t.Fatalf("newServer: %v", err)
	}
	return srv, stub
}

// get runs one request through the broker's handler.
func get(t *testing.T, srv *server, target string, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, target, nil)
	for k, v := range headers {
		if k == "Host" {
			req.Host = v
			continue
		}
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	srv.handler().ServeHTTP(rec, req)
	return rec
}

// mustState signs an install link the way /install does.
func mustState(t *testing.T, srv *server, redirect string) string {
	t.Helper()
	state, err := srv.signState(redirect)
	if err != nil {
		t.Fatalf("signState: %v", err)
	}
	return state
}

func TestNewServerNeedsCredentials(t *testing.T) {
	cases := []struct{ name, id, secret, want string }{
		{"no client id", "", "shhh", "client id"},
		{"no client secret", "123.456", "", "client secret"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := newServer(settings{ClientID: tc.id, ClientSecret: tc.secret}, discardLogger())
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("newServer = %v, want an error mentioning %q", err, tc.want)
			}
		})
	}
}

// TestInstallRedirectsWithUserScopes: the install link asks for user scopes
// only, so the authorization is the person's rather than a workspace install.
func TestInstallRedirectsWithUserScopes(t *testing.T) {
	srv, _ := newTestServer(t, discardLogger())
	srv.cfg.RedirectURL = "https://broker.example/callback"

	rec := get(t, srv, "/install", nil)
	if rec.Code != http.StatusFound {
		t.Fatalf("GET /install = %d, want 302", rec.Code)
	}
	loc, err := url.Parse(rec.Header().Get("Location"))
	if err != nil {
		t.Fatalf("Location is not a URL: %v", err)
	}
	if loc.Scheme+"://"+loc.Host+loc.Path != "https://slack.example/oauth/v2/authorize" {
		t.Errorf("authorize URL = %q, want the configured Slack endpoint", loc.String())
	}
	q := loc.Query()
	if q.Get("client_id") != "123.456" {
		t.Errorf("client_id = %q", q.Get("client_id"))
	}
	if got, want := q.Get("user_scope"), strings.Join(defaultScopes, ","); got != want {
		t.Errorf("user_scope = %q, want %q", got, want)
	}
	if q.Get("scope") != "" {
		t.Errorf("the link requested bot scope %q; a self-DM authorization must be user-scoped only", q.Get("scope"))
	}
	if q.Get("redirect_uri") != "https://broker.example/callback" {
		t.Errorf("redirect_uri = %q", q.Get("redirect_uri"))
	}
	if _, err := srv.checkState(q.Get("state")); err != nil {
		t.Errorf("the state the link carried does not verify: %v", err)
	}
}

// TestCallbackExchangesTheCodeForAUserToken is the whole job: code in, token
// shown once, with the headers that keep it out of caches.
func TestCallbackExchangesTheCodeForAUserToken(t *testing.T) {
	srv, stub := newTestServer(t, discardLogger())
	srv.cfg.RedirectURL = "https://broker.example/callback"
	stub.reply = `{"ok":true,"authed_user":{"id":"U01234567","scope":"im:history,chat:write",` +
		`"access_token":"xoxp-1-2-the-user-token"},"team":{"id":"T1","name":"Acme"}}`

	state := mustState(t, srv, srv.cfg.RedirectURL)
	rec := get(t, srv, "/callback?code=the-code&state="+url.QueryEscape(state), nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /callback = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	for _, want := range []string{"xoxp-1-2-the-user-token", "U01234567", "Acme", "user_oauth", "shown once"} {
		if !strings.Contains(body, want) {
			t.Errorf("the page should contain %q", want)
		}
	}
	if cache := rec.Header().Get("Cache-Control"); !strings.Contains(cache, "no-store") {
		t.Errorf("Cache-Control = %q, want no-store", cache)
	}
	if got := rec.Header().Get("Referrer-Policy"); got != "no-referrer" {
		t.Errorf("Referrer-Policy = %q, want no-referrer", got)
	}

	form := stub.last()
	if form.Get("code") != "the-code" || form.Get("client_id") != "123.456" || form.Get("client_secret") != "shhh" {
		t.Errorf("exchange form = %v, want the code and the app credentials", form)
	}
	if form.Get("redirect_uri") != srv.cfg.RedirectURL {
		t.Errorf("exchange redirect_uri = %q, want the one the install used (%q)",
			form.Get("redirect_uri"), srv.cfg.RedirectURL)
	}
}

// TestCallbackRejectsAForgedState: no Slack call at all for a state this broker
// did not sign.
func TestCallbackRejectsAForgedState(t *testing.T) {
	srv, stub := newTestServer(t, discardLogger())
	rec := get(t, srv, "/callback?code=the-code&state=AAAA.BBBB", nil)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("GET /callback with a forged state = %d, want 400", rec.Code)
	}
	if stub.count() != 0 {
		t.Errorf("the broker exchanged a code for a forged state")
	}
}

// TestCallbackSurfacesASlackRefusal: Slack's own code is what the person needs,
// because it is the one that says what to fix.
func TestCallbackSurfacesASlackRefusal(t *testing.T) {
	srv, stub := newTestServer(t, discardLogger())
	stub.reply = `{"ok":false,"error":"invalid_code"}`

	rec := get(t, srv, "/callback?code=stale&state="+url.QueryEscape(mustState(t, srv, "https://broker.example/callback")), nil)
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("GET /callback with a refused exchange = %d, want 502", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "invalid_code") {
		t.Errorf("the page should name Slack's error, got: %s", rec.Body.String())
	}
}

// TestCallbackHidesARotatingToken: a token pi-chatd cannot use must not be
// invited into a configuration file.
func TestCallbackHidesARotatingToken(t *testing.T) {
	srv, stub := newTestServer(t, discardLogger())
	stub.reply = `{"ok":true,"authed_user":{"id":"U1","access_token":"xoxe.xoxp-1-secret-value"}}`

	rec := get(t, srv, "/callback?code=c&state="+url.QueryEscape(mustState(t, srv, "https://broker.example/callback")), nil)
	body := rec.Body.String()
	if !strings.Contains(body, "rotation") {
		t.Errorf("the page should explain the rotation problem, got: %s", body)
	}
	if strings.Contains(body, "xoxe.xoxp-1-secret-value") {
		t.Error("the page showed a token that pi-chatd refuses to use")
	}
}

// TestRedirectComesFromTheRequestWhenNotConfigured, and the same URL is repeated
// at the exchange — the state carries it precisely so a proxy cannot make the
// two differ, which Slack answers with bad_redirect_uri.
func TestRedirectComesFromTheRequestWhenNotConfigured(t *testing.T) {
	srv, stub := newTestServer(t, discardLogger())
	stub.reply = `{"ok":true,"authed_user":{"id":"U1","access_token":"xoxp-1-2-token"}}`

	rec := get(t, srv, "/install", map[string]string{
		"Host": "broker.example", "X-Forwarded-Proto": "https",
	})
	loc, err := url.Parse(rec.Header().Get("Location"))
	if err != nil {
		t.Fatalf("Location: %v", err)
	}
	if got := loc.Query().Get("redirect_uri"); got != "https://broker.example/callback" {
		t.Fatalf("redirect_uri = %q, want the request's own https callback", got)
	}

	state := loc.Query().Get("state")
	if rec := get(t, srv, "/callback?code=c&state="+url.QueryEscape(state), nil); rec.Code != http.StatusOK {
		t.Fatalf("callback = %d: %s", rec.Code, rec.Body.String())
	}
	if got := stub.last().Get("redirect_uri"); got != "https://broker.example/callback" {
		t.Errorf("exchange redirect_uri = %q, want the URL the state carried", got)
	}
}

// TestTheTokenIsNeverLogged: the broker holds other people's credentials for a
// moment; a log line that quotes one is the leak that matters most.
func TestTheTokenIsNeverLogged(t *testing.T) {
	var logged bytes.Buffer
	srv, stub := newTestServer(t, slog.New(slog.NewTextHandler(&logged, nil)))
	stub.reply = `{"ok":true,"authed_user":{"id":"U1","access_token":"xoxp-1-2-never-log-me"},"team":{"name":"Acme"}}`

	rec := get(t, srv, "/callback?code=c&state="+url.QueryEscape(mustState(t, srv, "https://broker.example/callback")), nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("callback = %d: %s", rec.Code, rec.Body.String())
	}
	if strings.Contains(logged.String(), "never-log-me") {
		t.Errorf("the broker logged the token:\n%s", logged.String())
	}
	if !strings.Contains(logged.String(), "U1") {
		t.Errorf("the broker should still say who it issued a token to:\n%s", logged.String())
	}
}
