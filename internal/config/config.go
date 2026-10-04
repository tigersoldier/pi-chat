// Package config loads pi-chat's configuration file.
//
// The shape and the meaning of every field are specified in DESIGN.md §11; this
// package is the single place that knows the file layout, the defaults, and
// what counts as a valid configuration.
package config

import (
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/BurntSushi/toml"
)

// Defaults, also used as documentation by the example file.
const (
	DefaultGatewayStateDir   = "~/.config/pi-gateway"
	DefaultAdminTokenFile    = "~/.config/pi-chat/gateway-admin.token"
	DefaultThreadTokenFile   = "~/.config/pi-chat/gateway-thread.token"
	DefaultSlackAppTokenFile = "~/.config/pi-chat/slack-app-token"
	DefaultSlackBotTokenFile = "~/.config/pi-chat/slack-bot-token"
	DefaultSelfDMXOXCFile    = "~/.config/pi-chat/slack-xoxc"
	DefaultSelfDMXOXDFile    = "~/.config/pi-chat/slack-xoxd"
	DefaultSelfDMXOXPFile    = "~/.config/pi-chat/slack-xoxp"
	DefaultSelfDMPollPeriod  = "5s"
	DefaultProjectsRoot      = "~/work"
	DefaultReposRoot         = "~/code"
	DefaultDBPath            = "~/.local/state/pi-chat/pi-chat.db"
	DefaultMaxWarmSessions   = 8
	DefaultIdleCloseMinutes  = 10
	DefaultAdmissionSeconds  = 120
	DefaultFlushMS           = 1000
)

// Config is the whole configuration file.
type Config struct {
	Slack       Slack       `toml:"slack"`
	Gateway     Gateway     `toml:"gateway"`
	Paths       Paths       `toml:"paths"`
	Concurrency Concurrency `toml:"concurrency"`
	Render      Render      `toml:"render"`
	Behavior    Behavior    `toml:"behavior"`
	Log         Log         `toml:"log"`

	path string
}

// Slack holds the credentials of both Slack surfaces and the Slack-specific
// access rules.
//
// The app surface is the Socket Mode integration (DESIGN.md §2, §12). It is
// optional: with Enabled false, or with either token file missing, it is simply
// not started, and the self-DM surface — when it is configured — runs alone.
type Slack struct {
	// Enabled is a pointer so an absent key means "on": the app surface is what
	// this file had before it became optional, and a default of false would
	// silently disable a working install on upgrade.
	Enabled      *bool  `toml:"enabled"`
	AppTokenFile string `toml:"app_token_file"`
	BotTokenFile string `toml:"bot_token_file"`
	Access       Access `toml:"access"`
	SelfDM       SelfDM `toml:"self_dm"`
}

// AppEnabled reports whether the app surface is asked to run. Whether it *can*
// is decided at startup, from whether its two token files exist.
func (s Slack) AppEnabled() bool { return s.Enabled == nil || *s.Enabled }

// SelfDM configures the self-DM surface: the Slack conversation you have with
// yourself ("Notes to self"), driven without the app surface's bot identity
// (DESIGN.md §12).
//
// It is off by default, and it is deliberately all-or-nothing: an install that
// asks for it and then cannot authenticate fails loudly rather than running
// without the surface it was configured for.
//
// Two credential modes are supported, chosen by Auth:
//
//   - "session" (the default): your own browser session — an `xoxc-…` token and
//     the `d` cookie — pasted from DevTools. No app involvement; the credentials
//     are unofficial and account-wide.
//   - "user_oauth": an `xoxp-…` token minted by the team's Slack app through
//     OAuth, one per person. Sanctioned, scoped and revocable; see
//     docs/slack-user-token-setup.md.
type SelfDM struct {
	Enabled      bool   `toml:"enabled"`
	Auth         string `toml:"auth"`          // "session" | "user_oauth"
	XOXCFile     string `toml:"xoxc_file"`     // session mode: xoxc-… session token
	XOXDFile     string `toml:"xoxd_file"`     // session mode: the `d` cookie value (or a whole Cookie header)
	XOXPFile     string `toml:"xoxp_file"`     // user_oauth mode: xoxp-… user token
	WorkspaceURL string `toml:"workspace_url"` // session mode: https://acme.slack.com
	ChannelID    string `toml:"channel_id"`    // the self-DM's D… conversation id
	PollInterval string `toml:"poll_interval"` // "5s"; how often the DM is read
}

