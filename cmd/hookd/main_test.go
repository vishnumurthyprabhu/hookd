package main

import (
	"bytes"
	"strings"
	"testing"
)

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
		{name: "migrate", args: []string{"migrate"}, code: 0, stdout: "migrate: not implemented"},
		{name: "account create", args: []string{"account", "create", "--name", "acme"}, code: 0, stdout: "account create: not implemented"},
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
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
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
