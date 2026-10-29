package store

import (
	"context"
	"database/sql"
	"fmt"
	"net/url"
	"strconv"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib" // registers the "pgx" driver
	_ "modernc.org/sqlite"             // registers the "sqlite" driver

	"hookd/internal/config"
)

// SQLStore is the Store implementation for SQLite and PostgreSQL.
type SQLStore struct {
	db  *sql.DB
	d   dialect
	now func() time.Time
	// idempotencyWindow returns how long an event's idempotency key is
	// honoured. It is called for each event, so a change applies at once.
	idempotencyWindow func() time.Duration
	// pgSchema, if set, is the PostgreSQL schema the store works in.
	pgSchema string
}

var _ Store = (*SQLStore)(nil)

// Option changes how Open sets up a SQLStore.
type Option func(*SQLStore)

// WithClock makes the store read the current time from now instead of
// time.Now. Tests use it to control timestamps, windows and leases.
func WithClock(now func() time.Time) Option {
	return func(s *SQLStore) { s.now = now }
}

// WithIdempotencyWindow sets how long CreateEvent honours an idempotency key
// (HOOKD_IDEMPOTENCY_WINDOW). The default is config.Default's.
func WithIdempotencyWindow(d time.Duration) Option {
	return WithIdempotencyWindowFunc(func() time.Duration { return d })
}

// WithIdempotencyWindowFunc makes CreateEvent read the idempotency window from
// window each time it stores an event, so a window changed while Hookd runs
// applies to the next event.
func WithIdempotencyWindowFunc(window func() time.Duration) Option {
	return func(s *SQLStore) { s.idempotencyWindow = window }
}

// WithPostgresSchema makes a PostgreSQL store create and use its tables in
// schema, which must already exist, instead of the user's default search
// path. Tests use it to give each run a schema of its own. SQLite ignores it.
func WithPostgresSchema(schema string) Option {
	return func(s *SQLStore) { s.pgSchema = schema }
}

// Open connects to the database described by cfg and checks that it can be
// reached. It does not apply migrations; call Migrate for that.
func Open(ctx context.Context, cfg config.DB, opts ...Option) (*SQLStore, error) {
	defaultWindow := config.Default().IdempotencyWindow
	s := &SQLStore{now: time.Now, idempotencyWindow: func() time.Duration { return defaultWindow }}
	for _, opt := range opts {
		opt(s)
	}

	var (
		dsn      string
		maxConns = cfg.MaxConns
	)
	switch cfg.Driver {
	case config.DriverSQLite:
		s.d, dsn = sqliteDialect, sqliteDSN(cfg.Path)
		if cfg.Path == ":memory:" {
			// Each connection to :memory: would get its own empty database.
			maxConns = 1
		}
	case config.DriverPostgres:
		s.d, dsn = postgresDialect, PostgresDSN(cfg, s.pgSchema)
	default:
		return nil, fmt.Errorf("store: unknown database driver %q", cfg.Driver)
	}

	db, err := sql.Open(s.d.driver, dsn)
	if err != nil {
		return nil, fmt.Errorf("store: open %s database: %w", s.d.name, err)
	}
	if maxConns > 0 {
		db.SetMaxOpenConns(maxConns)
		db.SetMaxIdleConns(maxConns)
	}
	s.db = db
	if err := s.Ping(ctx); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

// sqliteDSN turns on foreign keys, waits for locks instead of failing, and
// starts transactions as writers so that two of them cannot deadlock while
// upgrading from read to write.
func sqliteDSN(path string) string {
	q := url.Values{}
	q.Add("_pragma", "foreign_keys(1)")
	q.Add("_pragma", "busy_timeout(5000)")
	if path != ":memory:" {
		q.Add("_pragma", "journal_mode(WAL)")
	}
	q.Set("_txlock", "immediate")
	return path + "?" + q.Encode()
}

// PostgresDSN returns the connection string for the "pgx" driver described by
// cfg. If schema is not empty, connections use it as their search path.
func PostgresDSN(cfg config.DB, schema string) string {
	q := url.Values{"sslmode": {cfg.SSLMode}}
	if schema != "" {
		// pgx sends unknown parameters to the server as settings.
		q.Set("search_path", schema)
	}
	u := url.URL{
		Scheme:   "postgres",
		User:     url.UserPassword(cfg.User, cfg.Password),
		Host:     cfg.Host + ":" + strconv.Itoa(cfg.Port),
		Path:     "/" + cfg.Name,
		RawQuery: q.Encode(),
	}
	if cfg.Password == "" {
		u.User = url.User(cfg.User)
	}
	return u.String()
}

// Ping implements Store.
func (s *SQLStore) Ping(ctx context.Context) error {
	if err := s.db.PingContext(ctx); err != nil {
		return fmt.Errorf("store: reach %s database: %w", s.d.name, err)
	}
	return nil
}

// Close implements Store.
func (s *SQLStore) Close() error {
	return s.db.Close()
}

// nowUTC returns the store's current time, as it will be stored.
func (s *SQLStore) nowUTC() time.Time {
	return s.now().UTC().Truncate(time.Microsecond)
}

func (s *SQLStore) exec(ctx context.Context, q queryer, query string, args ...any) (sql.Result, error) {
	return q.ExecContext(ctx, s.d.rebind(query), args...)
}

func (s *SQLStore) query(ctx context.Context, q queryer, query string, args ...any) (*sql.Rows, error) {
	return q.QueryContext(ctx, s.d.rebind(query), args...)
}

func (s *SQLStore) queryRow(ctx context.Context, q queryer, query string, args ...any) *sql.Row {
	return q.QueryRowContext(ctx, s.d.rebind(query), args...)
}

// queryer is satisfied by both *sql.DB and *sql.Tx.
type queryer interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// inTx runs fn in a transaction, committing if it returns nil and rolling
// back otherwise.
func (s *SQLStore) inTx(ctx context.Context, fn func(tx *sql.Tx) error) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("store: begin transaction: %w", err)
	}
	if err := fn(tx); err != nil {
		tx.Rollback()
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("store: commit: %w", err)
	}
	return nil
}