// The two credential modes for the self-DM surface.
const (
	SelfDMAuthSession   = "session"
	SelfDMAuthUserOAuth = "user_oauth"
)

// UserOAuth reports whether the surface authenticates with an OAuth user token
// instead of a browser session.
func (s SelfDM) UserOAuth() bool { return s.Auth == SelfDMAuthUserOAuth }

// AuthDescription names the credential mode in one phrase, for logs and `--check`.
func (s SelfDM) AuthDescription() string {
	if s.UserOAuth() {
		return "user OAuth token"
	}
	return "session credentials"
}

// PollEvery is the configured poll interval, defaulting when it was not parsed.
func (s SelfDM) PollEvery() time.Duration {
	d, err := time.ParseDuration(strings.TrimSpace(s.PollInterval))
	if err != nil || d <= 0 {
		return 5 * time.Second
	}
	return d
}

// APIBase is the Web API root for this workspace. Slack's client talks to the
// workspace's own host — not slack.com — and its session token is only valid
// there.
func (s SelfDM) APIBase() string {
	base := strings.TrimRight(strings.TrimSpace(s.WorkspaceURL), "/")
	if base == "" {
		return ""
	}
	return base + "/api/"
}

// Access is a platform's deny-by-default allowlist (DESIGN.md §10). It lives
// under the platform's own section because each integration has a different
// identity model: Slack uses member and channel IDs, Google Chat would use
// email addresses and space names.
type Access struct {
	AllowedUsers    []string `toml:"allowed_users"`
	AllowedChannels []string `toml:"allowed_channels"`
}

// Gateway points at pi-gatewayd and the two tokens pi-chat uses.
type Gateway struct {
	StateDir        string   `toml:"state_dir"`
	AdminTokenFile  string   `toml:"admin_token_file"`
	ThreadTokenFile string   `toml:"thread_token_file"`
	PiArgs          []string `toml:"pi_args"`
}

// Paths are the workspace convention (DESIGN.md §9).
type Paths struct {
	ProjectsRoot string `toml:"projects_root"`
	ReposRoot    string `toml:"repos_root"`
	DBPath       string `toml:"db_path"`
}

// Concurrency bounds warm pi processes (DESIGN.md §8).
type Concurrency struct {
	MaxWarmSessions        int    `toml:"max_warm_sessions"`
	ThreadIdleCloseMinutes int    `toml:"thread_idle_close_minutes"`
	AdmissionWaitSeconds   int    `toml:"admission_wait_seconds"`
	Eviction               string `toml:"eviction"`
}

// Render selects the Slack rendering path (DESIGN.md §6).
type Render struct {
	Mode    string `toml:"mode"` // stream | patch
	FlushMS int    `toml:"flush_ms"`
}

// Behavior is agent-side policy.
type Behavior struct {
	Approvals      string `toml:"approvals"` // auto | interactive
	InjectedPrompt string `toml:"injected_prompt"`
}

// Log configures the logger.
type Log struct {
	Level  string `toml:"level"`  // debug | info | warn | error
	Format string `toml:"format"` // text | json
}

// ReadToken reads a token file. Tokens are stored one per file so they never
// appear in the configuration, in shell history, or in a process listing.
// Surrounding whitespace is tolerated because it is easy to add by accident
// and impossible to see.
func ReadToken(path string) (string, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("read token %s: %w", path, err)
	}
	token := strings.TrimSpace(string(raw))
	if token == "" {
		return "", fmt.Errorf("token %s is empty", path)
	}
	return token, nil
}

