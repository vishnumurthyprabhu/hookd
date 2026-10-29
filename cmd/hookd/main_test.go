package main

import (
	"bytes"
	"context"
	"errors"
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
		{name: "apikey unknown subcommand", args: []string{"apikey", "rotate"}, code: 2, stderr: "unknown apikey command"},
		{
			name:   "apikey list without migrations",
			args:   []string{"apikey", "list", "--account", "acct_x"},
			code:   1,
			stderr: `hookd migrate`,
		},
		{name: "apikey list without account", args: []string{"apikey", "list"}, code: 2, stderr: "--account is required"},
		{
			name:   "apikey revoke without migrations",
			args:   []string{"apikey", "revoke", "--id", "key_x"},
			code:   1,
			stderr: `hookd migrate`,
		},
		{name: "apikey revoke without id", args: []string{"apikey", "revoke"}, code: 2, stderr: "--id is required"},
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

func TestAPIKeyListAndRevoke(t *testing.T) {
	path := useTempDB(t)
	runOK(t, "migrate")
	re := regexp.MustCompile(`(?m)^account_id: (\S+)\napi_key: +(\S+)$`)
	m := re.FindStringSubmatch(runOK(t, "account", "create", "--name", "acme"))
	if m == nil {
		t.Fatal("account create printed no key")
	}
	acctID, oldKey := m[1], m[2]
	m = re.FindStringSubmatch(runOK(t, "apikey", "create", "--account", acctID))
	if m == nil {
		t.Fatal("apikey create printed no key")
	}
	newKey := m[2]

	cfg := config.Default().DB
	cfg.Path = path
	s, err := store.Open(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	keys, err := s.ListAPIKeys(context.Background(), acctID)
	if err != nil || len(keys) != 2 {
		t.Fatalf("ListAPIKeys = %+v, %v; want 2 keys", keys, err)
	}

	out := runOK(t, "apikey", "list", "--account", acctID)
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if len(lines) != 3 || !strings.HasPrefix(lines[0], "ID") {
		t.Fatalf("apikey list printed %q, want a header and 2 keys", out)
	}
	for i, k := range keys {
		f := strings.Fields(lines[i+1])
		if len(f) != 3 || f[0] != k.ID || f[1] != k.Prefix || f[2] != k.CreatedAt.UTC().Format(time.RFC3339) {
			t.Errorf("apikey list line %q, want %s %s %s", lines[i+1], k.ID, k.Prefix, k.CreatedAt.UTC().Format(time.RFC3339))
		}
	}
	if strings.Contains(out, oldKey) || strings.Contains(out, newKey) {
		t.Fatal("apikey list printed a full key")
	}
	if !strings.HasPrefix(oldKey, keys[0].Prefix) {
		t.Fatalf("first listed key %s is not the oldest", keys[0].Prefix)
	}

	out = runOK(t, "apikey", "revoke", "--id", keys[0].ID)
	if !strings.Contains(out, "revoked "+keys[0].ID) || !strings.Contains(out, acctID) {
		t.Fatalf("apikey revoke printed %q", out)
	}
	if _, err := s.AccountByAPIKeyHash(context.Background(), ids.HashAPIKey(oldKey)); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("revoked key still authenticates: err = %v", err)
	}
	if a, err := s.AccountByAPIKeyHash(context.Background(), ids.HashAPIKey(newKey)); err != nil || a.ID != acctID {
		t.Fatalf("other key after revoke: %+v, %v", a, err)
	}
	if out := runOK(t, "apikey", "list", "--account", acctID); strings.Contains(out, keys[0].ID) || !strings.Contains(out, keys[1].ID) {
		t.Fatalf("apikey list after revoke printed %q", out)
	}

	for _, tc := range []struct {
		args   []string
		stderr string
	}{
		{[]string{"apikey", "revoke", "--id", keys[0].ID}, `no API key "` + keys[0].ID + `"`},
		{[]string{"apikey", "list", "--account", "acct_unknown"}, `no account "acct_unknown"`},
	} {
		var stdout, stderr bytes.Buffer
		if code := run(tc.args, &stdout, &stderr); code != 1 {
			t.Fatalf("%v: exit code %d, want 1", tc.args, code)
		}
		if !strings.Contains(stderr.String(), tc.stderr) || stdout.Len() != 0 {
			t.Fatalf("%v: stdout %q, stderr %q", tc.args, stdout.String(), stderr.String())
		}
	}

	runOK(t, "apikey", "revoke", "--id", keys[1].ID)
	var stdout, stderr bytes.Buffer
	if code := run([]string{"apikey", "list", "--account", acctID}, &stdout, &stderr); code != 0 {
		t.Fatalf("list of account without keys: exit code %d, stderr %q", code, stderr.String())
	}
	if stdout.Len() != 0 || !strings.Contains(stderr.String(), "no API keys") {
		t.Fatalf("list of account without keys: stdout %q, stderr %q", stdout.String(), stderr.String())
	}
}
