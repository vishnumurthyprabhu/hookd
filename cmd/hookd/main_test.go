package main

import (
	"bytes"
	"context"
	"net"
	"net/http"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"hookd/internal/config"
	"hookd/internal/ids"
	"hookd/internal/store"
)

// useTempDB points HOOKD_DB_PATH at a new SQLite file and returns its path.
// It also makes serve listen on a free port and stop as soon as it starts.
func useTempDB(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "hookd.db")
	t.Setenv("HOOKD_DB_PATH", path)
	t.Setenv("HOOKD_LISTEN_ADDR", "127.0.0.1:0")
	setSignalContext(t, func() (context.Context, context.CancelFunc) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		return ctx, cancel
	})
	return path
}

func setSignalContext(t *testing.T, f func() (context.Context, context.CancelFunc)) {
	old := signalContext
	signalContext = f
	t.Cleanup(func() { signalContext = old })
}

func TestServeHealthz(t *testing.T) {
	useTempDB(t)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	ln.Close()
	t.Setenv("HOOKD_LISTEN_ADDR", addr)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	setSignalContext(t, func() (context.Context, context.CancelFunc) { return ctx, cancel })

	done := make(chan int, 1)
	var stdout, stderr bytes.Buffer
	go func() { done <- run([]string{"serve"}, &stdout, &stderr) }()

	deadline := time.Now().Add(5 * time.Second)
	for {
		resp, err := http.Get("http://" + addr + "/healthz")
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("GET /healthz: status %d, want 200", resp.StatusCode)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("server did not answer: %v", err)
		}
		time.Sleep(20 * time.Millisecond)
	}

	cancel()
	select {
	case code := <-done:
		if code != 0 {
			t.Fatalf("serve exit code %d, stderr %q", code, stderr.String())
		}
	case <-time.After(10 * time.Second):
		t.Fatal("serve did not stop after its context was cancelled")
	}
}

func TestRun(t *testing.T) {
	tests := []struct {
		name   string
		args   []string
		env    map[string]string
		code   int
		stdout string
		stderr string
	}{
		{name: "no args prints usage", args: nil, code: 2, stderr: "Usage: hookd"},
		{name: "help", args: []string{"--help"}, code: 0, stdout: "Usage: hookd"},
		{name: "unknown command", args: []string{"frobnicate"}, code: 2, stderr: `unknown command "frobnicate"`},
		{name: "serve", args: []string{"serve"}, code: 0, stderr: "stopped"},
		{name: "migrate", args: []string{"migrate"}, code: 0, stdout: "applied 0001_init"},
		{
			name:   "account create without migrations",
			args:   []string{"account", "create", "--name", "acme"},
			code:   1,
			stderr: `hookd migrate`,
		},
		{name: "account create without name", args: []string{"account", "create"}, code: 2, stderr: "--name is required"},
		{name: "account without subcommand", args: []string{"account"}, code: 2, stderr: "unknown account command"},
		{
			name:   "apikey create without migrations",
			args:   []string{"apikey", "create", "--account", "acct_x"},
			code:   1,
			stderr: `hookd migrate`,
		},
		{name: "apikey create without account", args: []string{"apikey", "create"}, code: 2, stderr: "--account is required"},
		{name: "apikey without subcommand", args: []string{"apikey"}, code: 2, stderr: "unknown apikey command"},
		{name: "unknown flag", args: []string{"serve", "--nope"}, code: 2, stderr: "flag provided but not defined"},
		{
			name:   "bad config",
			args:   []string{"serve"},
			env:    map[string]string{"HOOKD_POLL_INTERVAL": "often"},
			code:   1,
			stderr: "HOOKD_POLL_INTERVAL",
		},
		{
			name:   "unreachable database",
			args:   []string{"migrate"},
			env:    map[string]string{"HOOKD_DB_PATH": "/nonexistent/dir/hookd.db"},
			code:   1,
			stderr: "hookd migrate:",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			useTempDB(t)
			for k, v := range tt.env {
				t.Setenv(k, v)
			}
			var stdout, stderr bytes.Buffer
			code := run(tt.args, &stdout, &stderr)
			if code != tt.code {
				t.Errorf("exit code = %d, want %d (stderr %q)", code, tt.code, stderr.String())
			}
			if !strings.Contains(stdout.String(), tt.stdout) {
				t.Errorf("stdout = %q, want it to contain %q", stdout.String(), tt.stdout)
			}
			if !strings.Contains(stderr.String(), tt.stderr) {
				t.Errorf("stderr = %q, want it to contain %q", stderr.String(), tt.stderr)
			}
		})
	}
}