// DefaultPath is the configuration file's location, honouring XDG_CONFIG_HOME.
func DefaultPath() string {
	if dir := os.Getenv("XDG_CONFIG_HOME"); dir != "" {
		return filepath.Join(dir, "pi-chat", "config.toml")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "config.toml"
	}
	return filepath.Join(home, ".config", "pi-chat", "config.toml")
}

// Defaults returns a configuration with every default applied. Load starts
// from this and lets the file override what it mentions.
func Defaults() *Config {
	return &Config{
		Slack: Slack{
			AppTokenFile: DefaultSlackAppTokenFile,
			BotTokenFile: DefaultSlackBotTokenFile,
			SelfDM: SelfDM{
				Auth:         SelfDMAuthSession,
				XOXCFile:     DefaultSelfDMXOXCFile,
				XOXDFile:     DefaultSelfDMXOXDFile,
				XOXPFile:     DefaultSelfDMXOXPFile,
				PollInterval: DefaultSelfDMPollPeriod,
			},
		},
		Gateway: Gateway{
			StateDir:        DefaultGatewayStateDir,
			AdminTokenFile:  DefaultAdminTokenFile,
			ThreadTokenFile: DefaultThreadTokenFile,
			// PiArgs starts empty: --approve is added by the approvals policy in
			// [behavior], so a default here would duplicate it (and hard-code a
			// policy into a list meant for the user's own arguments).
		},
		Paths: Paths{
			ProjectsRoot: DefaultProjectsRoot,
			ReposRoot:    DefaultReposRoot,
			DBPath:       DefaultDBPath,
		},
		Concurrency: Concurrency{
			MaxWarmSessions:        DefaultMaxWarmSessions,
			ThreadIdleCloseMinutes: DefaultIdleCloseMinutes,
			AdmissionWaitSeconds:   DefaultAdmissionSeconds,
			Eviction:               "evict-then-queue",
		},
		Render:   Render{Mode: "stream", FlushMS: DefaultFlushMS},
		Behavior: Behavior{Approvals: "auto"},
		Log:      Log{Level: "info", Format: "text"},
	}
}

// Path reports the file the configuration was loaded from.
func (c *Config) Path() string { return c.path }

// Load reads, defaults, expands and validates a configuration file. An empty
// path means DefaultPath.
func Load(path string) (*Config, error) {
	if path == "" {
		path = DefaultPath()
	}
	cfg := Defaults()
	md, err := toml.DecodeFile(path, cfg)
	if err != nil {
		return nil, fmt.Errorf("config %s: %w", path, err)
	}
	// An unrecognised key is nearly always a typo that would otherwise be
	// silently ignored and leave a default in place.
	if undecoded := md.Undecoded(); len(undecoded) > 0 {
		keys := make([]string, 0, len(undecoded))
		for _, k := range undecoded {
			keys = append(keys, k.String())
		}
		return nil, fmt.Errorf("config %s: unknown key(s): %s", path, strings.Join(keys, ", "))
	}
	cfg.path = path
	if err := cfg.expandPaths(); err != nil {
		return nil, fmt.Errorf("config %s: %w", path, err)
	}
	cfg.normalize()
	if err := cfg.Validate(); err != nil {
		return nil, fmt.Errorf("config %s: %w", path, err)
	}
	return cfg, nil
}

