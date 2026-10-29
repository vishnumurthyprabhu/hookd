package settings

import (
	"context"
	"errors"
	"log/slog"
	"reflect"
	"strings"
	"testing"
	"time"

	"hookd/internal/store"
	"hookd/internal/store/storetest"
)

func envOf(vars map[string]string) Option {
	return WithLookupEnv(func(name string) (string, bool) {
		v, ok := vars[name]
		return v, ok
	})
}

func newManager(t *testing.T, st Store, env map[string]string) *Manager {
	t.Helper()
	m, err := New(st, envOf(env))
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Load(context.Background()); err != nil {
		t.Fatal(err)
	}
	return m
}

func setting(t *testing.T, m *Manager, name string) Setting {
	t.Helper()
	for _, s := range m.List() {
		if s.Name == name {
			return s
		}
	}
	t.Fatalf("%s not listed", name)
	return Setting{}
}

func saved(t *testing.T, st store.Store) map[string]string {
	t.Helper()
	rows, err := st.ListSettings(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]string{}
	for _, r := range rows {
		out[r.Name] = r.Value
	}
	return out
}

func TestLookupOrderAndSources(t *testing.T) {
	ctx := context.Background()
	st := storetest.New(t)
	if err := st.SetSetting(ctx, "HOOKD_WORKERS", "3"); err != nil {
		t.Fatal(err)
	}
	// A row for a setting the UI cannot change is ignored.
	if err := st.SetSetting(ctx, "HOOKD_LISTEN_ADDR", ":1"); err != nil {
		t.Fatal(err)
	}
	m := newManager(t, st, map[string]string{
		"HOOKD_WORKERS":            "5",
		"HOOKD_POLL_INTERVAL":      "2s",
		"HOOKD_LOG_LEVEL":          "",
		"HOOKD_DB_PASSWORD":        "hunter2",
		"HOOKD_LISTEN_ADDR":        ":9090",
		"HOOKD_DELIVERY_LEASE":     " 2m ",
		"HOOKD_IDEMPOTENCY_WINDOW": "",
	})

	c := m.Current()
	if c.Workers != 3 || c.PollInterval != 2*time.Second || c.ListenAddr != ":9090" || c.DeliveryLease != 2*time.Minute {
		t.Fatalf("Current() = %+v", c)
	}

	tests := []struct {
		name, value, source string
		editable            bool
	}{
		{"HOOKD_WORKERS", "3", SourceUI, true},
		{"HOOKD_POLL_INTERVAL", "2s", SourceEnvironment, true},
		{"HOOKD_DELIVERY_LEASE", "2m", SourceEnvironment, true},
		{"HOOKD_DELIVERY_TIMEOUT", "15s", SourceDefault, true},
		{"HOOKD_IDEMPOTENCY_WINDOW", "24h", SourceDefault, true}, // empty counts as unset
		{"HOOKD_LOG_LEVEL", "info", SourceDefault, true},
		{"HOOKD_LISTEN_ADDR", ":9090", SourceEnvironment, false},
		{"HOOKD_DB_PASSWORD", PasswordSet, SourceEnvironment, false},
		{"HOOKD_DB_DRIVER", "sqlite", SourceDefault, false},
		{"HOOKD_RETRY_SCHEDULE", "5s,5m,30m,2h,5h,10h,10h", SourceDefault, true},
		{"HOOKD_RETENTION", "720h", SourceDefault, false},
	}
	for _, tt := range tests {
		got := setting(t, m, tt.name)
		if got.Value != tt.value || got.Source != tt.source || got.Editable != tt.editable {
			t.Errorf("%s = %+v, want value %q source %q editable %v", tt.name, got, tt.value, tt.source, tt.editable)
		}
	}

	editable := 0
	for _, s := range m.List() {
		if s.Editable {
			editable++
		}
		if strings.Contains(s.Value, "hunter2") {
			t.Errorf("%s shows the password", s.Name)
		}
	}
	if editable != 8 {
		t.Errorf("%d editable settings, want 8", editable)
	}

	m2 := newManager(t, storetest.New(t), nil)
	if got := setting(t, m2, "HOOKD_DB_PASSWORD"); got.Value != PasswordNotSet || got.Source != SourceDefault {
		t.Errorf("unset password = %+v", got)
	}
}

