// Package config reads Hookd's settings from HOOKD_* environment variables.
package config

import (
	"errors"
	"fmt"
	"math"
	"net"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

// Database drivers.
const (
	DriverSQLite   = "sqlite"
	DriverPostgres = "postgres"
)

// Log formats.
const (
	LogFormatText = "text"
	LogFormatJSON = "json"
)

// Config holds every Hookd setting.
type Config struct {
	ListenAddr        string
	DB                DB
	Workers           int
	PollInterval      time.Duration
	DeliveryTimeout   time.Duration
	DeliveryLease     time.Duration
	MaxPayloadBytes   int64
	IdempotencyWindow time.Duration
	AutoMigrate       bool
	LogLevel          string
	LogFormat         string
	AdminUI           bool
	// AdminReceiverURL is the base URL the workers use to reach the built-in test
	// receiver. Empty means derived from ListenAddr; see ReceiverBaseURL.
	AdminReceiverURL string
	// RetrySchedule is the wait before each retry, counted from the previous
	// attempt. Empty means no retries.
	RetrySchedule []time.Duration
	// Retention is how long events, deliveries and attempts are kept.
	Retention time.Duration
}

// DB holds the database settings. Path is used by SQLite; the others by PostgreSQL,
// except MaxConns, which applies to both.
type DB struct {
	Driver   string
	Path     string
	Host     string
	Port     int
	Name     string
	User     string
	Password string
	SSLMode  string
	MaxConns int
}

// Default returns the configuration used when no variable is set.
func Default() Config {
	return Config{
		ListenAddr: ":8080",
		DB: DB{
			Driver:   DriverSQLite,
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
		LogFormat:         LogFormatText,
		AdminUI:           true,
		AdminReceiverURL:  "",
		RetrySchedule:     DefaultRetrySchedule(),
		Retention:         720 * time.Hour,
	}
}

// MaxRetries is the most waits HOOKD_RETRY_SCHEDULE may list.
const MaxRetries = 20

// NoRetries is the HOOKD_RETRY_SCHEDULE value for an empty schedule. An empty
// variable keeps the default, as for every setting.
const NoRetries = "none"

// DefaultRetrySchedule returns the default waits: 8 attempts over about 27½ hours.
func DefaultRetrySchedule() []time.Duration {
	return []time.Duration{
		5 * time.Second,
		5 * time.Minute,
		30 * time.Minute,
		2 * time.Hour,
		5 * time.Hour,
		10 * time.Hour,
		10 * time.Hour,
	}
}

// Load reads the configuration from the process environment.
func Load() (Config, error) {
	return FromLookup(os.LookupEnv)
}

// FromLookup reads the configuration using lookup, which has the signature of
// os.LookupEnv. A variable that is unset or empty keeps its default. All invalid
// values are reported together in the returned error.
func FromLookup(lookup func(string) (string, bool)) (Config, error) {
	c := Default()
	p := parser{lookup: lookup}

	p.str("HOOKD_LISTEN_ADDR", &c.ListenAddr)
	p.oneOf("HOOKD_DB_DRIVER", &c.DB.Driver, DriverSQLite, DriverPostgres)
	p.str("HOOKD_DB_PATH", &c.DB.Path)
	p.str("HOOKD_DB_HOST", &c.DB.Host)
	p.port("HOOKD_DB_PORT", &c.DB.Port)
	p.str("HOOKD_DB_NAME", &c.DB.Name)
	p.str("HOOKD_DB_USER", &c.DB.User)
	p.str("HOOKD_DB_PASSWORD", &c.DB.Password)
	p.oneOf("HOOKD_DB_SSLMODE", &c.DB.SSLMode,
		"disable", "allow", "prefer", "require", "verify-ca", "verify-full")
	p.positiveInt("HOOKD_DB_MAX_CONNS", &c.DB.MaxConns)
	p.positiveInt("HOOKD_WORKERS", &c.Workers)
	p.duration("HOOKD_POLL_INTERVAL", &c.PollInterval)
	p.duration("HOOKD_DELIVERY_TIMEOUT", &c.DeliveryTimeout)
	p.duration("HOOKD_DELIVERY_LEASE", &c.DeliveryLease)
	p.positiveInt64("HOOKD_MAX_PAYLOAD_BYTES", &c.MaxPayloadBytes)
	p.duration("HOOKD_IDEMPOTENCY_WINDOW", &c.IdempotencyWindow)
	p.boolean("HOOKD_AUTO_MIGRATE", &c.AutoMigrate)
	p.oneOf("HOOKD_LOG_LEVEL", &c.LogLevel, "debug", "info", "warn", "error")
	p.oneOf("HOOKD_LOG_FORMAT", &c.LogFormat, LogFormatText, LogFormatJSON)
	p.boolean("HOOKD_ADMIN_UI", &c.AdminUI)
	p.httpURL("HOOKD_ADMIN_RECEIVER_URL", &c.AdminReceiverURL)
	p.durationList("HOOKD_RETRY_SCHEDULE", &c.RetrySchedule)
	p.duration("HOOKD_RETENTION", &c.Retention)

	// A lease that runs out while a delivery is still being sent lets another
	// worker claim it and send it a second time.
	if c.DeliveryLease <= c.DeliveryTimeout {
		p.errs = append(p.errs, fmt.Errorf("  HOOKD_DELIVERY_LEASE=%q: must be longer than HOOKD_DELIVERY_TIMEOUT=%q",
			c.DeliveryLease, c.DeliveryTimeout))
	}
	// No event may be deleted while its idempotency key is still honoured or a
	// retry of it is still waiting, even with each wait 10% longer from jitter.
	if c.Retention <= c.IdempotencyWindow {
		p.errs = append(p.errs, fmt.Errorf("  HOOKD_RETENTION=%q: must be longer than HOOKD_IDEMPOTENCY_WINDOW=%q",
			c.Retention, c.IdempotencyWindow))
	}
	if total := withJitter(c.RetrySchedule); c.Retention <= total {
		p.errs = append(p.errs, fmt.Errorf("  HOOKD_RETENTION=%q: must be longer than the total of HOOKD_RETRY_SCHEDULE plus 10%% (%q)",
			c.Retention, total))
	}

	if len(p.errs) > 0 {
		return Config{}, fmt.Errorf("invalid configuration:\n%w", errors.Join(p.errs...))
	}
	return c, nil
}

// ReceiverBaseURL returns the base URL, without a trailing slash, that the workers
// use to reach the built-in test receiver: AdminReceiverURL when it is set, or else
// http://127.0.0.1:<port> when ListenAddr listens on every interface and
// http://<host>:<port> when it names a host.
func (c Config) ReceiverBaseURL() string {
	if c.AdminReceiverURL != "" {
		return strings.TrimRight(c.AdminReceiverURL, "/")
	}
	host, port, err := net.SplitHostPort(c.ListenAddr)
	if err != nil {
		// The server cannot listen on such an address either.
		return "http://" + strings.TrimRight(c.ListenAddr, "/")
	}
	switch host {
	case "", "0.0.0.0", "::":
		host = "127.0.0.1"
	}
	return "http://" + net.JoinHostPort(host, port)
}

type parser struct {
	lookup func(string) (string, bool)
	errs   []error
}

// get returns the trimmed value of name and whether it is set and non-empty.
func (p *parser) get(name string) (string, bool) {
	v, ok := p.lookup(name)
	v = strings.TrimSpace(v)
	return v, ok && v != ""
}

func (p *parser) fail(name, value, want string) {
	p.errs = append(p.errs, fmt.Errorf("  %s=%q: must be %s", name, value, want))
}

func (p *parser) str(name string, dst *string) {
	if v, ok := p.get(name); ok {
		*dst = v
	}
}

func (p *parser) oneOf(name string, dst *string, allowed ...string) {
	v, ok := p.get(name)
	if !ok {
		return
	}
	for _, a := range allowed {
		if strings.EqualFold(v, a) {
			*dst = a
			return
		}
	}
	p.fail(name, v, "one of "+strings.Join(allowed, ", "))
}

func (p *parser) positiveInt(name string, dst *int) {
	v, ok := p.get(name)
	if !ok {
		return
	}
	n, err := strconv.Atoi(v)
	if err != nil || n <= 0 {
		p.fail(name, v, "a whole number greater than 0")
		return
	}
	*dst = n
}

func (p *parser) positiveInt64(name string, dst *int64) {
	v, ok := p.get(name)
	if !ok {
		return
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil || n <= 0 {
		p.fail(name, v, "a whole number greater than 0")
		return
	}
	*dst = n
}

func (p *parser) port(name string, dst *int) {
	v, ok := p.get(name)
	if !ok {
		return
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 1 || n > 65535 {
		p.fail(name, v, "a port number from 1 to 65535")
		return
	}
	*dst = n
}

func (p *parser) duration(name string, dst *time.Duration) {
	v, ok := p.get(name)
	if !ok {
		return
	}
	d, err := time.ParseDuration(v)
	if err != nil || d <= 0 {
		p.fail(name, v, `a positive duration such as "500ms", "15s" or "24h"`)
		return
	}
	*dst = d
}

// durationList accepts NoRetries or up to MaxRetries comma-separated positive
// durations.
func (p *parser) durationList(name string, dst *[]time.Duration) {
	v, ok := p.get(name)
	if !ok {
		return
	}
	if strings.EqualFold(v, NoRetries) {
		*dst = []time.Duration{}
		return
	}
	want := fmt.Sprintf(`%q or up to %d comma-separated positive durations such as "5s,5m,30m"`, NoRetries, MaxRetries)
	parts := strings.Split(v, ",")
	if len(parts) > MaxRetries {
		p.fail(name, v, want)
		return
	}
	list := make([]time.Duration, 0, len(parts))
	for _, s := range parts {
		d, err := time.ParseDuration(strings.TrimSpace(s))
		if err != nil || d <= 0 {
			p.fail(name, v, want)
			return
		}
		list = append(list, d)
	}
	*dst = list
}

// withJitter returns the sum of the waits plus 10%, without overflowing.
func withJitter(waits []time.Duration) time.Duration {
	const max = time.Duration(math.MaxInt64)
	var total time.Duration
	for _, w := range waits {
		w += w / 10
		if w < 0 || total > max-w {
			return max
		}
		total += w
	}
	return total
}

func (p *parser) boolean(name string, dst *bool) {
	v, ok := p.get(name)
	if !ok {
		return
	}
	b, err := strconv.ParseBool(v)
	if err != nil {
		p.fail(name, v, "true or false")
		return
	}
	*dst = b
}

// httpURL accepts an absolute http or https URL with a host.
func (p *parser) httpURL(name string, dst *string) {
	v, ok := p.get(name)
	if !ok {
		return
	}
	u, err := url.Parse(v)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		p.fail(name, v, "an absolute http or https URL")
		return
	}
	*dst = v
}