// Validate reports anything absent or contradictory.
func (c *Config) Validate() error {
	var errs []error
	require := func(what, value string) {
		if strings.TrimSpace(value) == "" {
			errs = append(errs, fmt.Errorf("%s must not be empty", what))
		}
	}
	// The app surface still needs its two file *paths* when it is enabled; whether
	// the files exist is a startup question, not a configuration one, because a
	// missing token now disables the surface instead of failing the daemon.
	if c.Slack.AppEnabled() {
		require("slack.app_token_file", c.Slack.AppTokenFile)
		require("slack.bot_token_file", c.Slack.BotTokenFile)
	}
	if !c.Slack.AppEnabled() && !c.Slack.SelfDM.Enabled {
		errs = append(errs, errors.New(
			"slack: no surface is enabled — turn on slack.enabled (the app) or slack.self_dm.enabled (the self-DM)"))
	}
	if sd := c.Slack.SelfDM; sd.Enabled {
		require("slack.self_dm.channel_id", sd.ChannelID)
		switch sd.Auth {
		case SelfDMAuthSession:
			require("slack.self_dm.xoxc_file", sd.XOXCFile)
			require("slack.self_dm.xoxd_file", sd.XOXDFile)
			require("slack.self_dm.workspace_url", sd.WorkspaceURL)
		case SelfDMAuthUserOAuth:
			require("slack.self_dm.xoxp_file", sd.XOXPFile)
		default:
			errs = append(errs, fmt.Errorf(
				"slack.self_dm.auth must be %q or %q, got %q",
				SelfDMAuthSession, SelfDMAuthUserOAuth, sd.Auth))
		}
		if sd.ChannelID != "" && !strings.HasPrefix(sd.ChannelID, "D") {
			errs = append(errs, fmt.Errorf(
				"slack.self_dm.channel_id must be the self-DM's D… conversation id, got %q", sd.ChannelID))
		}
		if d := sd.PollEvery(); d < time.Second {
			errs = append(errs, fmt.Errorf(
				"slack.self_dm.poll_interval must be at least 1s, got %q", sd.PollInterval))
		} else if _, err := time.ParseDuration(strings.TrimSpace(sd.PollInterval)); err != nil {
			errs = append(errs, fmt.Errorf("slack.self_dm.poll_interval: %v", err))
		}
		if sd.WorkspaceURL != "" {
			if u, err := url.Parse(sd.WorkspaceURL); err != nil || u.Scheme != "https" || u.Host == "" {
				errs = append(errs, fmt.Errorf(
					"slack.self_dm.workspace_url must be an https workspace URL like https://acme.slack.com, got %q",
					sd.WorkspaceURL))
			}
		}
	}
	require("gateway.state_dir", c.Gateway.StateDir)
	require("gateway.admin_token_file", c.Gateway.AdminTokenFile)
	require("gateway.thread_token_file", c.Gateway.ThreadTokenFile)
	require("paths.projects_root", c.Paths.ProjectsRoot)
	require("paths.repos_root", c.Paths.ReposRoot)
	require("paths.db_path", c.Paths.DBPath)

	for _, p := range []struct{ what, value string }{
		{"slack.app_token_file", c.Slack.AppTokenFile},
		{"slack.bot_token_file", c.Slack.BotTokenFile},
		{"slack.self_dm.xoxc_file", c.Slack.SelfDM.XOXCFile},
		{"slack.self_dm.xoxd_file", c.Slack.SelfDM.XOXDFile},
		{"slack.self_dm.xoxp_file", c.Slack.SelfDM.XOXPFile},
		{"gateway.state_dir", c.Gateway.StateDir},
		{"gateway.admin_token_file", c.Gateway.AdminTokenFile},
		{"gateway.thread_token_file", c.Gateway.ThreadTokenFile},
		{"paths.projects_root", c.Paths.ProjectsRoot},
		{"paths.repos_root", c.Paths.ReposRoot},
		{"paths.db_path", c.Paths.DBPath},
	} {
		if p.value != "" && !filepath.IsAbs(p.value) {
			errs = append(errs, fmt.Errorf("%s must be an absolute path, got %q", p.what, p.value))
		}
	}

	if c.Concurrency.MaxWarmSessions < 1 {
		errs = append(errs, fmt.Errorf("concurrency.max_warm_sessions must be at least 1, got %d",
			c.Concurrency.MaxWarmSessions))
	}
	if c.Concurrency.ThreadIdleCloseMinutes < 1 {
		errs = append(errs, fmt.Errorf("concurrency.thread_idle_close_minutes must be at least 1, got %d",
			c.Concurrency.ThreadIdleCloseMinutes))
	}
	if c.Concurrency.AdmissionWaitSeconds < 1 {
		errs = append(errs, fmt.Errorf("concurrency.admission_wait_seconds must be at least 1, got %d",
			c.Concurrency.AdmissionWaitSeconds))
	}
	if c.Concurrency.Eviction != "evict-then-queue" {
		errs = append(errs, fmt.Errorf("concurrency.eviction must be \"evict-then-queue\", got %q",
			c.Concurrency.Eviction))
	}
	if c.Render.Mode != "stream" && c.Render.Mode != "patch" {
		errs = append(errs, fmt.Errorf("render.mode must be \"stream\" or \"patch\", got %q", c.Render.Mode))
	}
	if c.Render.FlushMS < 1 {
		errs = append(errs, fmt.Errorf("render.flush_ms must be at least 1, got %d", c.Render.FlushMS))
	}
	if c.Behavior.Approvals != "auto" && c.Behavior.Approvals != "interactive" {
		errs = append(errs, fmt.Errorf("behavior.approvals must be \"auto\" or \"interactive\", got %q",
			c.Behavior.Approvals))
	}
	switch c.Log.Level {
	case "debug", "info", "warn", "error":
	default:
		errs = append(errs, fmt.Errorf("log.level must be debug, info, warn or error, got %q", c.Log.Level))
	}
	if c.Log.Format != "text" && c.Log.Format != "json" {
		errs = append(errs, fmt.Errorf("log.format must be \"text\" or \"json\", got %q", c.Log.Format))
	}
	return errors.Join(errs...)
}

