package main

import (
	"bytes"
	"context"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"hookd/internal/config"
	"hookd/internal/ids"
	"hookd/internal/store"
)

// useTempDB points HOOKD_DB_PATH at a new SQLite file and returns its path.
func useTempDB(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "hookd.db")
	t.Setenv("HOOKD_DB_PATH", path)
	return path
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
		{name: "serve", args: []string{"serve"}, code: 0, stdout: "serve: not implemented"},
		{name: "migrate", args: []string{"migrate"}, code: 0, stdout: "applied 0001_init"},
		{
			name:   "account create without migrations",
			args:   []string{"account", "create", "--name", "acme"},
			code:   1,
			stderr: `hookd migrate`,
		},
		{name: "account create without name", args: []string{"account", "create"}, code: 2, stderr: "--name is required"},
		{name: "account without subcommand", args: []string{"account"}, code: 2, stderr: "unknown account command"},
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
