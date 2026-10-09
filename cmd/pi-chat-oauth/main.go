// Command pi-chat-oauth is the small token broker for pi-chat's user-token
// self-DM mode (docs/slack-user-token-setup.md).
//
// One person — whoever owns the Slack app — runs this behind HTTPS and sends the
// install link to their team. Each person opens it once, approves the app on
// Slack's consent screen, and is shown a token of their own: an `xoxp-…` that
// acts as them and only them. They paste it into their own pi-chatd's
// `slack.self_dm.xoxp_file`, and from then on their daemon reads and writes
// their own "Notes to self" with no app, no bot and no shared secret.
//
// What it does and does not do, because this is a service that handles other
// people's credentials:
//
//   - It never stores a token. The authorization code is exchanged once, the
//     token is rendered into the page, and the page says so.
//   - It never logs a token, and the pages it serves are `no-store`, so a
//     credential is not left in a proxy cache or a browser history of the
//     response body.
//   - It cannot be used without the app's client secret: the exchange is the
//     confidential-client flow (`oauth/v2/access` with `user_scope`), so a
//     stolen code is worthless on its own.
//   - Whoever runs it does see each token as it is issued. That is inherent to
//     being the OAuth client; the alternative is PKCE with a localhost
//     callback, which Slack supports for public clients and which this service
//     deliberately is not.
//
// It is a reference implementation: one file, no dependencies beyond the
// standard library, meant to sit behind a reverse proxy that terminates TLS.
package main

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"html/template"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/tigersoldier/pi-chat/internal/slack"
)

// stateTTL bounds how long an install link stays valid. A link is a bearer
// invitation to authorize; it does not need to work a day later.
const stateTTL = 15 * time.Minute

// defaultScopes is what the self-DM surface needs from a person: read their
// direct messages, post in them as them, and resolve names.
var defaultScopes = []string{"im:history", "im:read", "im:write", "chat:write", "users:read"}

// settings is the broker's configuration, from flags or environment.
type settings struct {
	Addr         string
	ClientID     string
	ClientSecret string
	// RedirectURL is the public callback URL registered in the app. Empty means
	// "derive it from each request", which works behind a proxy that passes the
	// original Host and an `X-Forwarded-Proto: https` header.
	RedirectURL string
	Scopes      []string
	// SlackAPI and AuthorizeURL are overridable for tests and for Slack-
	// compatible hosts (GovSlack uses slack-gov.com).
	SlackAPI     string
	AuthorizeURL string
	Title        string
	// StateSecret keeps signed install links valid across restarts and across
	// more than one replica. Empty means "derive from the client secret", which
	// is fine for a single process.
	StateSecret string
}

// server is the broker's HTTP surface.
type server struct {
	cfg      settings
	log      *slog.Logger
	client   *http.Client
	stateKey []byte
	tmpl     *template.Template
}

// newServer validates the configuration and builds the broker.
func newServer(cfg settings, log *slog.Logger) (*server, error) {
	switch {
	case strings.TrimSpace(cfg.ClientID) == "":
		return nil, errors.New("no client id: pass -client-id or set SLACK_CLIENT_ID")
	case strings.TrimSpace(cfg.ClientSecret) == "":
		return nil, errors.New("no client secret: pass -client-secret or set SLACK_CLIENT_SECRET")
	}
	if len(cfg.Scopes) == 0 {
		cfg.Scopes = defaultScopes
	}
	if strings.TrimSpace(cfg.SlackAPI) == "" {
		cfg.SlackAPI = "https://slack.com/api/"
	}
	if strings.TrimSpace(cfg.AuthorizeURL) == "" {
		cfg.AuthorizeURL = "https://slack.com/oauth/v2/authorize"
	}
	if strings.TrimSpace(cfg.Title) == "" {
		cfg.Title = "pi-chat"
	}
	key := cfg.StateSecret
	if key == "" {
		key = cfg.ClientSecret
	}
	sum := sha256.Sum256([]byte("pi-chat-oauth\x00" + key))

	return &server{
		cfg:      cfg,
		log:      log,
		client:   &http.Client{Timeout: 20 * time.Second},
		stateKey: sum[:],
		tmpl:     template.Must(template.New("page").Parse(pageTemplate)),
	}, nil
}

// version is pi-chat-oauth's own version.
const version = "0.1.0-dev"