// expandPaths resolves "~" in every path-valued field.
func (c *Config) expandPaths() error {
	home, err := os.UserHomeDir()
	if err != nil {
		return fmt.Errorf("cannot resolve the home directory: %w", err)
	}
	for _, p := range []*string{
		&c.Slack.AppTokenFile, &c.Slack.BotTokenFile,
		&c.Slack.SelfDM.XOXCFile, &c.Slack.SelfDM.XOXDFile, &c.Slack.SelfDM.XOXPFile,
		&c.Gateway.StateDir, &c.Gateway.AdminTokenFile, &c.Gateway.ThreadTokenFile,
		&c.Paths.ProjectsRoot, &c.Paths.ReposRoot, &c.Paths.DBPath,
	} {
		*p = expand(*p, home)
	}
	return nil
}

// normalize fixes up values that have one obvious reading: a workspace URL
// written as a bare host gets its scheme, and a trailing slash goes, so
// APIBase does not have to be defensive.
func (c *Config) normalize() {
	if c.Slack.SelfDM.WorkspaceURL == "" {
		return
	}
	raw := strings.TrimSpace(c.Slack.SelfDM.WorkspaceURL)
	if !strings.Contains(raw, "://") {
		raw = "https://" + raw
	}
	c.Slack.SelfDM.WorkspaceURL = strings.TrimRight(raw, "/")
}

// expand resolves a leading "~" and cleans the result.
func expand(path, home string) string {
	if path == "" {
		return ""
	}
	switch {
	case path == "~":
		return home
	case strings.HasPrefix(path, "~/"):
		return filepath.Join(home, path[2:])
	}
	return filepath.Clean(path)
}

