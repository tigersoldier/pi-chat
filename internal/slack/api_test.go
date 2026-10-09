package slack

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// TestCookieHeader is the difference between a working session and an
// `invalid_auth` nobody can explain: a `+` copied out of a browser's cookie pane
// is a space on the wire unless it is escaped, and some workspaces need the
// sibling cookie that only a whole header can carry.
func TestCookieHeader(t *testing.T) {
	cases := []struct {
		name, value, want string
	}{
		{"empty", "", ""},
		{"a bare value", "xoxd-abc", "d=xoxd-abc"},
		{"a value with a plus", "xoxd-ab+cd", "d=xoxd-ab%2Bcd"},
		{"an already-encoded value", "xoxd-ab%2Bcd", "d=xoxd-ab%2Bcd"},
		{"a whole header", "d=xoxd-abc; d-s=12345", "d=xoxd-abc; d-s=12345"},
		{"surrounding whitespace", "  xoxd-abc\n", "d=xoxd-abc"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := CookieHeader(tc.value); got != tc.want {
				t.Errorf("CookieHeader(%q) = %q, want %q", tc.value, got, tc.want)
			}
		})
	}
}

// TestAPIUsesItsOwnBaseAndCookie pins the self-DM's transport: the workspace's
// own host, a bearer token and the session cookie on the same request. The app
// surface is the other direction — slack.com, no cookie — and a client that
// mixed the two would authenticate as nobody.
func TestAPIUsesItsOwnBaseAndCookie(t *testing.T) {
	var (
		mu   sync.Mutex
		path string
		auth string
		ck   string
		body string
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		mu.Lock()
		path, auth, ck, body = r.URL.Path, r.Header.Get("Authorization"), r.Header.Get("Cookie"), string(raw)
		mu.Unlock()
		_, _ = io.WriteString(w, `{"ok":true,"user_id":"U1","team_id":"T1"}`)
	}))
	defer srv.Close()

	api := NewAPI("xoxc-test", discardLogger(),
		WithBaseURL(srv.URL+"/api"), WithCookie(CookieHeader("xoxd-ab+cd")))
	if _, err := api.AuthTest(t.Context()); err != nil {
		t.Fatalf("AuthTest: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if path != "/api/auth.test" {
		t.Errorf("request path = %q, want /api/auth.test (the workspace host, not slack.com)", path)
	}
	if auth != "Bearer xoxc-test" {
		t.Errorf("Authorization = %q, want the session token as a bearer token", auth)
	}
	if ck != "d=xoxd-ab%2Bcd" {
		t.Errorf("Cookie = %q, want the escaped session cookie", ck)
	}
	if strings.Contains(body, "token=") {
		t.Errorf("the token travelled in the body as well: %q", body)
	}
}
