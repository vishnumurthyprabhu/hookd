package store_test

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	_ "modernc.org/sqlite"

	"hookd/internal/config"
	"hookd/internal/ids"
	"hookd/internal/store"
	"hookd/internal/store/storetest"
)

func sqliteConfig(t *testing.T) config.DB {
	cfg := config.Default().DB
	cfg.Path = filepath.Join(t.TempDir(), "hookd.db")
	return cfg
}

func TestMigrateTwiceIsNoOp(t *testing.T) {
	ctx := context.Background()
	cfg := sqliteConfig(t)

	s, err := store.Open(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	applied, err := s.Migrate(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(applied) == 0 || applied[0] != "0001_init" {
		t.Fatalf("first Migrate applied %v, want it to start with 0001_init", applied)
	}
	s.Close()

	// A new connection to the same file finds nothing left to do.
	s, err = store.Open(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	applied, err = s.Migrate(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(applied) != 0 {
		t.Fatalf("second Migrate applied %v, want nothing", applied)
	}
}

func TestMigrateCreatesTablesAndRecordsVersions(t *testing.T) {
	ctx := context.Background()
	cfg := sqliteConfig(t)
	st, err := store.Open(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	st.Close()

	// Inspect the schema with a plain connection.
	db, err := sql.Open("sqlite", cfg.Path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	for _, name := range []string{
		"accounts", "api_keys", "endpoints", "events", "deliveries", "schema_migrations",
		"deliveries_status_locked_until", "events_account_idempotency_key",
	} {
		var n int
		if err := db.QueryRow(`SELECT count(*) FROM sqlite_master WHERE name = ?`, name).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n != 1 {
			t.Errorf("schema object %q missing", name)
		}
	}
	var versions int
	if err := db.QueryRow(`SELECT count(*) FROM schema_migrations WHERE version = '0001_init'`).Scan(&versions); err != nil {
		t.Fatal(err)
	}
	if versions != 1 {
		t.Errorf("schema_migrations has %d rows for 0001_init, want 1", versions)
	}
}

func TestOpenUnknownDriver(t *testing.T) {
	cfg := config.Default().DB
	cfg.Driver = "oracle"
	if _, err := store.Open(context.Background(), cfg); err == nil || !strings.Contains(err.Error(), "oracle") {
		t.Fatalf("Open with unknown driver: err = %v", err)
	}
}

func TestOpenMemory(t *testing.T) {
	ctx := context.Background()
	cfg := config.Default().DB
	cfg.Path = ":memory:"
	s, err := store.Open(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if _, err := s.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateAccount(ctx, "acme"); err != nil {
		t.Fatal(err)
	}
}

func TestAccountAndAPIKey(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 10, 29, 12, 0, 0, 123456789, time.UTC)
	s := storetest.New(t, store.WithClock(func() time.Time { return now }))

	acct, err := s.CreateAccount(ctx, "acme")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(acct.ID, ids.PrefixAccount) || acct.Name != "acme" {
		t.Fatalf("account = %+v", acct)
	}

	rec, key, err := s.CreateAPIKey(ctx, acct.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(key, ids.APIKeyPrefix) {
		t.Fatalf("key %q lacks prefix %q", key, ids.APIKeyPrefix)
	}
	if !strings.HasPrefix(rec.ID, ids.PrefixAPIKey) || rec.AccountID != acct.ID || rec.Prefix != ids.APIKeyDisplayPrefix(key) {
		t.Fatalf("key record = %+v", rec)
	}

	got, err := s.AccountByAPIKeyHash(ctx, ids.HashAPIKey(key))
	if err != nil {
		t.Fatal(err)
	}
	want := store.Account{ID: acct.ID, Name: "acme", CreatedAt: now.Truncate(time.Microsecond)}
	if got != want {
		t.Fatalf("AccountByAPIKeyHash = %+v, want %+v", got, want)
	}

	if _, err := s.AccountByAPIKeyHash(ctx, ids.HashAPIKey(key+"x")); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("unknown hash: err = %v, want ErrNotFound", err)
	}
	if _, err := s.AccountByAPIKeyHash(ctx, key); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("plain key used as hash: err = %v, want ErrNotFound", err)
	}
}

func TestAPIKeysBelongToTheirAccount(t *testing.T) {
	ctx := context.Background()
	s := storetest.New(t)

	a, _ := s.CreateAccount(ctx, "a")
	b, _ := s.CreateAccount(ctx, "b")
	_, keyA, err := s.CreateAPIKey(ctx, a.ID)
	if err != nil {
		t.Fatal(err)
	}
	_, keyB, err := s.CreateAPIKey(ctx, b.ID)
	if err != nil {
		t.Fatal(err)
	}
	_, keyA2, err := s.CreateAPIKey(ctx, a.ID)
	if err != nil {
		t.Fatal(err)
	}

	for key, want := range map[string]string{keyA: a.ID, keyB: b.ID, keyA2: a.ID} {
		got, err := s.AccountByAPIKeyHash(ctx, ids.HashAPIKey(key))
		if err != nil {
			t.Fatal(err)
		}
		if got.ID != want {
			t.Errorf("key %s… belongs to %s, want %s", ids.APIKeyDisplayPrefix(key), got.ID, want)
		}
	}
}

func TestCreateAPIKeyUnknownAccount(t *testing.T) {
	s := storetest.New(t)
	if _, _, err := s.CreateAPIKey(context.Background(), "acct_missing"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}

func TestStoreOnlyKeepsKeyHash(t *testing.T) {
	ctx := context.Background()
	cfg := sqliteConfig(t)
	s, err := store.Open(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	acct, _ := s.CreateAccount(ctx, "acme")
	_, key, err := s.CreateAPIKey(ctx, acct.ID)
	if err != nil {
		t.Fatal(err)
	}
	s.Close()

	db, err := sql.Open("sqlite", cfg.Path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var hash, prefix string
	if err := db.QueryRow(`SELECT key_hash, key_prefix FROM api_keys`).Scan(&hash, &prefix); err != nil {
		t.Fatal(err)
	}
	if hash != ids.HashAPIKey(key) || hash == key || len(prefix) >= len(key) {
		t.Fatalf("stored hash %q prefix %q for key %q", hash, prefix, key)
	}
}