// Summary is a redacted, human-readable rendering of the effective
// configuration: it reports whether a secret file exists, never its contents.
//
// The app surface's token lines are marked "not used" when the surface is off,
// because "missing" there would read as a problem rather than as the switch it
// is.
func (c *Config) Summary() []string {
	file := func(label, path, note string) string {
		state := "missing"
		if _, err := os.Stat(path); err == nil {
			state = "present"
		}
		if note != "" {
			state += ", " + note
		}
		return fmt.Sprintf("%s: %s (%s)", label, path, state)
	}
	// The app surface's two lines say when the surface is off, because "missing"
	// on its own reads as a problem rather than as the switch it is.
	appTokenNote, botTokenNote := "", ""
	if !c.Slack.AppEnabled() {
		appTokenNote, botTokenNote = "not used", "not used"
	}
	sd := c.Slack.SelfDM
	sdMode := "off"
	if sd.Enabled {
		sdMode = "on (" + sd.AuthDescription() + ")"
	}
	// Whichever credential file the configured mode does not read is marked, so a
	// missing self-DM file reads as the switch it is rather than as a fault.
	sessionNote, xoxpNote := "not used", "not used"
	if sd.Enabled && sd.UserOAuth() {
		xoxpNote = ""
	} else if sd.Enabled {
		sessionNote = ""
	}
	workspace := orNone(sd.WorkspaceURL)
	if sd.UserOAuth() && workspace == "none" {
		// A user token is not tied to the workspace host: Slack resolves it.
		workspace = "slack.com"
	}
	lines := []string{
		fmt.Sprintf("slack app surface: %s (a missing token disables it)", onOff(c.Slack.AppEnabled())),
		file("slack app token", c.Slack.AppTokenFile, appTokenNote),
		file("slack bot token", c.Slack.BotTokenFile, botTokenNote),
		fmt.Sprintf("slack self-DM surface: %s", sdMode),
		file("self-DM xoxp token", sd.XOXPFile, xoxpNote),
		file("self-DM xoxc token", sd.XOXCFile, sessionNote),
		file("self-DM xoxd cookie", sd.XOXDFile, sessionNote),
		fmt.Sprintf("self-DM channel: %s, workspace: %s, poll: %s",
			orNone(sd.ChannelID), workspace, sd.PollEvery()),
		file("gateway admin token", c.Gateway.AdminTokenFile, ""),
		file("gateway thread token", c.Gateway.ThreadTokenFile, ""),
		fmt.Sprintf("gateway state dir: %s", c.Gateway.StateDir),
		fmt.Sprintf("allowed users: %s", joinOr(c.Slack.Access.AllowedUsers, "none — every request is denied")),
		fmt.Sprintf("allowed channels: %s", joinOr(c.Slack.Access.AllowedChannels, "any channel the bot is in")),
		fmt.Sprintf("projects root: %s", c.Paths.ProjectsRoot),
		fmt.Sprintf("repos root: %s", c.Paths.ReposRoot),
		fmt.Sprintf("database: %s", c.Paths.DBPath),
		fmt.Sprintf("warm session cap: %d (%s), idle close %dm, admission wait %ds",
			c.Concurrency.MaxWarmSessions, c.Concurrency.Eviction,
			c.Concurrency.ThreadIdleCloseMinutes, c.Concurrency.AdmissionWaitSeconds),
		fmt.Sprintf("render: %s (flush %dms)", c.Render.Mode, c.Render.FlushMS),
		fmt.Sprintf("approvals: %s, pi args: %v", c.Behavior.Approvals, c.Gateway.PiArgs),
		fmt.Sprintf("injected prompt: %s", yesNo(c.Behavior.InjectedPrompt != "")),
		fmt.Sprintf("log: %s/%s", c.Log.Level, c.Log.Format),
	}
	return lines
}

func orNone(value string) string {
	if strings.TrimSpace(value) == "" {
		return "none"
	}
	return value
}

func onOff(b bool) string {
	if b {
		return "on"
	}
	return "off"
}

func joinOr(values []string, fallback string) string {
	if len(values) == 0 {
		return fallback
	}
	return strings.Join(values, ",")
}

func yesNo(b bool) string {
	if b {
		return "yes"
	}
	return "no"
}
