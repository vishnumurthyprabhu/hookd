package config

import (
	"bytes"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"
)

func env(vars map[string]string) func(string) (string, bool) {
	return func(name string) (string, bool) {
		v, ok := vars[name]
		return v, ok
	}
}

func TestDefaults(t *testing.T) {
	got, err := FromLookup(env(nil))
	if err != nil {
		t.Fatalf("FromLookup: %v", err)
	}
	want := Config{
		ListenAddr: ":8080",
		DB: DB{
			Driver:   "sqlite",
			Path:     "./hookd.db",
			Host:     "localhost",
			Port:     5432,
			Name:     "hookd",
			User:     "hookd",
			Password: "",
			SSLMode:  "disable",
			MaxConns: 10,
		},
		Workers:           8,
		PollInterval:      time.Second,
		DeliveryTimeout:   15 * time.Second,
		DeliveryLease:     60 * time.Second,
		MaxPayloadBytes:   262144,
		IdempotencyWindow: 24 * time.Hour,
		AutoMigrate:       true,
		LogLevel:          "info",
		LogFormat:         "text",
		AdminUI:           true,
		AdminReceiverURL:  "",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("defaults:\n got  %+v\n want %+v", got, want)
	}
}

func TestEmptyValueKeepsDefault(t *testing.T) {
	got, err := FromLookup(env(map[string]string{"HOOKD_WORKERS": "", "HOOKD_DB_DRIVER": "  "}))
	if err != nil {
		t.Fatalf("FromLookup: %v", err)
	}
	if got.Workers != 8 || got.DB.Driver != "sqlite" {
		t.Errorf("got workers=%d driver=%q, want defaults", got.Workers, got.DB.Driver)
	}
}

// settings lists every variable with a valid override, the value it must produce,
// and invalid values that must be rejected.
var settings = []struct {
	name    string
	value   string
	get     func(Config) any
	want    any
	invalid []string
}{
	{"HOOKD_LISTEN_ADDR", "127.0.0.1:9000", func(c Config) any { return c.ListenAddr }, "127.0.0.1:9000", nil},
	{"HOOKD_DB_DRIVER", "postgres", func(c Config) any { return c.DB.Driver }, "postgres", []string{"mysql", "sqlite3"}},
	{"HOOKD_DB_PATH", ":memory:", func(c Config) any { return c.DB.Path }, ":memory:", nil},
	{"HOOKD_DB_HOST", "db.internal", func(c Config) any { return c.DB.Host }, "db.internal", nil},
	{"HOOKD_DB_PORT", "6543", func(c Config) any { return c.DB.Port }, 6543, []string{"abc", "0", "65536", "-1"}},
	{"HOOKD_DB_NAME", "hookd_prod", func(c Config) any { return c.DB.Name }, "hookd_prod", nil},
	{"HOOKD_DB_USER", "svc", func(c Config) any { return c.DB.User }, "svc", nil},
	{"HOOKD_DB_PASSWORD", "s3cret", func(c Config) any { return c.DB.Password }, "s3cret", nil},
	{"HOOKD_DB_SSLMODE", "verify-full", func(c Config) any { return c.DB.SSLMode }, "verify-full", []string{"on", "true"}},
	{"HOOKD_DB_MAX_CONNS", "25", func(c Config) any { return c.DB.MaxConns }, 25, []string{"many", "0", "-3", "1.5"}},
	{"HOOKD_WORKERS", "2", func(c Config) any { return c.Workers }, 2, []string{"eight", "0", "-1"}},
	{"HOOKD_POLL_INTERVAL", "250ms", func(c Config) any { return c.PollInterval }, 250 * time.Millisecond, []string{"1", "soon", "0s", "-1s"}},
	{"HOOKD_DELIVERY_TIMEOUT", "3s", func(c Config) any { return c.DeliveryTimeout }, 3 * time.Second, []string{"15", "fast", "0s"}},
	{"HOOKD_DELIVERY_LEASE", "2m", func(c Config) any { return c.DeliveryLease }, 2 * time.Minute, []string{"60", "-5s"}},
	{"HOOKD_MAX_PAYLOAD_BYTES", "1024", func(c Config) any { return c.MaxPayloadBytes }, int64(1024), []string{"1MB", "0", "-1"}},
	{"HOOKD_IDEMPOTENCY_WINDOW", "1h30m", func(c Config) any { return c.IdempotencyWindow }, 90 * time.Minute, []string{"1d", "0"}},
	{"HOOKD_AUTO_MIGRATE", "false", func(c Config) any { return c.AutoMigrate }, false, []string{"yes", "maybe"}},
	{"HOOKD_LOG_LEVEL", "DEBUG", func(c Config) any { return c.LogLevel }, "debug", []string{"trace", "verbose"}},
	{"HOOKD_LOG_FORMAT", "json", func(c Config) any { return c.LogFormat }, "json", []string{"xml", "logfmt"}},
	{"HOOKD_ADMIN_UI", "false", func(c Config) any { return c.AdminUI }, false, []string{"off", "nope"}},
	{"HOOKD_ADMIN_RECEIVER_URL", "https://hookd.internal:8443/", func(c Config) any { return c.AdminReceiverURL }, "https://hookd.internal:8443/",
		[]string{"hookd.internal:8080", "/admin", "ftp://hookd.internal", "http://", "http//hookd.internal", "http://[::1"}},
}