// runOK runs args and fails the test unless the exit code is 0.
func runOK(t *testing.T, args ...string) string {
	t.Helper()
	var stdout, stderr bytes.Buffer
	if code := run(args, &stdout, &stderr); code != 0 {
		t.Fatalf("hookd %s: exit code %d, stderr %q", strings.Join(args, " "), code, stderr.String())
	}
	return stdout.String()
}

func TestMigrateTwice(t *testing.T) {
	useTempDB(t)
	if out := runOK(t, "migrate"); !strings.Contains(out, "applied 0001_init") {
		t.Fatalf("first migrate: %q", out)
	}
	if out := runOK(t, "migrate"); strings.Contains(out, "applied") || !strings.Contains(out, "up to date") {
		t.Fatalf("second migrate: %q, want only \"up to date\"", out)
	}
}

func TestServeAutoMigrate(t *testing.T) {
	useTempDB(t)
	t.Setenv("HOOKD_AUTO_MIGRATE", "false")
	runOK(t, "serve")
	if out := runOK(t, "migrate"); !strings.Contains(out, "applied 0001_init") {
		t.Fatalf("serve with auto-migrate off still migrated: migrate printed %q", out)
	}

	useTempDB(t)
	t.Setenv("HOOKD_AUTO_MIGRATE", "true")
	runOK(t, "serve")
	if out := runOK(t, "migrate"); !strings.Contains(out, "up to date") {
		t.Fatalf("serve with auto-migrate on did not migrate: migrate printed %q", out)
	}
}

func TestAccountCreate(t *testing.T) {
	path := useTempDB(t)
	runOK(t, "migrate")
	out := runOK(t, "account", "create", "--name", "acme")

	m := regexp.MustCompile(`(?m)^account_id: (\S+)\napi_key: +(\S+)$`).FindStringSubmatch(out)
	if m == nil {
		t.Fatalf("account create printed %q", out)
	}
	acctID, key := m[1], m[2]

	cfg := config.Default().DB
	cfg.Path = path
	s, err := store.Open(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	acct, err := s.AccountByAPIKeyHash(context.Background(), ids.HashAPIKey(key))
	if err != nil {
		t.Fatalf("lookup of printed key: %v", err)
	}
	if acct.ID != acctID || acct.Name != "acme" {
		t.Fatalf("key belongs to %+v, want %s named acme", acct, acctID)
	}
}

func TestAPIKeyCreate(t *testing.T) {
	path := useTempDB(t)
	runOK(t, "migrate")
	re := regexp.MustCompile(`(?m)^account_id: (\S+)\napi_key: +(\S+)$`)
	m := re.FindStringSubmatch(runOK(t, "account", "create", "--name", "acme"))
	if m == nil {
		t.Fatal("account create printed no key")
	}
	acctID, oldKey := m[1], m[2]

	out := runOK(t, "apikey", "create", "--account", acctID)
	m = re.FindStringSubmatch(out)
	if m == nil {
		t.Fatalf("apikey create printed %q", out)
	}
	if m[1] != acctID {
		t.Fatalf("apikey create printed account %s, want %s", m[1], acctID)
	}
	newKey := m[2]
	if newKey == oldKey {
		t.Fatal("apikey create printed the account's existing key")
	}

	cfg := config.Default().DB
	cfg.Path = path
	s, err := store.Open(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	for _, key := range []string{oldKey, newKey} {
		acct, err := s.AccountByAPIKeyHash(context.Background(), ids.HashAPIKey(key))
		if err != nil {
			t.Fatalf("lookup of key: %v", err)
		}
		if acct.ID != acctID {
			t.Fatalf("key belongs to %s, want %s", acct.ID, acctID)
		}
	}

	var stdout, stderr bytes.Buffer
	if code := run([]string{"apikey", "create", "--account", "acct_unknown"}, &stdout, &stderr); code != 1 {
		t.Fatalf("unknown account: exit code %d, want 1", code)
	}
	if !strings.Contains(stderr.String(), `no account "acct_unknown"`) || stdout.Len() != 0 {
		t.Fatalf("unknown account: stdout %q, stderr %q", stdout.String(), stderr.String())
	}
}
