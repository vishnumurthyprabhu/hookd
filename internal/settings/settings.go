// Package settings keeps Hookd's effective configuration while it runs: the
// environment plus the overrides saved from the admin UI in the settings table.
// The values are checked by the start-up parser, config.FromLookup, so a value
// the UI accepts is one Hookd would accept at start-up.
package settings

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"hookd/internal/config"
	"hookd/internal/store"
)

// ReloadInterval is how often Run reads the saved overrides again, so that a
// change made on one instance reaches every instance.
const ReloadInterval = 5 * time.Second

// Sources of a setting's value.
const (
	SourceDefault     = "default"
	SourceEnvironment = "environment"
	SourceUI          = "UI"
)

// The value shown for HOOKD_DB_PASSWORD instead of the password.
const (
	PasswordSet    = "set"
	PasswordNotSet = "not set"
)

// ErrNotEditable is returned by Set and Reset for a setting that can only be
// set with an environment variable.
var ErrNotEditable = errors.New("settings: this setting is set with environment variables and cannot be changed here")

// InvalidError is returned by Set and Reset when the configuration with the
// change would be invalid. Its message is the start-up parser's.
type InvalidError struct {
	Err error
}

func (e *InvalidError) Error() string { return e.Err.Error() }
func (e *InvalidError) Unwrap() error { return e.Err }

// Store is the part of store.Store the manager uses.
type Store interface {
	ListSettings(ctx context.Context) ([]store.Setting, error)
	SetSetting(ctx context.Context, name, value string) error
	DeleteSetting(ctx context.Context, name string) error
}

// Setting is one HOOKD_* setting as the settings page shows it.
type Setting struct {
	Name string
	// Value is the effective value. For HOOKD_DB_PASSWORD it is PasswordSet
	// or PasswordNotSet.
	Value string
	// Source is SourceDefault, SourceEnvironment or SourceUI.
	Source   string
	Editable bool
}

type definition struct {
	name     string
	editable bool
	value    func(config.Config) string
}

// definitions lists every setting in the order config.FromLookup reads them.
var definitions = []definition{
	{"HOOKD_LISTEN_ADDR", false, func(c config.Config) string { return c.ListenAddr }},
	{"HOOKD_DB_DRIVER", false, func(c config.Config) string { return c.DB.Driver }},
	{"HOOKD_DB_PATH", false, func(c config.Config) string { return c.DB.Path }},
	{"HOOKD_DB_HOST", false, func(c config.Config) string { return c.DB.Host }},
	{"HOOKD_DB_PORT", false, func(c config.Config) string { return strconv.Itoa(c.DB.Port) }},
	{"HOOKD_DB_NAME", false, func(c config.Config) string { return c.DB.Name }},
	{"HOOKD_DB_USER", false, func(c config.Config) string { return c.DB.User }},
	{"HOOKD_DB_PASSWORD", false, func(c config.Config) string {
		if c.DB.Password != "" {
			return PasswordSet
		}
		return PasswordNotSet
	}},
	{"HOOKD_DB_SSLMODE", false, func(c config.Config) string { return c.DB.SSLMode }},
	{"HOOKD_DB_MAX_CONNS", false, func(c config.Config) string { return strconv.Itoa(c.DB.MaxConns) }},
	{"HOOKD_WORKERS", true, func(c config.Config) string { return strconv.Itoa(c.Workers) }},
	{"HOOKD_POLL_INTERVAL", true, func(c config.Config) string { return formatDuration(c.PollInterval) }},
	{"HOOKD_DELIVERY_TIMEOUT", true, func(c config.Config) string { return formatDuration(c.DeliveryTimeout) }},
	{"HOOKD_DELIVERY_LEASE", true, func(c config.Config) string { return formatDuration(c.DeliveryLease) }},
	{"HOOKD_MAX_PAYLOAD_BYTES", true, func(c config.Config) string { return strconv.FormatInt(c.MaxPayloadBytes, 10) }},
	{"HOOKD_IDEMPOTENCY_WINDOW", true, func(c config.Config) string { return formatDuration(c.IdempotencyWindow) }},
	{"HOOKD_AUTO_MIGRATE", false, func(c config.Config) string { return strconv.FormatBool(c.AutoMigrate) }},
	{"HOOKD_LOG_LEVEL", true, func(c config.Config) string { return c.LogLevel }},
	{"HOOKD_LOG_FORMAT", false, func(c config.Config) string { return c.LogFormat }},
	{"HOOKD_ADMIN_UI", false, func(c config.Config) string { return strconv.FormatBool(c.AdminUI) }},
	{"HOOKD_ADMIN_RECEIVER_URL", false, func(c config.Config) string { return c.AdminReceiverURL }},
	{"HOOKD_RETRY_SCHEDULE", true, func(c config.Config) string { return formatSchedule(c.RetrySchedule) }},
	{"HOOKD_RETENTION", false, func(c config.Config) string { return formatDuration(c.Retention) }},
}

