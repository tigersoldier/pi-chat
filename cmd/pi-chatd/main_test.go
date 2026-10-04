package main

import (
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tigersoldier/pi-chat/internal/config"
)

// TestDisableOrFail pins the startup policy for a configured surface that cannot
// run: with another surface up it is left out and reported, so a stale cookie
// cannot take the app down or make systemd flap; alone it is the daemon's error,
// because there would be nothing to serve.
func TestDisableOrFail(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	stale := errors.New("the Slack session cookie or xoxc token is stale")

	if err := disableOrFail(false, log, stale); !errors.Is(err, stale) {
		t.Errorf("with no other surface, disableOrFail = %v, want the error", err)
	}
	if err := disableOrFail(true, log, stale); err != nil {
		t.Errorf("with another surface up, disableOrFail = %v, want nil (the daemon keeps serving)", err)
	}
}

// writeToken writes a credential file the way the daemon reads one.
func writeToken(t *testing.T, name, value string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, []byte(value+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// TestSelfDMAPIUsesTheConfiguredCredentialMode: the mode decides which files are
// read. user_oauth needs the token alone; session needs the pair.
func TestSelfDMAPIUsesTheConfiguredCredentialMode(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))

	cfg := &config.Config{}
	cfg.Slack.SelfDM = config.SelfDM{
		Auth:     config.SelfDMAuthUserOAuth,
		XOXPFile: writeToken(t, "slack-xoxp", "xoxp-1-2-abcdef"),
	}
	if api, err := selfDMAPI(cfg, log); err != nil || api == nil {
		t.Fatalf("user_oauth mode = (%v, %v), want a client", api, err)
	}

	cfg = &config.Config{}
	cfg.Slack.SelfDM = config.SelfDM{
		Auth:         config.SelfDMAuthSession,
		XOXCFile:     writeToken(t, "slack-xoxc", "xoxc-1-2-abcdef"),
		XOXDFile:     writeToken(t, "slack-xoxd", "xoxd-abcdef"),
		WorkspaceURL: "https://acme.slack.com",
	}
	if api, err := selfDMAPI(cfg, log); err != nil || api == nil {
		t.Fatalf("session mode = (%v, %v), want a client", api, err)
	}
}

// TestSelfDMAPIRefusesTheWrongKindOfCredential: a credential that is not a user
// token must fail at startup with a message that names the mistake, instead of
// becoming an `invalid_auth` a long way from the file somebody pasted into.
func TestSelfDMAPIRefusesTheWrongKindOfCredential(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	cases := []struct {
		name, token, want string
	}{
		{"a bot token", "xoxb-1-2-abcdef", "bot token"},
		{"a session token", "xoxc-1-2-abcdef", "session token"},
		{"a rotating token", "xoxe.xoxp-1-abcdef", "rotation"},
		{"a truncated paste", "xoxp", "does not look like"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := &config.Config{}
			cfg.Slack.SelfDM = config.SelfDM{
				Auth:     config.SelfDMAuthUserOAuth,
				XOXPFile: writeToken(t, "slack-xoxp", tc.token),
			}
			_, err := selfDMAPI(cfg, log)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("selfDMAPI with %s = %v, want an error mentioning %q", tc.name, err, tc.want)
			}
			if strings.Contains(err.Error(), tc.token) && len(tc.token) > 8 {
				t.Errorf("the error echoed the credential: %v", err)
			}
		})
	}
}

// TestSelfDMAPIWantsTheCredentialsItsModeUses: each mode reads its own files, so
// a missing one names the file rather than failing somewhere later.
func TestSelfDMAPIWantsTheCredentialsItsModeUses(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	missing := filepath.Join(t.TempDir(), "nope")

	if _, err := selfDMAPI(&config.Config{Slack: config.Slack{SelfDM: config.SelfDM{
		Auth: config.SelfDMAuthUserOAuth, XOXPFile: missing,
	}}}, log); err == nil || !strings.Contains(err.Error(), "xoxp") {
		t.Errorf("a missing user token = %v, want an error naming the token file", err)
	}
	if _, err := selfDMAPI(&config.Config{Slack: config.Slack{SelfDM: config.SelfDM{
		Auth: config.SelfDMAuthSession, XOXCFile: missing, XOXDFile: missing,
	}}}, log); err == nil {
		t.Error("a missing session pair must fail")
	}
}
