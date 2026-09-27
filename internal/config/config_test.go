package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeConfig writes content to a file in a temporary config directory and
// returns its path.
func writeConfig(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// home points HOME at a temporary directory so "~" expansion is deterministic.
func home(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	return dir
}

func TestDefaultsAreValid(t *testing.T) {
	home(t)
	cfg := Defaults()
	if err := cfg.expandPaths(); err != nil {
		t.Fatal(err)
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("defaults must validate: %v", err)
	}
}

func TestLoadAppliesDefaultsForMissingKeys(t *testing.T) {
	home(t)
	path := writeConfig(t, "[slack.access]\nallowed_users = [\"U123\"]\n")

	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Concurrency.MaxWarmSessions != DefaultMaxWarmSessions {
		t.Errorf("max_warm_sessions = %d, want the default %d",
			cfg.Concurrency.MaxWarmSessions, DefaultMaxWarmSessions)
	}
	if cfg.Render.Mode != "stream" {
		t.Errorf("render.mode = %q, want \"stream\"", cfg.Render.Mode)
	}
	if got, want := len(cfg.Gateway.PiArgs), 1; got != want || cfg.Gateway.PiArgs[0] != "--approve" {
		t.Errorf("pi_args = %v, want [--approve]", cfg.Gateway.PiArgs)
	}
	if len(cfg.Slack.Access.AllowedUsers) != 1 {
		t.Errorf("allowed_users = %v, want one entry", cfg.Slack.Access.AllowedUsers)
	}
	if cfg.Path() != path {
		t.Errorf("Path() = %q, want %q", cfg.Path(), path)
	}
}

func TestLoadOverridesDefaultsAndExpandsHome(t *testing.T) {
	dir := home(t)
	path := writeConfig(t, strings.Join([]string{
		"[concurrency]",
		"max_warm_sessions = 3",
		"[paths]",
		"projects_root = \"~/projects\"",
		"repos_root = \"/srv/code\"",
		"[render]",
		"mode = \"patch\"",
		"flush_ms = 250",
		"",
	}, "\n"))

	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Concurrency.MaxWarmSessions != 3 {
		t.Errorf("max_warm_sessions = %d, want 3", cfg.Concurrency.MaxWarmSessions)
	}
	if want := filepath.Join(dir, "projects"); cfg.Paths.ProjectsRoot != want {
		t.Errorf("projects_root = %q, want %q", cfg.Paths.ProjectsRoot, want)
	}
	if cfg.Paths.ReposRoot != "/srv/code" {
		t.Errorf("repos_root = %q, want /srv/code", cfg.Paths.ReposRoot)
	}
	if cfg.Render.Mode != "patch" || cfg.Render.FlushMS != 250 {
		t.Errorf("render = %+v, want patch/250", cfg.Render)
	}
	// Untouched keys keep their defaults, including the ones that carry "~".
	if cfg.Gateway.AdminTokenFile == "" || strings.HasPrefix(cfg.Gateway.AdminTokenFile, "~") {
		t.Errorf("admin_token_file = %q, want an expanded absolute path", cfg.Gateway.AdminTokenFile)
	}
}

func TestLoadRejectsUnknownKeys(t *testing.T) {
	home(t)
	path := writeConfig(t, "[concurrency]\nmax_warm_session = 4\n")

	_, err := Load(path)
	if err == nil {
		t.Fatal("a misspelled key must fail loudly, not fall back to a default")
	}
	if !strings.Contains(err.Error(), "max_warm_session") {
		t.Errorf("error should name the key, got: %v", err)
	}
}

// The allowlist belongs to the platform: a top-level [access] section is the
// shape this file used before Slack became one integration among several, and
// silently ignoring it would deny (or allow) the wrong people.
func TestLoadRejectsTopLevelAccessSection(t *testing.T) {
	home(t)
	path := writeConfig(t, "[access]\nallowed_users = [\"U123\"]\n")

	_, err := Load(path)
	if err == nil {
		t.Fatal("a top-level [access] section must be rejected, not ignored")
	}
	if !strings.Contains(err.Error(), "access") {
		t.Errorf("error should name the key, got: %v", err)
	}
}

func TestValidateRejectsBadValues(t *testing.T) {
	home(t)
	cases := []struct {
		name string
		body string
		want string
	}{
		{"zero cap", "[concurrency]\nmax_warm_sessions = 0\n", "max_warm_sessions"},
		{"unknown eviction", "[concurrency]\neviction = \"kill\"\n", "eviction"},
		{"unknown render mode", "[render]\nmode = \"blocks\"\n", "render.mode"},
		{"unknown approvals", "[behavior]\napprovals = \"never\"\n", "approvals"},
		{"unknown log level", "[log]\nlevel = \"trace\"\n", "log.level"},
		{"unknown log format", "[log]\nformat = \"yaml\"\n", "log.format"},
		{"relative path", "[paths]\nrepos_root = \"code\"\n", "absolute"},
		{"empty gateway dir", "[gateway]\nstate_dir = \"\"\n", "state_dir"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Load(writeConfig(t, tc.body))
			if err == nil {
				t.Fatalf("expected an error mentioning %q", tc.want)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error should mention %q, got: %v", tc.want, err)
			}
		})
	}
}

func TestLoadMissingFileNamesThePath(t *testing.T) {
	home(t)
	missing := filepath.Join(t.TempDir(), "nope.toml")
	_, err := Load(missing)
	if err == nil {
		t.Fatal("want an error")
	}
	if !strings.Contains(err.Error(), missing) {
		t.Errorf("error should name the file, got: %v", err)
	}
}

func TestEmptyAllowlistIsValidButDeniesEveryone(t *testing.T) {
	home(t)
	cfg, err := Load(writeConfig(t, ""))
	if err != nil {
		t.Fatalf("an empty allowlist is a valid, if useless, configuration: %v", err)
	}
	if len(cfg.Slack.Access.AllowedUsers) != 0 {
		t.Fatalf("allowed_users = %v, want empty", cfg.Slack.Access.AllowedUsers)
	}
	joined := strings.Join(cfg.Summary(), "\n")
	if !strings.Contains(joined, "every request is denied") {
		t.Errorf("the summary must say nobody is allowed, got:\n%s", joined)
	}
}

func TestSummaryNeverPrintsTokenContents(t *testing.T) {
	dir := home(t)
	secret := "xoxb-supersecret-value"
	tokenFile := filepath.Join(dir, "slack-bot-token")
	if err := os.WriteFile(tokenFile, []byte(secret), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(writeConfig(t, "[slack]\nbot_token_file = \""+tokenFile+"\"\n"))
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(cfg.Summary(), "\n")
	if strings.Contains(joined, "supersecret") {
		t.Fatalf("the summary leaked a token value:\n%s", joined)
	}
	if !strings.Contains(joined, "present") {
		t.Errorf("the summary should report that the file exists, got:\n%s", joined)
	}
}

func TestDefaultPathHonoursXDGConfigHome(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)
	if got, want := DefaultPath(), filepath.Join(dir, "pi-chat", "config.toml"); got != want {
		t.Errorf("DefaultPath() = %q, want %q", got, want)
	}
	t.Setenv("XDG_CONFIG_HOME", "")
	t.Setenv("HOME", dir)
	if got, want := DefaultPath(), filepath.Join(dir, ".config", "pi-chat", "config.toml"); got != want {
		t.Errorf("DefaultPath() = %q, want %q", got, want)
	}
}
