// Package config loads pi-chat's configuration file.
//
// The shape and the meaning of every field are specified in DESIGN.md §11; this
// package is the single place that knows the file layout, the defaults, and
// what counts as a valid configuration.
package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/BurntSushi/toml"
)

// Defaults, also used as documentation by the example file.
const (
	DefaultGatewayStateDir   = "~/.config/pi-gateway"
	DefaultAdminTokenFile    = "~/.config/pi-chat/gateway-admin.token"
	DefaultThreadTokenFile   = "~/.config/pi-chat/gateway-thread.token"
	DefaultSlackAppTokenFile = "~/.config/pi-chat/slack-app-token"
	DefaultSlackBotTokenFile = "~/.config/pi-chat/slack-bot-token"
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

// Slack holds the Socket Mode credentials and the Slack-specific access rules.
type Slack struct {
	AppTokenFile string `toml:"app_token_file"`
	BotTokenFile string `toml:"bot_token_file"`
	Access       Access `toml:"access"`
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
		},
		Gateway: Gateway{
			StateDir:        DefaultGatewayStateDir,
			AdminTokenFile:  DefaultAdminTokenFile,
			ThreadTokenFile: DefaultThreadTokenFile,
			PiArgs:          []string{"--approve"},
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
	require("slack.app_token_file", c.Slack.AppTokenFile)
	require("slack.bot_token_file", c.Slack.BotTokenFile)
	require("gateway.state_dir", c.Gateway.StateDir)
	require("gateway.admin_token_file", c.Gateway.AdminTokenFile)
	require("gateway.thread_token_file", c.Gateway.ThreadTokenFile)
	require("paths.projects_root", c.Paths.ProjectsRoot)
	require("paths.repos_root", c.Paths.ReposRoot)
	require("paths.db_path", c.Paths.DBPath)

	for _, p := range []struct{ what, value string }{
		{"slack.app_token_file", c.Slack.AppTokenFile},
		{"slack.bot_token_file", c.Slack.BotTokenFile},
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
		&c.Gateway.StateDir, &c.Gateway.AdminTokenFile, &c.Gateway.ThreadTokenFile,
		&c.Paths.ProjectsRoot, &c.Paths.ReposRoot, &c.Paths.DBPath,
	} {
		*p = expand(*p, home)
	}
	return nil
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
func (c *Config) Summary() []string {
	file := func(label, path string) string {
		state := "missing"
		if _, err := os.Stat(path); err == nil {
			state = "present"
		}
		return fmt.Sprintf("%s: %s (%s)", label, path, state)
	}
	lines := []string{
		file("slack app token", c.Slack.AppTokenFile),
		file("slack bot token", c.Slack.BotTokenFile),
		file("gateway admin token", c.Gateway.AdminTokenFile),
		file("gateway thread token", c.Gateway.ThreadTokenFile),
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
