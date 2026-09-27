// Command pi-chatd is pi-chat's daemon: it bridges chat platforms to
// pi-gatewayd. Slack is the first integration.
//
// Phase 0 (the Slack vertical slice) is not wired up yet — see PLAN.md M3.
// Today the command loads and validates the configuration, which is what
// `--check` exists for.
package main

import (
	"flag"
	"fmt"
	"os"

	"github.com/tigersoldier/pi-chat/internal/config"
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

	fatal(fmt.Errorf("the Slack adapter is not implemented yet; " +
		"run with --check to validate the configuration, or see PLAN.md M3"))
}

func fatal(err error) {
	fmt.Fprintf(os.Stderr, "pi-chatd: %v\n", err)
	os.Exit(1)
}
