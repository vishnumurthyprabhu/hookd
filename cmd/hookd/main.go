// Command hookd runs the Hookd webhook delivery service and its operator commands.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"hookd/internal/admin"
	"hookd/internal/api"
	"hookd/internal/config"
	"hookd/internal/delivery"
	"hookd/internal/settings"
	"hookd/internal/store"
)

const usage = `Usage: hookd <command> [options]

Commands:
  serve                        Run the API server and delivery workers
  migrate                      Apply database migrations
  account create --name NAME   Create an account and print its API key
  apikey create --account ID   Create another API key for an account and print it
  apikey list --account ID     List an account's API keys
  apikey revoke --id KEY_ID    Revoke an API key so it no longer works

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
	case "apikey":
		sub := map[string]func(config.Config, []string, io.Writer, io.Writer) int{
			"create": apikeyCreate,
			"list":   apikeyList,
			"revoke": apikeyRevoke,
		}
		if len(rest) == 0 || sub[rest[0]] == nil {
			fmt.Fprintf(stderr, "hookd: unknown apikey command %q\n\n%s", strings.Join(rest, " "), usage)
			return 2
		}
		name, rest, cmd = "apikey "+rest[0], rest[1:], sub[rest[0]]
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
	// Until the settings manager exists, which needs the store, the log level
	// is the environment's.
	log := cfg.NewLogger(stderr)
	log.Debug("starting", "listen_addr", cfg.ListenAddr, "db_driver", cfg.DB.Driver)

	// mgr is set before anything stores an event.
	var mgr *settings.Manager
	ctx := context.Background()
	s, err := store.Open(ctx, cfg.DB, store.WithIdempotencyWindowFunc(func() time.Duration {
		return mgr.Current().IdempotencyWindow
	}))
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

	// The settings saved from the admin UI apply from the start, so a restart
	// keeps them. If they cannot be read or are invalid, Hookd starts with
	// the environment's and the manager keeps trying.
	mgr, err = settings.New(s)
	if err != nil {
		log.Error("settings", "err", err)
		return 1
	}
	// From here on the log level follows HOOKD_LOG_LEVEL as the manager sees
	// it, and the manager logs with that logger too.
	log = cfg.NewLoggerAt(stderr, mgr.LevelVar())
	settings.WithLogger(log)(mgr)
	if err := mgr.Load(ctx); err != nil {
		log.Error("saved settings not applied; using the environment's", "err", err)
	}

	ln, err := net.Listen("tcp", cfg.ListenAddr)
	if err != nil {
		log.Error("listen", "err", err)
		return 1
	}
	log.Info("listening", "addr", ln.Addr().String())

	ctx, stop := signalContext()
	defer stop()
	settingsCtx, stopSettings := context.WithCancel(ctx)
	settingsDone := make(chan struct{})
	go func() {
		defer close(settingsDone)
		mgr.Run(settingsCtx)
	}()

	pool := delivery.NewPool(s, log, func() delivery.Config {
		c := mgr.Current()
		return delivery.Config{
			Workers:      c.Workers,
			PollInterval: c.PollInterval,
			Timeout:      c.DeliveryTimeout,
			Lease:        c.DeliveryLease,
		}
	})
	workersCtx, stopWorkers := context.WithCancel(ctx)
	workersDone := make(chan struct{})
	go func() {
		defer close(workersDone)
		pool.Run(workersCtx)
	}()

	opts := []api.Option{
		api.WithMaxPayloadBytesFunc(func() int64 { return mgr.Current().MaxPayloadBytes }),
		api.WithWake(pool.Wake()),
	}
	if cfg.AdminUI {
		opts = append(opts, api.WithAdmin(admin.New(s, log)))
	}
	srv := api.New(s, log, opts...)
	err = srv.Serve(ctx, ln)
	// Workers finish the deliveries they are sending before they stop.
	stopWorkers()
	<-workersDone
	stopSettings()
	<-settingsDone
	if err != nil {
		log.Error("serve", "err", err)
		return 1
	}
	log.Info("stopped")
	return 0
}

// signalContext returns the context serve runs under: it is done on SIGINT
// or SIGTERM. Tests replace it.
var signalContext = func() (context.Context, context.CancelFunc) {
	return signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
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

// apikeyCreate adds an API key to an existing account. The account's other
// keys keep working.
func apikeyCreate(cfg config.Config, args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("apikey create", stderr)
	account := fs.String("account", "", "account ID, acct_... (required)")
	if err := fs.Parse(args); err != nil {
		return flagExit(err)
	}
	accountID := strings.TrimSpace(*account)
	if accountID == "" {
		fmt.Fprintln(stderr, "hookd apikey create: --account is required")
		fs.Usage()
		return 2
	}
	ctx := context.Background()
	s, err := store.Open(ctx, cfg.DB)
	if err != nil {
		fmt.Fprintf(stderr, "hookd apikey create: %v\n", err)
		return 1
	}
	defer s.Close()
	_, key, err := s.CreateAPIKey(ctx, accountID)
	if errors.Is(err, store.ErrNotFound) {
		fmt.Fprintf(stderr, "hookd apikey create: no account %q\n", accountID)
		return 1
	}
	if err != nil {
		fmt.Fprintf(stderr, "hookd apikey create: %v (has \"hookd migrate\" been run?)\n", err)
		return 1
	}
	fmt.Fprintf(stdout, "account_id: %s\napi_key:    %s\n", accountID, key)
	fmt.Fprintln(stderr, "Keep the API key safe: it is shown only once.")
	return 0
}

// apikeyList prints the record of each of an account's API keys: its ID,
// display prefix and creation time. The keys themselves are not stored.
func apikeyList(cfg config.Config, args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("apikey list", stderr)
	account := fs.String("account", "", "account ID, acct_... (required)")
	if err := fs.Parse(args); err != nil {
		return flagExit(err)
	}
	accountID := strings.TrimSpace(*account)
	if accountID == "" {
		fmt.Fprintln(stderr, "hookd apikey list: --account is required")
		fs.Usage()
		return 2
	}
	ctx := context.Background()
	s, err := store.Open(ctx, cfg.DB)
	if err != nil {
		fmt.Fprintf(stderr, "hookd apikey list: %v\n", err)
		return 1
	}
	defer s.Close()
	keys, err := s.ListAPIKeys(ctx, accountID)
	if errors.Is(err, store.ErrNotFound) {
		fmt.Fprintf(stderr, "hookd apikey list: no account %q\n", accountID)
		return 1
	}
	if err != nil {
		fmt.Fprintf(stderr, "hookd apikey list: %v (has \"hookd migrate\" been run?)\n", err)
		return 1
	}
	if len(keys) == 0 {
		fmt.Fprintf(stderr, "Account %s has no API keys.\n", accountID)
		return 0
	}
	tw := tabwriter.NewWriter(stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "ID\tPREFIX\tCREATED")
	for _, k := range keys {
		fmt.Fprintf(tw, "%s\t%s\t%s\n", k.ID, k.Prefix, k.CreatedAt.UTC().Format(time.RFC3339))
	}
	tw.Flush()
	return 0
}

// apikeyRevoke deletes an API key, so requests made with it get 401. The
// account's other keys keep working.
func apikeyRevoke(cfg config.Config, args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("apikey revoke", stderr)
	idFlag := fs.String("id", "", "API key ID, key_... as shown by \"hookd apikey list\" (required)")
	if err := fs.Parse(args); err != nil {
		return flagExit(err)
	}
	id := strings.TrimSpace(*idFlag)
	if id == "" {
		fmt.Fprintln(stderr, "hookd apikey revoke: --id is required")
		fs.Usage()
		return 2
	}
	ctx := context.Background()
	s, err := store.Open(ctx, cfg.DB)
	if err != nil {
		fmt.Fprintf(stderr, "hookd apikey revoke: %v\n", err)
		return 1
	}
	defer s.Close()
	k, err := s.DeleteAPIKey(ctx, id)
	if errors.Is(err, store.ErrNotFound) {
		fmt.Fprintf(stderr, "hookd apikey revoke: no API key %q\n", id)
		return 1
	}
	if err != nil {
		fmt.Fprintf(stderr, "hookd apikey revoke: %v (has \"hookd migrate\" been run?)\n", err)
		return 1
	}
	fmt.Fprintf(stdout, "revoked %s (%s...) of account %s\n", k.ID, k.Prefix, k.AccountID)
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
