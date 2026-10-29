// Package storetest gives tests a fresh, migrated store of their own.
//
// Tests run on SQLite by default. Setting HOOKD_TEST_PG=1 also runs them on
// the PostgreSQL database described by the HOOKD_DB_* settings; each test
// then gets a new schema, which is dropped when it ends.
package storetest

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"

	"hookd/internal/config"
	"hookd/internal/store"
)

// PostgresEnv is the variable that turns on the PostgreSQL tests when set
// to 1.
const PostgresEnv = "HOOKD_TEST_PG"

// DB is an empty database of a test's own: a SQLite file or a PostgreSQL
// schema.
type DB struct {
	// Config describes the database. Tests may change settings that do not
	// pick the database, such as MaxConns, before calling Open.
	Config config.DB
	// schema is the PostgreSQL schema, or empty for SQLite.
	schema string
}

// SQLite returns a new SQLite database in t.TempDir().
func SQLite(t testing.TB) *DB {
	t.Helper()
	cfg := config.Default().DB
	cfg.Driver = config.DriverSQLite
	cfg.Path = filepath.Join(t.TempDir(), "hookd.db")
	return &DB{Config: cfg}
}

// Postgres creates a new schema in the PostgreSQL database described by the
// HOOKD_DB_* settings and drops it when the test ends. It skips the test
// unless HOOKD_TEST_PG=1.
func Postgres(t testing.TB) *DB {
	t.Helper()
	if os.Getenv(PostgresEnv) != "1" {
		t.Skipf("set %s=1 and the HOOKD_DB_* settings to run on PostgreSQL", PostgresEnv)
	}
	c, err := config.Load()
	if err != nil {
		t.Fatalf("load database settings: %v", err)
	}
	cfg := c.DB
	cfg.Driver = config.DriverPostgres

	b := make([]byte, 8)
	rand.Read(b)
	schema := "hookd_test_" + hex.EncodeToString(b)

	admin, err := sql.Open("pgx", store.PostgresDSN(cfg, ""))
	if err != nil {
		t.Fatalf("open PostgreSQL: %v", err)
	}
	ctx := context.Background()
	if _, err := admin.ExecContext(ctx, `CREATE SCHEMA `+schema); err != nil {
		admin.Close()
		t.Fatalf("create schema %s: %v", schema, err)
	}
	// Registered before any store is opened, so it runs after they are closed.
	t.Cleanup(func() {
		defer admin.Close()
		if _, err := admin.ExecContext(ctx, `DROP SCHEMA `+schema+` CASCADE`); err != nil {
			t.Errorf("drop schema %s: %v", schema, err)
		}
	})
	return &DB{Config: cfg, schema: schema}
}

// Run runs fn as a subtest on each dialect: "sqlite", and "postgres", which
// is skipped unless HOOKD_TEST_PG=1. Each gets a database of its own.
func Run(t *testing.T, fn func(t *testing.T, db *DB)) {
	t.Helper()
	t.Run(config.DriverSQLite, func(t *testing.T) { fn(t, SQLite(t)) })
	t.Run(config.DriverPostgres, func(t *testing.T) { fn(t, Postgres(t)) })
}

// Driver returns config.DriverSQLite or config.DriverPostgres.
func (db *DB) Driver() string { return db.Config.Driver }

// Open opens a store on the database without migrating it and closes it
// when the test ends.
func (db *DB) Open(t testing.TB, opts ...store.Option) *store.SQLStore {
	t.Helper()
	if db.schema != "" {
		opts = append([]store.Option{store.WithPostgresSchema(db.schema)}, opts...)
	}
	s, err := store.Open(context.Background(), db.Config, opts...)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

// New opens a store on the database and applies every migration.
func (db *DB) New(t testing.TB, opts ...store.Option) *store.SQLStore {
	t.Helper()
	s := db.Open(t, opts...)
	if _, err := s.Migrate(context.Background()); err != nil {
		t.Fatalf("migrate store: %v", err)
	}
	return s
}

// SQL opens a plain connection to the database, for checking what a store
// wrote, and closes it when the test ends. Queries must use the dialect's
// own placeholders.
func (db *DB) SQL(t testing.TB) *sql.DB {
	t.Helper()
	var (
		conn *sql.DB
		err  error
	)
	if db.schema != "" {
		conn, err = sql.Open("pgx", store.PostgresDSN(db.Config, db.schema))
	} else {
		conn, err = sql.Open("sqlite", db.Config.Path)
	}
	if err != nil {
		t.Fatalf("open database: %v", err)
	}
	t.Cleanup(func() { conn.Close() })
	return conn
}

// New opens a new SQLite database in t.TempDir(), applies every migration
// and closes the store when the test ends. No two tests share data.
func New(t testing.TB, opts ...store.Option) *store.SQLStore {
	t.Helper()
	return SQLite(t).New(t, opts...)
}