// Editable reports whether name can be changed from the UI.
func Editable(name string) bool {
	for _, d := range definitions {
		if d.name == name {
			return d.editable
		}
	}
	return false
}

// formatDuration drops the zero units time.Duration.String leaves at the end,
// so 24h is shown as "24h" rather than "24h0m0s".
func formatDuration(d time.Duration) string {
	s := d.String()
	if strings.HasSuffix(s, "m0s") {
		s = strings.TrimSuffix(s, "0s")
	}
	if strings.HasSuffix(s, "h0m") {
		s = strings.TrimSuffix(s, "0m")
	}
	return s
}

// formatSchedule shows the waits as comma-separated durations, or config.NoRetries
// when there are none, in the form HOOKD_RETRY_SCHEDULE accepts.
func formatSchedule(waits []time.Duration) string {
	if len(waits) == 0 {
		return config.NoRetries
	}
	parts := make([]string, len(waits))
	for i, w := range waits {
		parts[i] = formatDuration(w)
	}
	return strings.Join(parts, ",")
}

// Option configures a Manager.
type Option func(*Manager)

// WithLookupEnv replaces os.LookupEnv as the source of environment variables.
func WithLookupEnv(lookup func(string) (string, bool)) Option {
	return func(m *Manager) { m.lookupEnv = lookup }
}

// WithLogger sets the logger Run reports invalid saved settings to.
func WithLogger(log *slog.Logger) Option {
	return func(m *Manager) { m.log = log }
}

// Manager holds the effective configuration. It is safe for concurrent use.
type Manager struct {
	st        Store
	lookupEnv func(string) (string, bool)
	log       *slog.Logger
	interval  time.Duration
	level     slog.LevelVar

	// write serializes Set and Reset, so each checks against the overrides
	// the other saved.
	write sync.Mutex

	mu        sync.RWMutex
	cfg       config.Config
	overrides map[string]string // the editable overrides cfg was built with
	loadErr   error
}

// New returns a manager whose configuration is taken from the environment
// alone. It returns the parser's error if the environment is invalid. Call
// Load to apply the saved overrides.
func New(st Store, opts ...Option) (*Manager, error) {
	m := &Manager{
		st:        st,
		lookupEnv: os.LookupEnv,
		log:       slog.New(slog.DiscardHandler),
		interval:  ReloadInterval,
	}
	for _, o := range opts {
		o(m)
	}
	cfg, err := m.build(nil)
	if err != nil {
		return nil, err
	}
	m.apply(cfg, nil)
	return m, nil
}

// Current returns the effective configuration. Callers should call it each
// time they need a value rather than keep a copy.
func (m *Manager) Current() config.Config {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.cfg
}

// LevelVar returns the log level, which follows HOOKD_LOG_LEVEL.
func (m *Manager) LevelVar() *slog.LevelVar { return &m.level }

// Err returns the error of the last reload, or nil if the saved overrides
// were valid. While it is not nil the manager keeps the last valid
// configuration.
func (m *Manager) Err() error {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.loadErr
}

// List returns every setting with its effective value and source, in a fixed
// order.
func (m *Manager) List() []Setting {
	m.mu.RLock()
	cfg, overrides := m.cfg, m.overrides
	m.mu.RUnlock()

	out := make([]Setting, 0, len(definitions))
	for _, d := range definitions {
		source := SourceDefault
		if _, ok := overrides[d.name]; ok {
			source = SourceUI
		} else if _, ok := m.env(d.name); ok {
			source = SourceEnvironment
		}
		out = append(out, Setting{Name: d.name, Value: d.value(cfg), Source: source, Editable: d.editable})
	}
	return out
}