func TestEverySettingIsCovered(t *testing.T) {
	// Every field of Config and DB must have an entry in settings.
	fields := reflect.TypeOf(Config{}).NumField() - 1 + reflect.TypeOf(DB{}).NumField()
	if len(settings) != fields {
		t.Errorf("settings table has %d entries, config has %d settings", len(settings), fields)
	}
}

func TestOverrides(t *testing.T) {
	for _, s := range settings {
		t.Run(s.name, func(t *testing.T) {
			c, err := FromLookup(env(map[string]string{s.name: s.value}))
			if err != nil {
				t.Fatalf("FromLookup: %v", err)
			}
			if got := s.get(c); got != s.want {
				t.Errorf("%s=%q gave %v, want %v", s.name, s.value, got, s.want)
			}
			if got := s.get(Default()); got == s.want {
				t.Errorf("override value %v equals the default, so the test proves nothing", got)
			}
		})
	}
}

func TestInvalidValues(t *testing.T) {
	for _, s := range settings {
		for _, bad := range s.invalid {
			t.Run(s.name+"="+bad, func(t *testing.T) {
				_, err := FromLookup(env(map[string]string{s.name: bad}))
				if err == nil {
					t.Fatalf("%s=%q was accepted", s.name, bad)
				}
				msg := err.Error()
				if !strings.Contains(msg, s.name) || !strings.Contains(msg, bad) {
					t.Errorf("error %q does not name the variable and the value", msg)
				}
			})
		}
	}
}

func TestInvalidValuesReportedTogether(t *testing.T) {
	_, err := FromLookup(env(map[string]string{
		"HOOKD_POLL_INTERVAL": "often",
		"HOOKD_DB_DRIVER":     "oracle",
	}))
	if err == nil {
		t.Fatal("invalid configuration was accepted")
	}
	for _, want := range []string{"HOOKD_POLL_INTERVAL", "HOOKD_DB_DRIVER", "one of sqlite, postgres", "duration"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %q", err, want)
		}
	}
}

func TestLeaseMustBeLongerThanTimeout(t *testing.T) {
	for _, vars := range []map[string]string{
		{"HOOKD_DELIVERY_LEASE": "10s"},
		{"HOOKD_DELIVERY_LEASE": "15s"},
		{"HOOKD_DELIVERY_TIMEOUT": "90s"},
		{"HOOKD_DELIVERY_LEASE": "30s", "HOOKD_DELIVERY_TIMEOUT": "30s"},
	} {
		_, err := FromLookup(env(vars))
		if err == nil {
			t.Errorf("%v was accepted", vars)
			continue
		}
		for _, want := range []string{"HOOKD_DELIVERY_LEASE", "HOOKD_DELIVERY_TIMEOUT", "longer than"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("%v: error %q does not mention %q", vars, err, want)
			}
		}
	}

	c, err := FromLookup(env(map[string]string{"HOOKD_DELIVERY_LEASE": "31s", "HOOKD_DELIVERY_TIMEOUT": "30s"}))
	if err != nil {
		t.Fatalf("lease just over timeout rejected: %v", err)
	}
	if c.DeliveryLease != 31*time.Second || c.DeliveryTimeout != 30*time.Second {
		t.Errorf("got lease=%v timeout=%v", c.DeliveryLease, c.DeliveryTimeout)
	}
}