func TestNewRefusesInvalidEnvironment(t *testing.T) {
	_, err := New(storetest.New(t), envOf(map[string]string{"HOOKD_WORKERS": "zero"}))
	if err == nil || !strings.Contains(err.Error(), "HOOKD_WORKERS") {
		t.Fatalf("New = %v, want an error naming HOOKD_WORKERS", err)
	}
}

func TestSet(t *testing.T) {
	ctx := context.Background()
	st := storetest.New(t)
	m := newManager(t, st, map[string]string{"HOOKD_WORKERS": "5"})

	if err := m.Set(ctx, "HOOKD_WORKERS", " 2 "); err != nil {
		t.Fatal(err)
	}
	if m.Current().Workers != 2 || setting(t, m, "HOOKD_WORKERS").Source != SourceUI {
		t.Fatalf("after Set: %+v", setting(t, m, "HOOKD_WORKERS"))
	}
	if got := saved(t, st); got["HOOKD_WORKERS"] != "2" {
		t.Fatalf("saved = %v", got)
	}

	if err := m.Set(ctx, "HOOKD_LOG_LEVEL", "debug"); err != nil {
		t.Fatal(err)
	}
	if m.LevelVar().Level() != slog.LevelDebug {
		t.Fatalf("level = %v, want debug", m.LevelVar().Level())
	}

	refused := []struct {
		name, value, want string
	}{
		{"HOOKD_WORKERS", "-1", "HOOKD_WORKERS"},
		{"HOOKD_WORKERS", "  ", "a value is required"},
		{"HOOKD_POLL_INTERVAL", "soon", `a positive duration`},
		// The lease must stay longer than the timeout (default 15s).
		{"HOOKD_DELIVERY_LEASE", "15s", "must be longer than HOOKD_DELIVERY_TIMEOUT"},
		{"HOOKD_DELIVERY_TIMEOUT", "1m", "must be longer than HOOKD_DELIVERY_TIMEOUT"},
	}
	for _, tt := range refused {
		err := m.Set(ctx, tt.name, tt.value)
		var inv *InvalidError
		if !errors.As(err, &inv) || !strings.Contains(err.Error(), tt.want) {
			t.Errorf("Set(%s, %q) = %v, want an InvalidError containing %q", tt.name, tt.value, err, tt.want)
		}
	}

	err := m.Set(ctx, "HOOKD_LISTEN_ADDR", ":9000")
	if !errors.Is(err, ErrNotEditable) {
		t.Errorf("Set(HOOKD_LISTEN_ADDR) = %v, want ErrNotEditable", err)
	}
	if err := m.Set(ctx, "HOOKD_NOPE", "1"); !errors.Is(err, ErrNotEditable) {
		t.Errorf("Set(HOOKD_NOPE) = %v, want ErrNotEditable", err)
	}

	want := map[string]string{"HOOKD_WORKERS": "2", "HOOKD_LOG_LEVEL": "debug"}
	got := saved(t, st)
	if len(got) != len(want) || got["HOOKD_WORKERS"] != "2" || got["HOOKD_LOG_LEVEL"] != "debug" {
		t.Fatalf("saved after refusals = %v, want %v", got, want)
	}
	c := m.Current()
	if c.Workers != 2 || c.DeliveryLease != time.Minute || c.DeliveryTimeout != 15*time.Second || c.ListenAddr != ":8080" {
		t.Fatalf("Current() after refusals = %+v", c)
	}

	// Raising the lease first makes the longer timeout valid.
	if err := m.Set(ctx, "HOOKD_DELIVERY_LEASE", "2m"); err != nil {
		t.Fatal(err)
	}
	if err := m.Set(ctx, "HOOKD_DELIVERY_TIMEOUT", "1m"); err != nil {
		t.Fatal(err)
	}
}

