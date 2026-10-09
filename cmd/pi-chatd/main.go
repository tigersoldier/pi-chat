// Command pi-chatd is pi-chat's daemon: it bridges chat platforms to
// pi-gatewayd. Slack is the first integration, and it has two surfaces that can
// run together or apart (DESIGN.md §12):
//
//   - the app surface: an installed Slack app, Socket Mode in, Web API out;
//   - the self-DM surface: your own "Notes to self" conversation, driven with a
//     browser session token and polled — no app required.
//
// pi-gatewayd must already be running (PLAN.md M0).
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"slices"
	"syscall"
	"time"

	"github.com/tigersoldier/pi-chat/internal/bot"
	"github.com/tigersoldier/pi-chat/internal/config"
	"github.com/tigersoldier/pi-chat/internal/slack"
	"github.com/tigersoldier/pi-chat/internal/slack/selfdm"
	"github.com/tigersoldier/pi-chat/internal/store"
)

// version is pi-chat's own version, not pi's and not pi-gateway's.
const version = "0.1.0-dev"

func main() {
	fs := flag.NewFlagSet("pi-chatd", flag.ExitOnError)
	var (
		configPath  = fs.String("config", "", "configuration file (default "+config.DefaultPath()+")")
		check       = fs.Bool("check", false, "load and validate the configuration, print a summary, and exit")
		showVersion = fs.Bool("version", false, "print pi-chatd's version and exit")
	)
	fs.Usage = func() {
		fmt.Fprintf(os.Stderr, "Usage: pi-chatd [options]\n\nOptions:\n")
		fs.PrintDefaults()
	}
	_ = fs.Parse(os.Args[1:])

	if *showVersion {
		fmt.Printf("pi-chatd %s\n", version)
		return
	}

	cfg, err := config.Load(*configPath)
	if err != nil {
		fatal(err)
	}

	if *check {
		fmt.Printf("config %s is valid\n", cfg.Path())
		for _, line := range cfg.Summary() {
			fmt.Printf("  %s\n", line)
		}
		return
	}

	if err := run(cfg); err != nil {
		fatal(err)
	}
}

// ingress is one loop that feeds the core.
type ingress struct {
	name string
	run  func(ctx context.Context) error
}

// surfaces is what starting the Slack surfaces produced.
type surfaces struct {
	mux    bot.Platform
	app    *appSurface
	selfdm *selfdm.Surface
}

// appSurface is the installed-app integration: Socket Mode in, Web API out.
type appSurface struct {
	platform  bot.Platform
	api       *slack.API
	appToken  string
	botUserID string
}

// run wires the Slack surfaces to the core and blocks until a signal ends it.
func run(cfg *config.Config) error {
	log := newLogger(cfg.Log)

	// A signal cancels this context, which ends every ingress loop and releases
	// every gateway connection. Turns that are still running are abandoned:
	// the session stays alive in pi-gatewayd, and its answer is in pi's
	// session file.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	st, err := store.Open(ctx, cfg.Paths.DBPath)
	if err != nil {
		return err
	}
	defer st.Close()

	// No thread is warm in a fresh process: a warm thread means a bound
	// connection, and this process holds none yet. Without this, rows left by a
	// crash would claim to be warm forever.
	if n, err := st.MarkThreadsCold(ctx); err != nil {
		log.Warn("cannot reset the warm markers", "error", err)
	} else if n > 0 {
		log.Info("reset threads that a previous run left warm", "count", n)
	}

	sl, err := startSurfaces(ctx, cfg, log, st)
	if err != nil {
		return err
	}

	core := bot.New(cfg, log, sl.mux, st, version)
	defer core.Close()

	// Startup GC: a directory with no thread row is the remains of a crash or
	// of an interrupted delete. Sessions the table still knows are kept, cold
	// ones included, because /pi resume has to find them (DESIGN.md §9).
	if removed, err := core.SweepWorkspaces(ctx); err != nil {
		log.Warn("the workspace sweep reported problems", "error", err)
	} else if len(removed) > 0 {
		log.Info("swept orphaned project directories", "count", len(removed))
	}

	loops := make([]ingress, 0, 2)
	if sl.app != nil {
		socket := slack.NewSocket(sl.app.appToken, sl.app.api, log,
			slack.NewRouter(core, sl.app.botUserID, log).Handle)
		loops = append(loops, ingress{name: "slack app socket", run: socket.Run})
	}
	if sl.selfdm != nil {
		sd := sl.selfdm
		loops = append(loops, ingress{
			name: "slack self-DM poller",
			run:  func(ctx context.Context) error { return sd.Run(ctx, core) },
		})
	}

	names := make([]string, 0, len(loops))
	for _, loop := range loops {
		names = append(names, loop.name)
	}
	log.Info("pi-chatd is ready",
		"config", cfg.Path(),
		"surfaces", names,
		"render", cfg.Render.Mode,
		"allowed_users", len(cfg.Slack.Access.AllowedUsers),
		"allowed_channels", len(cfg.Slack.Access.AllowedChannels),
		"gateway_state_dir", cfg.Gateway.StateDir,
		"database", cfg.Paths.DBPath)

	// Every ingress owns one way in; the core's maintenance loop runs beside
	// them. A loop that stops for good — a revoked token, Socket Mode switched
	// off — cancels the context so the others stop with it: this is one service,
	// even when it has two ways in.
	loopErr := make(chan error, len(loops))
	for _, loop := range loops {
		go func(loop ingress) {
			err := loop.run(ctx)
			if err != nil && !errors.Is(err, context.Canceled) {
				err = fmt.Errorf("%s: %w", loop.name, err)
			} else {
				err = nil
			}
			// Every loop reports exactly once, error or not: the drain below is then
			// a drain, and a clean shutdown does not wait for a timeout to expire.
			loopErr <- err
			stop()
		}(loop)
	}

	coreErr := core.Run(ctx)
	stop()

	// Collect how the loops ended, bounded so one that is slow to notice the
	// cancellation cannot hold the shutdown open.
	var loopErrFirst error
	deadline := time.After(time.Second)
	for range loops {
		select {
		case err := <-loopErr:
			if err != nil && loopErrFirst == nil {
				loopErrFirst = err
			}
		case <-deadline:
		}
	}
	if loopErrFirst != nil {
		return loopErrFirst
	}
	if errors.Is(coreErr, context.Canceled) {
		log.Info("shutting down")
		return nil
	}
	return coreErr
}