func main() {
	fs := flag.NewFlagSet("pi-chat-oauth", flag.ExitOnError)
	var (
		cfg         settings
		scopes      string
		showVersion = fs.Bool("version", false, "print the version and exit")
	)
	fs.StringVar(&cfg.Addr, "addr", ":8080", "listen address")
	fs.StringVar(&cfg.ClientID, "client-id", os.Getenv("SLACK_CLIENT_ID"), "Slack app client ID")
	fs.StringVar(&cfg.ClientSecret, "client-secret", os.Getenv("SLACK_CLIENT_SECRET"), "Slack app client secret")
	fs.StringVar(&cfg.RedirectURL, "redirect-url", "",
		"public callback URL registered in the app (default: derived from each request's Host)")
	fs.StringVar(&scopes, "scopes", strings.Join(defaultScopes, ","), "comma-separated user scopes to request")
	fs.StringVar(&cfg.SlackAPI, "slack-api", "", "Slack Web API base (default https://slack.com/api/)")
	fs.StringVar(&cfg.AuthorizeURL, "slack-authorize", "", "Slack authorize URL (default https://slack.com/oauth/v2/authorize)")
	fs.StringVar(&cfg.Title, "title", "", "service name shown on the pages (default pi-chat)")
	fs.StringVar(&cfg.StateSecret, "state-secret", "",
		"key for signing install links (default: derived from the client secret)")
	fs.Usage = func() {
		fmt.Fprintf(os.Stderr, "Usage: pi-chat-oauth [options]\n\n"+
			"Hands each person their own Slack user token for pi-chat's self-DM surface.\n"+
			"Run it behind HTTPS; see docs/slack-user-token-setup.md.\n\nOptions:\n")
		fs.PrintDefaults()
	}
	_ = fs.Parse(os.Args[1:])

	if *showVersion {
		fmt.Printf("pi-chat-oauth %s\n", version)
		return
	}
	for _, scope := range strings.Split(scopes, ",") {
		if scope = strings.TrimSpace(scope); scope != "" {
			cfg.Scopes = append(cfg.Scopes, scope)
		}
	}

	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
	srv, err := newServer(cfg, log)
	if err != nil {
		fmt.Fprintf(os.Stderr, "pi-chat-oauth: %v\n", err)
		os.Exit(1)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	httpSrv := &http.Server{
		Addr:              cfg.Addr,
		Handler:           srv.handler(),
		ReadHeaderTimeout: 10 * time.Second,
	}
	redirect := cfg.RedirectURL
	if redirect == "" {
		redirect = "derived from the request Host"
	}
	log.Info("pi-chat-oauth is listening",
		"addr", cfg.Addr, "callback", redirect, "scopes", strings.Join(cfg.Scopes, ","))

	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = httpSrv.Shutdown(shutdown)
	}()
	if err := httpSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		fmt.Fprintf(os.Stderr, "pi-chat-oauth: %v\n", err)
		os.Exit(1)
	}
}

// handler wires the routes. The paths are exact and the methods are checked in
// each handler, so a crawling browser or a probe cannot start an authorization.
func (s *server) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/", s.handleHome)
	mux.HandleFunc("/install", s.handleInstall)
	mux.HandleFunc("/callback", s.handleCallback)
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = io.WriteString(w, "ok\n")
	})
	return mux
}

// handleHome explains the service and links to the authorization.
func (s *server) handleHome(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	s.render(w, http.StatusOK, pageData{Title: s.cfg.Title, Scopes: strings.Join(s.cfg.Scopes, ", ")})
}

// handleInstall sends the browser to Slack's consent screen, with a signed
// state that carries the callback URL the exchange must repeat.
func (s *server) handleInstall(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	redirect := s.redirectURL(r)
	state, err := s.signState(redirect)
	if err != nil {
		s.log.Error("cannot sign an install link", "error", err)
		s.render(w, http.StatusInternalServerError, pageData{
			Title: s.cfg.Title,
			Error: "This broker cannot create a secure install link right now. Try again, and check its logs.",
		})
		return
	}
	query := url.Values{
		"client_id":    {s.cfg.ClientID},
		"user_scope":   {strings.Join(s.cfg.Scopes, ",")},
		"redirect_uri": {redirect},
		"state":        {state},
	}
	http.Redirect(w, r, s.cfg.AuthorizeURL+"?"+query.Encode(), http.StatusFound)
}