func TestReset(t *testing.T) {
	ctx := context.Background()
	st := storetest.New(t)
	m := newManager(t, st, map[string]string{"HOOKD_WORKERS": "5"})

	for _, kv := range [][2]string{{"HOOKD_WORKERS", "2"}, {"HOOKD_MAX_PAYLOAD_BYTES", "100"}} {
		if err := m.Set(ctx, kv[0], kv[1]); err != nil {
			t.Fatal(err)
		}
	}
	if err := m.Reset(ctx, "HOOKD_WORKERS"); err != nil {
		t.Fatal(err)
	}
	if got := setting(t, m, "HOOKD_WORKERS"); got.Value != "5" || got.Source != SourceEnvironment {
		t.Fatalf("after reset to environment: %+v", got)
	}
	if err := m.Reset(ctx, "HOOKD_MAX_PAYLOAD_BYTES"); err != nil {
		t.Fatal(err)
	}
	if got := setting(t, m, "HOOKD_MAX_PAYLOAD_BYTES"); got.Value != "262144" || got.Source != SourceDefault {
		t.Fatalf("after reset to default: %+v", got)
	}
	if got := saved(t, st); len(got) != 0 {
		t.Fatalf("saved after resets = %v", got)
	}
	// Resetting a setting that has no override is not an error.
	if err := m.Reset(ctx, "HOOKD_WORKERS"); err != nil {
		t.Fatal(err)
	}
	if err := m.Reset(ctx, "HOOKD_DB_PATH"); !errors.Is(err, ErrNotEditable) {
		t.Fatalf("Reset(HOOKD_DB_PATH) = %v, want ErrNotEditable", err)
	}

	// A reset that would leave the lease no longer than the timeout is refused.
	if err := m.Set(ctx, "HOOKD_DELIVERY_TIMEOUT", "5s"); err != nil {
		t.Fatal(err)
	}
	if err := m.Set(ctx, "HOOKD_DELIVERY_LEASE", "10s"); err != nil {
		t.Fatal(err)
	}
	var inv *InvalidError
	if err := m.Reset(ctx, "HOOKD_DELIVERY_TIMEOUT"); !errors.As(err, &inv) {
		t.Fatalf("Reset(HOOKD_DELIVERY_TIMEOUT) = %v, want an InvalidError", err)
	}
	if got := saved(t, st); got["HOOKD_DELIVERY_TIMEOUT"] != "5s" {
		t.Fatalf("saved after refused reset = %v", got)
	}
}

func TestReload(t *testing.T) {
	ctx := context.Background()
	db := storetest.SQLite(t)
	st := db.New(t)
	other := db.Open(t) // another instance's connection
	m := newManager(t, st, nil)

	// A change saved by another instance is picked up.
	if err := other.SetSetting(ctx, "HOOKD_WORKERS", "4"); err != nil {
		t.Fatal(err)
	}
	if err := other.SetSetting(ctx, "HOOKD_LOG_LEVEL", "error"); err != nil {
		t.Fatal(err)
	}
	if err := m.Load(ctx); err != nil {
		t.Fatal(err)
	}
	if m.Current().Workers != 4 || m.LevelVar().Level() != slog.LevelError || m.Err() != nil {
		t.Fatalf("after reload: %+v, level %v, err %v", m.Current(), m.LevelVar().Level(), m.Err())
	}

	// Overrides that are invalid together keep the last valid configuration.
	if err := other.SetSetting(ctx, "HOOKD_WORKERS", "6"); err != nil {
		t.Fatal(err)
	}
	if err := other.SetSetting(ctx, "HOOKD_DELIVERY_LEASE", "1s"); err != nil {
		t.Fatal(err)
	}
	err := m.Load(ctx)
	if err == nil || !strings.Contains(err.Error(), "HOOKD_DELIVERY_LEASE") {
		t.Fatalf("Load with invalid overrides = %v", err)
	}
	if m.Err() == nil || m.Err().Error() != err.Error() {
		t.Fatalf("Err() = %v, want %v", m.Err(), err)
	}
	if c := m.Current(); c.Workers != 4 || c.DeliveryLease != time.Minute {
		t.Fatalf("kept configuration = %+v", c)
	}
	if got := setting(t, m, "HOOKD_DELIVERY_LEASE"); got.Value != "1m" || got.Source != SourceDefault {
		t.Fatalf("listed lease = %+v", got)
	}

	// Once fixed, the next reload applies them and clears the error.
	if err := other.DeleteSetting(ctx, "HOOKD_DELIVERY_LEASE"); err != nil {
		t.Fatal(err)
	}
	if err := m.Load(ctx); err != nil || m.Err() != nil || m.Current().Workers != 6 {
		t.Fatalf("after fix: %v, %v, %+v", err, m.Err(), m.Current())
	}
}