// startSurfaces builds every configured surface and the mux that routes between
// them. The app surface is optional in a soft way — a missing token file turns
// it off with a warning — while the self-DM surface is optional in the strict
// way: once enabled, a misconfiguration is an error, because there is nothing
// else that surface could fall back to.
func startSurfaces(ctx context.Context, cfg *config.Config, log *slog.Logger, st *store.Store) (*surfaces, error) {
	out := &surfaces{}
	routes := map[string]bot.Platform{}

	if cfg.Slack.AppEnabled() {
		app, err := startAppSurface(ctx, cfg, log)
		if err != nil {
			return nil, err
		}
		out.app = app
	}

	if cfg.Slack.SelfDM.Enabled {
		sd, err := startSelfDMSurface(ctx, cfg, log, st)
		if err != nil {
			// A surface that is configured but cannot run is disabled when another
			// surface can still serve, and fatal when it is all there is: a stale
			// cookie must not take the app down or make systemd flap on restart, and
			// three surfaces' worth of silence is not better than one.
			return nil, disableOrFail(out.app != nil, log, err)
		}
		if err := checkSelfDMAdmission(cfg, sd); err != nil {
			return nil, disableOrFail(out.app != nil, log, err)
		}
		out.selfdm = sd
		routes[cfg.Slack.SelfDM.ChannelID] = sd.Platform()
	}

	if out.app == nil && out.selfdm == nil {
		return nil, fmt.Errorf("%w: the Slack app tokens are missing (%s, %s) and slack.self_dm is not enabled",
			slack.ErrNoSurface, cfg.Slack.AppTokenFile, cfg.Slack.BotTokenFile)
	}

	// The app surface is the default route: every channel that is not the
	// configured self-DM belongs to the bot.
	var def bot.Platform
	if out.app != nil {
		def = out.app.platform
	}
	out.mux = slack.NewMultiPlatform(log, def, routes)
	return out, nil
}

// startAppSurface checks the app's two tokens and builds its platform. A token
// file that cannot be read disables the surface — that is the point of making
// it optional — while a token Slack *refuses* is still an error: a missing
// secret is a switch, a wrong one is a mistake.
func startAppSurface(ctx context.Context, cfg *config.Config, log *slog.Logger) (*appSurface, error) {
	botToken, err := config.ReadToken(cfg.Slack.BotTokenFile)
	if err != nil {
		log.Warn("the Slack app surface is off: its bot token is not readable",
			"file", cfg.Slack.BotTokenFile, "error", err)
		return nil, nil
	}
	appToken, err := config.ReadToken(cfg.Slack.AppTokenFile)
	if err != nil {
		log.Warn("the Slack app surface is off: its app token is not readable",
			"file", cfg.Slack.AppTokenFile, "error", err)
		return nil, nil
	}

	api := slack.NewAPI(botToken, log)
	me, err := api.AuthTest(ctx)
	if err != nil {
		return nil, fmt.Errorf("check the Slack bot token: %w", err)
	}
	log.Info("connected to Slack as the app",
		"workspace", me.Team,
		"workspace_id", me.TeamID,
		"bot_user", me.User,
		"bot_user_id", me.UserID)

	return &appSurface{
		platform: slack.NewPlatform(api, cfg,
			slack.Identity{TeamID: me.TeamID, UserID: me.UserID, BotID: me.BotID}, log),
		api:       api,
		appToken:  appToken,
		botUserID: me.UserID,
	}, nil
}