// handleCallback verifies the state, exchanges the code, and shows the token
// once.
func (s *server) handleCallback(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if denial := r.URL.Query().Get("error"); denial != "" {
		s.render(w, http.StatusBadRequest, pageData{
			Title: s.cfg.Title,
			Error: "Slack refused the authorization: " + denial + ". Nothing was granted; start again if that was not what you meant.",
		})
		return
	}

	redirect, err := s.checkState(r.URL.Query().Get("state"))
	if err != nil {
		s.render(w, http.StatusBadRequest, pageData{Title: s.cfg.Title, Error: err.Error()})
		return
	}
	code := strings.TrimSpace(r.URL.Query().Get("code"))
	if code == "" {
		s.render(w, http.StatusBadRequest, pageData{Title: s.cfg.Title, Error: "Slack returned no authorization code."})
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()
	grant, err := s.exchange(ctx, code, redirect)
	if err != nil {
		s.log.Warn("the token exchange failed", "error", err)
		s.render(w, http.StatusBadGateway, pageData{Title: s.cfg.Title, Error: err.Error()})
		return
	}
	s.log.Info("issued a user token for the self-DM surface",
		"user", grant.UserID, "team", grant.TeamName, "scopes", grant.Scopes)

	// A token pi-chatd cannot use (an expiring one, from an app with rotation on)
	// is diagnosed here rather than after somebody has pasted it and restarted.
	if err := slack.ValidateUserToken(grant.Token); err != nil {
		s.render(w, http.StatusOK, pageData{
			Title: s.cfg.Title,
			Warning: "Slack did grant access, but the token pi-chat cannot use it: " + err.Error() +
				". Do not paste it anywhere; fix the app setting and run this again.",
			UserID: grant.UserID, TeamName: grant.TeamName,
		})
		return
	}

	s.render(w, http.StatusOK, pageData{
		Title:    s.cfg.Title,
		Token:    grant.Token,
		UserID:   grant.UserID,
		TeamName: grant.TeamName,
		Scopes:   grant.Scopes,
		Config:   configSnippet(grant),
	})
}

// tokenGrant is what the exchange produced, without anything that is not shown
// to the person it belongs to.
type tokenGrant struct {
	Token      string
	UserID     string
	TeamID     string
	TeamName   string
	Enterprise string
	Scopes     string
}

// exchange turns an authorization code into the person's user token.
//
// The standard authorize endpoint with `user_scope` is deliberate: it is the
// confidential-client flow, so it works with an app that also has a bot user and
// needs no PKCE, and the token arrives under `authed_user` (Slack's own
// `v2_user` / `oauth.v2.user.access` flow is the public-client variant and is
// not needed here).
func (s *server) exchange(ctx context.Context, code, redirect string) (tokenGrant, error) {
	form := url.Values{
		"client_id":     {s.cfg.ClientID},
		"client_secret": {s.cfg.ClientSecret},
		"code":          {code},
		"redirect_uri":  {redirect},
	}
	endpoint := strings.TrimRight(s.cfg.SlackAPI, "/") + "/oauth.v2.access"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return tokenGrant{}, fmt.Errorf("build the exchange request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := s.client.Do(req)
	if err != nil {
		return tokenGrant{}, fmt.Errorf("reach Slack: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return tokenGrant{}, fmt.Errorf("read Slack's answer: %w", err)
	}

	var out struct {
		OK          bool   `json:"ok"`
		Error       string `json:"error"`
		AccessToken string `json:"access_token"`
		TokenType   string `json:"token_type"`
		Scope       string `json:"scope"`
		Team        struct {
			ID   string `json:"id"`
			Name string `json:"name"`
		} `json:"team"`
		Enterprise struct {
			ID   string `json:"id"`
			Name string `json:"name"`
		} `json:"enterprise"`
		AuthedUser struct {
			ID          string `json:"id"`
			Scope       string `json:"scope"`
			AccessToken string `json:"access_token"`
			TokenType   string `json:"token_type"`
		} `json:"authed_user"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return tokenGrant{}, fmt.Errorf("Slack answered with something unreadable (HTTP %d)", resp.StatusCode)
	}
	if !out.OK {
		code := out.Error
		if code == "" {
			code = "http_" + strconv.Itoa(resp.StatusCode)
		}
		return tokenGrant{}, fmt.Errorf("Slack refused the exchange: %s", code)
	}

	// A user-scope-only request puts the token under `authed_user`; accept a
	// top-level user token too, in case this app is configured user-scopes-only.
	token := strings.TrimSpace(out.AuthedUser.AccessToken)
	if token == "" && strings.HasPrefix(out.AccessToken, "xoxp-") {
		token = out.AccessToken
	}
	if token == "" {
		return tokenGrant{}, errors.New("the exchange returned no user token: check that the app declares the user scopes " +
			"and that this install link requests them")
	}
	scopes := out.AuthedUser.Scope
	if scopes == "" {
		scopes = out.Scope
	}
	teamName := out.Team.Name
	if out.Enterprise.Name != "" {
		teamName = out.Team.Name + " (" + out.Enterprise.Name + ")"
	}
	return tokenGrant{
		Token:      token,
		UserID:     out.AuthedUser.ID,
		TeamID:     out.Team.ID,
		TeamName:   teamName,
		Enterprise: out.Enterprise.ID,
		Scopes:     scopes,
	}, nil
}

// redirectURL is the callback the exchange must repeat exactly. When it is not
// configured it is derived from the request, which is why the state carries it:
// the callback cannot derive it again reliably behind a proxy.
func (s *server) redirectURL(r *http.Request) string {
	if s.cfg.RedirectURL != "" {
		return s.cfg.RedirectURL
	}
	scheme := "http"
	if r.TLS != nil || strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https") {
		scheme = "https"
	}
	return scheme + "://" + r.Host + "/callback"
}

// signState returns a signed, timestamped install token carrying the callback
// URL, so a forged or expired one is rejected before Slack is called. It fails
// rather than falling back to a weaker source when the system RNG is
// unavailable: a predictable nonce is not worth having.
func (s *server) signState(redirect string) (string, error) {
	nonce := make([]byte, 16)
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return "", fmt.Errorf("read a random nonce: %w", err)
	}
	payload := fmt.Sprintf("%d.%s.%s", time.Now().Unix(), hex.EncodeToString(nonce), redirect)
	return base64.RawURLEncoding.EncodeToString([]byte(payload)) + "." + s.mac(payload), nil
}

// checkState verifies a state and returns the callback URL it carried.
func (s *server) checkState(state string) (string, error) {
	parts := strings.Split(state, ".")
	if len(parts) != 2 {
		return "", errors.New("this link is not a pi-chat-oauth link; start from the broker's page")
	}
	raw, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return "", errors.New("this link is malformed; start from the broker's page")
	}
	signed, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return "", errors.New("this link is malformed; start from the broker's page")
	}
	mac := hmac.New(sha256.New, s.stateKey)
	mac.Write(raw)
	if !hmac.Equal(mac.Sum(nil), signed) {
		return "", errors.New("this link was not issued by this broker; start from its page")
	}
	fields := strings.SplitN(string(raw), ".", 3)
	if len(fields) != 3 {
		return "", errors.New("this link is malformed; start from the broker's page")
	}
	issued, err := strconv.ParseInt(fields[0], 10, 64)
	if err != nil {
		return "", errors.New("this link is malformed; start from the broker's page")
	}
	if age := time.Since(time.Unix(issued, 0)); age > stateTTL || age < -time.Minute {
		return "", errors.New("this install link has expired; open the broker's page and start again")
	}
	return fields[2], nil
}

func (s *server) mac(payload string) string {
	mac := hmac.New(sha256.New, s.stateKey)
	mac.Write([]byte(payload))
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

// pageData is everything the one template can show.
type pageData struct {
	Title    string
	Error    string
	Warning  string
	Token    string
	UserID   string
	TeamName string
	Scopes   string
	Config   string
}

// render writes a page with the headers that keep a credential out of caches,
// referrers and other people's pages.
func (s *server) render(w http.ResponseWriter, status int, data pageData) {
	if data.Title == "" {
		data.Title = s.cfg.Title
	}
	w.Header().Set("Cache-Control", "no-store, no-cache, must-revalidate, private")
	w.Header().Set("Pragma", "no-cache")
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Content-Security-Policy",
		"default-src 'none'; style-src 'unsafe-inline'; script-src 'unsafe-inline'; base-uri 'none'; form-action 'none'")
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	if err := s.tmpl.Execute(w, data); err != nil {
		s.log.Error("cannot render a page", "error", err)
	}
}

// configSnippet is the configuration the person pastes, with their own member
// id filled in so the allowlist step cannot be forgotten.
func configSnippet(grant tokenGrant) string {
	return fmt.Sprintf(`[slack.access]
allowed_users = [%q]

[slack.self_dm]
enabled       = true
auth          = "user_oauth"
xoxp_file     = "~/.config/pi-chat/slack-xoxp"
channel_id    = "D…"      # open "Notes to self" and read D… from its URL
poll_interval = "5s"
`, grant.UserID)
}

// pageTemplate is the whole UI. No external assets, so the Content-Security-
// Policy above can stay closed, and one page shape for home, success and error.
const pageTemplate = `<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<meta name="referrer" content="no-referrer">
<title>{{.Title}} — Slack access</title>
<style>
  :root { color-scheme: light dark; }
  body { font: 16px/1.55 system-ui, -apple-system, "Segoe UI", sans-serif; margin: 0; padding: 3rem 1rem; }
  main { max-width: 44rem; margin: 0 auto; }
  h1 { font-size: 1.5rem; margin: 0 0 .5rem; }
  p { margin: .6rem 0; }
  .card { border: 1px solid currentColor; border-radius: .5rem; padding: 1rem 1.25rem; margin: 1.25rem 0; }
  .error { border-color: #b3261e; }
  .warn { border-color: #946200; }
  code, pre, textarea { font-family: ui-monospace, SFMono-Regular, Menlo, monospace; font-size: .875rem; }
  textarea { width: 100%; min-height: 5.5rem; resize: vertical; }
  pre { overflow-x: auto; padding: .75rem; border-radius: .375rem; background: rgba(127,127,127,.12); }
  ol, ul { padding-left: 1.25rem; }
  li { margin: .3rem 0; }
  .muted { opacity: .75; font-size: .9rem; }
  a.button { display: inline-block; padding: .6rem 1rem; border: 1px solid currentColor; border-radius: .375rem; text-decoration: none; font-weight: 600; }
  button { font: inherit; padding: .4rem .8rem; margin-top: .4rem; }
</style>
</head>
<body>
<main>
<h1>{{.Title}}: Slack access for your own pi session</h1>

{{if .Error}}
  <div class="card error"><p><strong>That did not work.</strong></p><p>{{.Error}}</p>
  <p class="muted">Nothing was granted. You can close this page or start again from the broker's link.</p></div>
{{end}}

{{if .Warning}}
  <div class="card warn"><p><strong>Access was granted, but the token cannot be used.</strong></p><p>{{.Warning}}</p>
  <p class="muted">Connected as {{.UserID}}{{if .TeamName}} in {{.TeamName}}{{end}}.</p></div>
{{end}}

{{if .Token}}
  <div class="card">
    <p>Connected as <strong>{{.UserID}}</strong>{{if .TeamName}} in <strong>{{.TeamName}}</strong>{{end}}.</p>
    <p>This is your personal token. It is shown once — the broker does not store it.</p>
    <textarea id="token" readonly>{{.Token}}</textarea>
    <button type="button" onclick="copyToken()">Copy token</button>
    {{if .Scopes}}<p class="muted">Scopes: {{.Scopes}}</p>{{end}}
  </div>

  <h2>Put it in your pi-chat configuration</h2>
  <ol>
    <li>Save the token where only you can read it:<br>
      <code>umask 077; cat &gt; ~/.config/pi-chat/slack-xoxp</code> (paste, then Ctrl-D)</li>
    <li>Add this to <code>~/.config/pi-chat/config.toml</code>:</li>
  </ol>
  <pre>{{.Config}}</pre>
  <ol start="3">
    <li>Check and restart: <code>make check &amp;&amp; systemctl --user restart pi-chatd</code></li>
    <li>In Slack, open <em>Notes to self</em> and type <code>/pi help</code>.</li>
  </ol>
{{end}}

{{if and (not .Token) (not .Error) (not .Warning)}}
  <p>This service hands out a personal Slack token for <strong>{{.Title}}</strong>: one token per person,
  acting as that person only. Whoever runs it owns the Slack app; nothing about it is shared with anyone
  else's daemon.</p>
  <p><a class="button" href="/install">Continue to Slack</a></p>
  <p class="muted">Slack will ask you to approve: {{.Scopes}}.
  The token is shown once, here, and never stored by this service.</p>
{{end}}

<p class="muted">pi-chat-oauth · the token this page shows is a credential: keep it 0600 and revoke it in
Slack if it leaks.</p>
<script>
function copyToken() {
  var el = document.getElementById('token');
  el.select();
  try { navigator.clipboard.writeText(el.value); } catch (e) { /* select() already helps */ }
}
</script>
</main>
</body>
</html>
`