// Load reads the saved overrides and applies them. If they make the
// configuration invalid, it keeps the current configuration and returns the
// parser's error, which Err also returns until a later Load succeeds.
func (m *Manager) Load(ctx context.Context) error {
	overrides, err := m.saved(ctx)
	if err != nil {
		return err
	}
	cfg, err := m.build(overrides)
	if err != nil {
		m.mu.Lock()
		m.loadErr = err
		m.mu.Unlock()
		return err
	}
	m.apply(cfg, overrides)
	return nil
}

// Run calls Load every ReloadInterval until ctx is done. An error is logged
// when it first appears or changes, and when the overrides become valid again.
func (m *Manager) Run(ctx context.Context) {
	t := time.NewTicker(m.interval)
	defer t.Stop()
	var last string
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		err := m.Load(ctx)
		if ctx.Err() != nil {
			return
		}
		msg := ""
		if err != nil {
			msg = err.Error()
		}
		switch {
		case msg != "" && msg != last:
			m.log.Error("saved settings not applied; keeping the last valid configuration", "err", err)
		case msg == "" && last != "":
			m.log.Info("saved settings applied")
		}
		last = msg
	}
}

// Set saves value as the UI override of name and applies it at once. Nothing
// is saved if name is not editable (ErrNotEditable) or if the configuration
// with the new value is invalid (*InvalidError).
func (m *Manager) Set(ctx context.Context, name, value string) error {
	if !Editable(name) {
		return ErrNotEditable
	}
	value = strings.TrimSpace(value)
	if value == "" {
		return &InvalidError{Err: fmt.Errorf("%s: a value is required; reset it to go back to the environment or default value", name)}
	}
	return m.change(ctx, name, value, func() error { return m.st.SetSetting(ctx, name, value) })
}

// Reset deletes the UI override of name, so the environment or the default
// applies again. Like Set, it is refused if name is not editable or if the
// configuration without the override is invalid.
func (m *Manager) Reset(ctx context.Context, name string) error {
	if !Editable(name) {
		return ErrNotEditable
	}
	return m.change(ctx, name, "", func() error { return m.st.DeleteSetting(ctx, name) })
}

// change checks the saved overrides with name set to value (or removed when
// value is empty), and only if they are valid calls save and applies them.
func (m *Manager) change(ctx context.Context, name, value string, save func() error) error {
	m.write.Lock()
	defer m.write.Unlock()

	overrides, err := m.saved(ctx)
	if err != nil {
		return err
	}
	if value == "" {
		delete(overrides, name)
	} else {
		overrides[name] = value
	}
	cfg, err := m.build(overrides)
	if err != nil {
		return &InvalidError{Err: err}
	}
	if err := save(); err != nil {
		return err
	}
	m.apply(cfg, overrides)
	return nil
}

// saved returns the editable overrides in the store. Rows for other names,
// which the UI never writes, and empty values are ignored.
func (m *Manager) saved(ctx context.Context) (map[string]string, error) {
	rows, err := m.st.ListSettings(ctx)
	if err != nil {
		return nil, fmt.Errorf("settings: %w", err)
	}
	overrides := make(map[string]string, len(rows))
	for _, r := range rows {
		if v := strings.TrimSpace(r.Value); v != "" && Editable(r.Name) {
			overrides[r.Name] = v
		}
	}
	return overrides, nil
}

// env returns the environment variable name if it is set and not empty, the
// same test config.FromLookup uses.
func (m *Manager) env(name string) (string, bool) {
	v, ok := m.lookupEnv(name)
	return v, ok && strings.TrimSpace(v) != ""
}

// build parses the configuration, taking each variable from overrides first
// and from the environment otherwise.
func (m *Manager) build(overrides map[string]string) (config.Config, error) {
	return config.FromLookup(func(name string) (string, bool) {
		if v, ok := overrides[name]; ok {
			return v, true
		}
		return m.lookupEnv(name)
	})
}

// apply makes cfg, built with overrides, the effective configuration.
func (m *Manager) apply(cfg config.Config, overrides map[string]string) {
	m.mu.Lock()
	m.cfg, m.overrides, m.loadErr = cfg, overrides, nil
	m.mu.Unlock()
	m.level.Set(cfg.SlogLevel())
}
