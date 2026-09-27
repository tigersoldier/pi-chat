// Command pi-chatd is pi-chat's daemon: it bridges chat platforms to
// pi-gatewayd. Slack is the first integration.
//
// It connects to Slack over Socket Mode, turns mentions of the bot into pi
// prompts through pi-gatewayd, and streams the answers back into the thread.
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
	"syscall"

	"github.com/tigersoldier/pi-chat/internal/bot"
	"github.com/tigersoldier/pi-chat/internal/config"
	"github.com/tigersoldier/pi-chat/internal/slack"
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

// run wires the Slack adapter to the core and blocks until a signal ends it.
func run(cfg *config.Config) error {
	log := newLogger(cfg.Log)

	// A signal cancels this context, which ends the socket loop and releases
	// every gateway connection. Turns that are still running are abandoned:
	// the session stays alive in pi-gatewayd, and its answer is in pi's
	// session file.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	botToken, err := config.ReadToken(cfg.Slack.BotTokenFile)
	if err != nil {
		return err
	}
	appToken, err := config.ReadToken(cfg.Slack.AppTokenFile)
	if err != nil {
		return err
	}

	api := slack.NewAPI(botToken, log)
	me, err := api.AuthTest(ctx)
	if err != nil {
		return fmt.Errorf("check the Slack bot token: %w", err)
	}
	log.Info("connected to Slack",
		"workspace", me.Team,
		"workspace_id", me.TeamID,
		"bot_user", me.User,
		"bot_user_id", me.UserID)

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

	core := bot.New(cfg, log, slack.NewPlatform(api, cfg, me.TeamID, log), st, version)
	defer core.Close()

	// Startup GC: a directory with no thread row is the remains of a crash or
	// of an interrupted delete. Sessions the table still knows are kept, cold
	// ones included, because /pi resume has to find them (DESIGN.md §9).
	if removed, err := core.SweepWorkspaces(ctx); err != nil {
		log.Warn("the workspace sweep reported problems", "error", err)
	} else if len(removed) > 0 {
		log.Info("swept orphaned project directories", "count", len(removed))
	}

	log.Info("pi-chatd is ready",
		"config", cfg.Path(),
		"render", cfg.Render.Mode,
		"allowed_users", len(cfg.Slack.Access.AllowedUsers),
		"allowed_channels", len(cfg.Slack.Access.AllowedChannels),
		"gateway_state_dir", cfg.Gateway.StateDir,
		"database", cfg.Paths.DBPath)

	socket := slack.NewSocket(appToken, api, log, slack.NewRouter(core, me.UserID, log).Handle)

	// The socket owns ingress; the core's maintenance loop runs beside it. A
	// socket that stops for good — a revoked token, Socket Mode switched off —
	// cancels the context so the sweeper stops with it, and vice versa: this is
	// one service, not two processes sharing a token.
	socketErr := make(chan error, 1)
	go func() {
		socketErr <- socket.Run(ctx)
		stop()
	}()
	coreErr := core.Run(ctx)
	if err := <-socketErr; err != nil && !errors.Is(err, context.Canceled) {
		return err
	}
	if errors.Is(coreErr, context.Canceled) {
		log.Info("shutting down")
		return nil
	}
	return coreErr
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