func TestReceiverBaseURL(t *testing.T) {
	for _, tc := range []struct {
		vars map[string]string
		want string
	}{
		{nil, "http://127.0.0.1:8080"},
		{map[string]string{"HOOKD_LISTEN_ADDR": ":9000"}, "http://127.0.0.1:9000"},
		{map[string]string{"HOOKD_LISTEN_ADDR": "0.0.0.0:9000"}, "http://127.0.0.1:9000"},
		{map[string]string{"HOOKD_LISTEN_ADDR": "[::]:9000"}, "http://127.0.0.1:9000"},
		{map[string]string{"HOOKD_LISTEN_ADDR": "10.0.0.5:9000"}, "http://10.0.0.5:9000"},
		{map[string]string{"HOOKD_LISTEN_ADDR": "localhost:9000"}, "http://localhost:9000"},
		{map[string]string{"HOOKD_LISTEN_ADDR": "[::1]:9000"}, "http://[::1]:9000"},
		{map[string]string{"HOOKD_ADMIN_RECEIVER_URL": "https://hookd.internal:8443"}, "https://hookd.internal:8443"},
		{map[string]string{"HOOKD_ADMIN_RECEIVER_URL": "http://hookd-1.svc:8080/"}, "http://hookd-1.svc:8080"},
		{map[string]string{"HOOKD_ADMIN_RECEIVER_URL": "http://lb.example/hookd/", "HOOKD_LISTEN_ADDR": "0.0.0.0:9000"},
			"http://lb.example/hookd"},
	} {
		c, err := FromLookup(env(tc.vars))
		if err != nil {
			t.Fatalf("%v: FromLookup: %v", tc.vars, err)
		}
		if got := c.ReceiverBaseURL(); got != tc.want {
			t.Errorf("%v: ReceiverBaseURL() = %q, want %q", tc.vars, got, tc.want)
		}
	}
}

func TestLoadReadsProcessEnvironment(t *testing.T) {
	t.Setenv("HOOKD_WORKERS", "3")
	c, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.Workers != 3 {
		t.Errorf("Workers = %d, want 3", c.Workers)
	}
}

func TestLoggerLevel(t *testing.T) {
	c := Default()
	c.LogLevel = "warn"
	var buf bytes.Buffer
	log := c.NewLogger(&buf)
	log.Info("hidden")
	log.Warn("shown")
	if out := buf.String(); strings.Contains(out, "hidden") || !strings.Contains(out, "shown") {
		t.Errorf("unexpected log output at warn level: %q", out)
	}
}

func TestLoggerFormat(t *testing.T) {
	c := Default()
	var buf bytes.Buffer
	c.NewLogger(&buf).Info("hello", "k", "v")
	if out := buf.String(); !strings.Contains(out, "msg=hello") || !strings.Contains(out, "k=v") {
		t.Errorf("text output = %q", out)
	}

	c.LogFormat = LogFormatJSON
	buf.Reset()
	c.NewLogger(&buf).Info("hello", "k", "v")
	var rec map[string]any
	if err := json.Unmarshal(buf.Bytes(), &rec); err != nil {
		t.Fatalf("json output %q: %v", buf.String(), err)
	}
	if rec["msg"] != "hello" || rec["k"] != "v" {
		t.Errorf("json record = %v", rec)
	}
}

func TestLoggerDebug(t *testing.T) {
	c := Default()
	var buf bytes.Buffer
	c.NewLogger(&buf).Debug("quiet")
	if buf.Len() != 0 {
		t.Errorf("debug logged at default info level: %q", buf.String())
	}
	c.LogLevel = "debug"
	c.NewLogger(&buf).Debug("loud")
	if !strings.Contains(buf.String(), "loud") {
		t.Errorf("debug not logged at debug level: %q", buf.String())
	}
}
