// Command hookd runs the Hookd webhook delivery service and its operator commands.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"hookd/internal/config"
	"hookd/internal/store"
)

const usage = `Usage: hookd <command> [options]

Commands:
  serve                        Run the API server and delivery workers
  migrate                      Apply database migrations
  account create --name NAME   Create an account and print its API key

Settings are read from HOOKD_* environment variables.
`

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

// run dispatches args to a subcommand and returns the process exit code.
func run(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprint(stderr, usage)
		return 2
	}

	var cmd func(config.Config, []string, io.Writer, io.Writer) int
	name, rest := args[0], args[1:]
	switch name {
	case "help", "-h", "-help", "--help":
		fmt.Fprint(stdout, usage)
		return 0
	case "serve":
		cmd = serve
	case "migrate":
		cmd = migrate
	case "account":
		if len(rest) == 0 || rest[0] != "create" {
			fmt.Fprintf(stderr, "hookd: unknown account command %q\n\n%s", strings.Join(rest, " "), usage)
			return 2
		}
		name, rest, cmd = "account create", rest[1:], accountCreate
	default:
		fmt.Fprintf(stderr, "hookd: unknown command %q\n\n%s", name, usage)
		return 2
	}

	cfg, err := config.Load()
	if err != nil {
		fmt.Fprintf(stderr, "hookd: %v\n", err)
		return 1
	}
	return cmd(cfg, rest, stdout, stderr)
}

func serve(cfg config.Config, args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("serve", stderr)
	if err := fs.Parse(args); err != nil {
		return flagExit(err)
	}
	log := cfg.NewLogger(stderr)
	log.Debug("starting", "listen_addr", cfg.ListenAddr, "db_driver", cfg.DB.Driver)

	ctx := context.Background()
	s, err := store.Open(ctx, cfg.DB)
	if err != nil {
		log.Error("open database", "err", err)
		return 1
	}
	defer s.Close()
	if cfg.AutoMigrate {
		applied, err := s.Migrate(ctx)
		if err != nil {
			log.Error("apply migrations", "err", err)
			return 1
		}
		for _, v := range applied {
			log.Info("applied migration", "version", v)
		}
	}
	fmt.Fprintln(stdout, "serve: not implemented")
	return 0
}

func migrate(cfg config.Config, args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("migrate", stderr)
	if err := fs.Parse(args); err != nil {
		return flagExit(err)
	}
	ctx := context.Background()
	s, err := store.Open(ctx, cfg.DB)
	if err != nil {
		fmt.Fprintf(stderr, "hookd migrate: %v\n", err)
		return 1
	}
	defer s.Close()
	applied, err := s.Migrate(ctx)
	for _, v := range applied {
		fmt.Fprintf(stdout, "applied %s\n", v)
	}
	if err != nil {
		fmt.Fprintf(stderr, "hookd migrate: %v\n", err)
		return 1
	}
	if len(applied) == 0 {
		fmt.Fprintln(stdout, "database is up to date")
	}
	return 0
}

func accountCreate(cfg config.Config, args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("account create", stderr)
	name := fs.String("name", "", "account name (required)")
	if err := fs.Parse(args); err != nil {
		return flagExit(err)
	}
	if strings.TrimSpace(*name) == "" {
		fmt.Fprintln(stderr, "hookd account create: --name is required")
		fs.Usage()
		return 2
	}
	ctx := context.Background()
	s, err := store.Open(ctx, cfg.DB)
	if err != nil {
		fmt.Fprintf(stderr, "hookd account create: %v\n", err)
		return 1
	}
	defer s.Close()
	acct, err := s.CreateAccount(ctx, strings.TrimSpace(*name))
	if err != nil {
		fmt.Fprintf(stderr, "hookd account create: %v (has \"hookd migrate\" been run?)\n", err)
		return 1
	}
	_, key, err := s.CreateAPIKey(ctx, acct.ID)
	if err != nil {
		fmt.Fprintf(stderr, "hookd account create: %v\n", err)
		return 1
	}
	fmt.Fprintf(stdout, "account_id: %s\napi_key:    %s\n", acct.ID, key)
	fmt.Fprintln(stderr, "Keep the API key safe: it is shown only once.")
	return 0
}

func newFlagSet(name string, stderr io.Writer) *flag.FlagSet {
	fs := flag.NewFlagSet("hookd "+name, flag.ContinueOnError)
	fs.SetOutput(stderr)
	return fs
}

func flagExit(err error) int {
	if errors.Is(err, flag.ErrHelp) {
		return 0
	}
	return 2
}