func TestRun(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	db := storetest.SQLite(t)
	m := newManager(t, db.New(t), nil)
	m.interval = 10 * time.Millisecond
	done := make(chan struct{})
	go func() { m.Run(ctx); close(done) }()
	defer func() { cancel(); <-done }()

	if err := db.Open(t).SetSetting(ctx, "HOOKD_WORKERS", "7"); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for m.Current().Workers != 7 {
		if time.Now().After(deadline) {
			t.Fatal("Run did not pick up the saved change")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestRetrySchedule(t *testing.T) {
	ctx := context.Background()
	st := storetest.New(t)
	m := newManager(t, st, map[string]string{"HOOKD_RETENTION": "48h"})

	// A schedule saved from the UI is applied at once and shown as saved.
	if err := m.Set(ctx, "HOOKD_RETRY_SCHEDULE", "5s, 10s,1m30s"); err != nil {
		t.Fatal(err)
	}
	want := []time.Duration{5 * time.Second, 10 * time.Second, 90 * time.Second}
	if got := m.Current().RetrySchedule; !reflect.DeepEqual(got, want) {
		t.Fatalf("RetrySchedule = %v, want %v", got, want)
	}
	if got := setting(t, m, "HOOKD_RETRY_SCHEDULE"); got.Value != "5s,10s,1m30s" || got.Source != SourceUI || !got.Editable {
		t.Fatalf("after Set: %+v", got)
	}

	if err := m.Set(ctx, "HOOKD_RETRY_SCHEDULE", "none"); err != nil {
		t.Fatal(err)
	}
	if got := m.Current().RetrySchedule; len(got) != 0 {
		t.Fatalf("RetrySchedule after none = %v", got)
	}
	if got := setting(t, m, "HOOKD_RETRY_SCHEDULE"); got.Value != "none" {
		t.Fatalf("after none: %+v", got)
	}

	// 20h + 20h + 4h is 48.4h with jitter, more than the 48h retention. Nothing
	// refused is saved or applied.
	refused := []struct{ value, want string }{
		{"20h,20h,4h", "HOOKD_RETRY_SCHEDULE plus 10%"},
		{"5s,soon", "HOOKD_RETRY_SCHEDULE"},
		{"  ", "a value is required"},
	}
	for _, tt := range refused {
		err := m.Set(ctx, "HOOKD_RETRY_SCHEDULE", tt.value)
		var inv *InvalidError
		if !errors.As(err, &inv) || !strings.Contains(err.Error(), tt.want) {
			t.Errorf("Set(HOOKD_RETRY_SCHEDULE, %q) = %v, want an InvalidError containing %q", tt.value, err, tt.want)
		}
	}
	if got := saved(t, st); len(got) != 1 || got["HOOKD_RETRY_SCHEDULE"] != "none" {
		t.Fatalf("saved after refusals = %v", got)
	}
	if got := m.Current().RetrySchedule; len(got) != 0 {
		t.Fatalf("RetrySchedule after refusals = %v", got)
	}

	if err := m.Reset(ctx, "HOOKD_RETRY_SCHEDULE"); err != nil {
		t.Fatal(err)
	}
	if got := setting(t, m, "HOOKD_RETRY_SCHEDULE"); got.Value != "5s,5m,30m,2h,5h,10h,10h" || got.Source != SourceDefault {
		t.Fatalf("after reset: %+v", got)
	}
}

func TestRetentionNotEditable(t *testing.T) {
	ctx := context.Background()
	st := storetest.New(t)
	// A saved row for HOOKD_RETENTION, which the UI never writes, is ignored.
	if err := st.SetSetting(ctx, "HOOKD_RETENTION", "1h"); err != nil {
		t.Fatal(err)
	}
	m := newManager(t, st, map[string]string{"HOOKD_RETENTION": "1000h"})

	if err := m.Set(ctx, "HOOKD_RETENTION", "2000h"); !errors.Is(err, ErrNotEditable) {
		t.Errorf("Set(HOOKD_RETENTION) = %v, want ErrNotEditable", err)
	}
	if err := m.Reset(ctx, "HOOKD_RETENTION"); !errors.Is(err, ErrNotEditable) {
		t.Errorf("Reset(HOOKD_RETENTION) = %v, want ErrNotEditable", err)
	}
	if got := m.Current().Retention; got != 1000*time.Hour {
		t.Errorf("Retention = %v, want 1000h", got)
	}
	if got := setting(t, m, "HOOKD_RETENTION"); got.Value != "1000h" || got.Source != SourceEnvironment || got.Editable {
		t.Errorf("HOOKD_RETENTION = %+v", got)
	}
	if got := saved(t, st); got["HOOKD_RETENTION"] != "1h" || len(got) != 1 {
		t.Errorf("saved = %v", got)
	}
}