// startSelfDMSurface checks the configured credentials and builds the self-DM
// surface. Nothing here is optional once the surface is enabled: it does not
// exist until it can read the conversation it was pointed at.
func startSelfDMSurface(ctx context.Context, cfg *config.Config, log *slog.Logger, st *store.Store) (*selfdm.Surface, error) {
	api, err := selfDMAPI(cfg, log)
	if err != nil {
		return nil, err
	}

	sd, err := selfdm.New(ctx, api, st, selfdm.Config{
		Channel:   cfg.Slack.SelfDM.ChannelID,
		PollEvery: cfg.Slack.SelfDM.PollEvery(),
		FlushMS:   cfg.Render.FlushMS,
	}, log)
	if err != nil {
		return nil, fmt.Errorf("slack.self_dm: %w", err)
	}
	log.Info("connected to Slack as yourself for the self-DM",
		"auth", cfg.Slack.SelfDM.AuthDescription(),
		"workspace_id", sd.Identity().TeamID,
		"user_id", sd.Identity().UserID,
		"channel", cfg.Slack.SelfDM.ChannelID,
		"poll", cfg.Slack.SelfDM.PollEvery())
	return sd, nil
}

// selfDMAPI builds the Web API client for the configured credential mode. Both
// modes act as a person; they differ in where the credential came from, what it
// carries and how long it lives: a pasted browser session, or an OAuth user
// token minted by the team's app (docs/slack-user-token-setup.md).
func selfDMAPI(cfg *config.Config, log *slog.Logger) (*slack.API, error) {
	sd := cfg.Slack.SelfDM
	if sd.UserOAuth() {
		token, err := config.ReadToken(sd.XOXPFile)
		if err != nil {
			// Name the key, not just the path: the path may be a default the
			// person never chose, and the key is what they have to fix.
			return nil, fmt.Errorf("slack.self_dm.xoxp_file: %w", err)
		}
		if err := slack.ValidateUserToken(token); err != nil {
			return nil, fmt.Errorf("slack.self_dm.xoxp_file: %w", err)
		}
		// A user token belongs to a person rather than to one workspace host, and
		// carries no cookie: Slack resolves the workspace from the token itself.
		return slack.NewAPI(token, log), nil
	}

	xoxc, err := config.ReadToken(sd.XOXCFile)
	if err != nil {
		return nil, fmt.Errorf("slack.self_dm.xoxc_file: %w", err)
	}
	xoxd, err := config.ReadToken(sd.XOXDFile)
	if err != nil {
		return nil, fmt.Errorf("slack.self_dm.xoxd_file: %w", err)
	}
	return slack.NewAPI(xoxc, log,
		slack.WithBaseURL(sd.APIBase()),
		slack.WithCookie(slack.CookieHeader(xoxd))), nil
}

// disableOrFail is the policy for a configured surface that cannot run: when
// another surface is up, the failure is reported at error level and the broken
// one is left out of the wiring; when it is the only one, the error is the
// daemon's.
func disableOrFail(others bool, log *slog.Logger, err error) error {
	if !others {
		return err
	}
	log.Error("the Slack self-DM surface is off: it could not start, and the daemon keeps running without it",
		"error", err)
	return nil
}

// checkSelfDMAdmission refuses a self-DM whose own user is not allowed to talk
// to it. The surface would otherwise start cleanly and refuse every single
// message, which reads as a broken bot rather than as a missing allowlist entry.
func checkSelfDMAdmission(cfg *config.Config, sd *selfdm.Surface) error {
	user := sd.Identity().UserID
	if !slices.Contains(cfg.Slack.Access.AllowedUsers, user) {
		return fmt.Errorf(
			"slack.self_dm is enabled as %s, who is not in slack.access.allowed_users: every message would be refused",
			user)
	}
	channels := cfg.Slack.Access.AllowedChannels
	if len(channels) > 0 && !slices.Contains(channels, cfg.Slack.SelfDM.ChannelID) {
		return fmt.Errorf("slack.self_dm.channel_id %s is not in slack.access.allowed_channels",
			cfg.Slack.SelfDM.ChannelID)
	}
	return nil
}

// newLogger builds the logger the configuration asks for. Logs go to stderr;
// stdout is left to command output.
func newLogger(cfg config.Log) *slog.Logger {
	level := slog.LevelInfo
	switch cfg.Level {
	case "debug":
		level = slog.LevelDebug
	case "warn":
		level = slog.LevelWarn
	case "error":
		level = slog.LevelError
	}
	options := &slog.HandlerOptions{Level: level}
	var handler slog.Handler
	if cfg.Format == "json" {
		handler = slog.NewJSONHandler(os.Stderr, options)
	} else {
		handler = slog.NewTextHandler(os.Stderr, options)
	}
	return slog.New(handler)
}

func fatal(err error) {
	fmt.Fprintf(os.Stderr, "pi-chatd: %v\n", err)
	os.Exit(1)
}
