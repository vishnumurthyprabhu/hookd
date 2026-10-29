package store_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"hookd/internal/config"
	"hookd/internal/ids"
	"hookd/internal/store"
	"hookd/internal/store/storetest"
)

func TestMigrateTwiceIsNoOp(t *testing.T) {
	storetest.Run(t, func(t *testing.T, db *storetest.DB) {
		ctx := context.Background()
		applied, err := db.Open(t).Migrate(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if len(applied) == 0 || applied[0] != "0001_init" {
			t.Fatalf("first Migrate applied %v, want it to start with 0001_init", applied)
		}

		// A new connection to the same database finds nothing left to do.
		applied, err = db.Open(t).Migrate(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if len(applied) != 0 {
			t.Fatalf("second Migrate applied %v, want nothing", applied)
		}
	})
}

func TestMigrateConcurrentlyOnEmptyDatabase(t *testing.T) {
	storetest.Run(t, func(t *testing.T, db *storetest.DB) {
		ctx := context.Background()
		const n = 4
		stores := make([]*store.SQLStore, n)
		for i := range stores {
			stores[i] = db.Open(t)
		}
		results := make(chan []string, n)
		errs := make(chan error, n)
		for _, s := range stores {
			go func() {
				applied, err := s.Migrate(ctx)
				results <- applied
				errs <- err
			}()
		}
		seen := map[string]bool{}
		for range n {
			if err := <-errs; err != nil {
				t.Errorf("Migrate: %v", err)
			}
			for _, v := range <-results {
				if seen[v] {
					t.Errorf("migration %s applied twice", v)
				}
				seen[v] = true
			}
		}
		if !seen["0001_init"] {
			t.Errorf("applied %v, want 0001_init among them", seen)
		}
		ms, err := stores[0].Migrate(ctx)
		if err != nil || len(ms) != 0 {
			t.Fatalf("Migrate afterwards = %v, %v, want nothing to do", ms, err)
		}
	})
}

func TestMigrateCreatesTablesAndRecordsVersions(t *testing.T) {
	storetest.Run(t, func(t *testing.T, db *storetest.DB) {
		db.New(t)

		// Inspect the schema with a plain connection.
		conn := db.SQL(t)
		query := `SELECT count(*) FROM sqlite_master WHERE name = ?`
		if db.Driver() == config.DriverPostgres {
			query = `SELECT count(*) FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
				WHERE c.relname = $1 AND n.nspname = current_schema()`
		}
		for _, name := range []string{
			"accounts", "api_keys", "endpoints", "events", "deliveries", "schema_migrations",
			"deliveries_status_locked_until", "events_account_idempotency_key",
		} {
			var n int
			if err := conn.QueryRow(query, name).Scan(&n); err != nil {
				t.Fatal(err)
			}
			if n != 1 {
				t.Errorf("schema object %q missing", name)
			}
		}
		var versions int
		if err := conn.QueryRow(`SELECT count(*) FROM schema_migrations WHERE version = '0001_init'`).Scan(&versions); err != nil {
			t.Fatal(err)
		}
		if versions != 1 {
			t.Errorf("schema_migrations has %d rows for 0001_init, want 1", versions)
		}
	})
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
	storetest.Run(t, func(t *testing.T, db *storetest.DB) {
		ctx := context.Background()
		now := time.Date(2026, 10, 29, 12, 0, 0, 123456789, time.UTC)
		s := db.New(t, store.WithClock(func() time.Time { return now }))

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
	})
}

func TestAPIKeysBelongToTheirAccount(t *testing.T) {
	storetest.Run(t, func(t *testing.T, db *storetest.DB) {
		ctx := context.Background()
		s := db.New(t)

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
	})
}

func TestCreateAPIKeyUnknownAccount(t *testing.T) {
	storetest.Run(t, func(t *testing.T, db *storetest.DB) {
		s := db.New(t)
		if _, _, err := s.CreateAPIKey(context.Background(), "acct_missing"); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("err = %v, want ErrNotFound", err)
		}
	})
}

func TestListAPIKeys(t *testing.T) {
	storetest.Run(t, func(t *testing.T, db *storetest.DB) {
		ctx := context.Background()
		now := time.Date(2026, 10, 29, 12, 0, 0, 123456789, time.UTC)
		s := db.New(t, store.WithClock(func() time.Time { return now }))

		a, _ := s.CreateAccount(ctx, "a")
		b, _ := s.CreateAccount(ctx, "b")
		empty, _ := s.CreateAccount(ctx, "empty")
		var want []store.APIKey
		for range 3 {
			rec, _, err := s.CreateAPIKey(ctx, a.ID)
			if err != nil {
				t.Fatal(err)
			}
			rec.CreatedAt = rec.CreatedAt.Truncate(time.Microsecond)
			want = append(want, rec)
			if _, _, err := s.CreateAPIKey(ctx, b.ID); err != nil {
				t.Fatal(err)
			}
		}

		got, err := s.ListAPIKeys(ctx, a.ID)
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != len(want) {
			t.Fatalf("ListAPIKeys = %+v, want %+v", got, want)
		}
		for i := range want {
			if got[i] != want[i] {
				t.Errorf("key %d = %+v, want %+v", i, got[i], want[i])
			}
		}

		got, err = s.ListAPIKeys(ctx, empty.ID)
		if err != nil || got == nil || len(got) != 0 {
			t.Fatalf("account without keys: ListAPIKeys = %#v, %v; want empty slice", got, err)
		}
		if _, err := s.ListAPIKeys(ctx, "acct_missing"); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("unknown account: err = %v, want ErrNotFound", err)
		}
	})
}

func TestDeleteAPIKey(t *testing.T) {
	storetest.Run(t, func(t *testing.T, db *storetest.DB) {
		ctx := context.Background()
		now := time.Date(2026, 10, 29, 12, 0, 0, 123456789, time.UTC)
		s := db.New(t, store.WithClock(func() time.Time { return now }))

		acct, _ := s.CreateAccount(ctx, "acme")
		old, oldKey, err := s.CreateAPIKey(ctx, acct.ID)
		if err != nil {
			t.Fatal(err)
		}
		kept, keptKey, err := s.CreateAPIKey(ctx, acct.ID)
		if err != nil {
			t.Fatal(err)
		}

		got, err := s.DeleteAPIKey(ctx, old.ID)
		if err != nil {
			t.Fatal(err)
		}
		old.CreatedAt = old.CreatedAt.Truncate(time.Microsecond)
		if got != old {
			t.Fatalf("DeleteAPIKey = %+v, want %+v", got, old)
		}
		if _, err := s.AccountByAPIKeyHash(ctx, ids.HashAPIKey(oldKey)); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("deleted key still authenticates: err = %v", err)
		}
		if a, err := s.AccountByAPIKeyHash(ctx, ids.HashAPIKey(keptKey)); err != nil || a.ID != acct.ID {
			t.Fatalf("other key after delete: %+v, %v", a, err)
		}
		keys, err := s.ListAPIKeys(ctx, acct.ID)
		if err != nil {
			t.Fatal(err)
		}
		if len(keys) != 1 || keys[0].ID != kept.ID {
			t.Fatalf("keys after delete = %+v, want only %s", keys, kept.ID)
		}

		if _, err := s.DeleteAPIKey(ctx, old.ID); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("second delete: err = %v, want ErrNotFound", err)
		}
		if _, err := s.DeleteAPIKey(ctx, "key_missing"); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("unknown key: err = %v, want ErrNotFound", err)
		}
	})
}

func TestStoreOnlyKeepsKeyHash(t *testing.T) {
	storetest.Run(t, func(t *testing.T, db *storetest.DB) {
		ctx := context.Background()
		s := db.New(t)
		acct, _ := s.CreateAccount(ctx, "acme")
		_, key, err := s.CreateAPIKey(ctx, acct.ID)
		if err != nil {
			t.Fatal(err)
		}

		var hash, prefix string
		if err := db.SQL(t).QueryRow(`SELECT key_hash, key_prefix FROM api_keys`).Scan(&hash, &prefix); err != nil {
			t.Fatal(err)
		}
		if hash != ids.HashAPIKey(key) || hash == key || len(prefix) >= len(key) {
			t.Fatalf("stored hash %q prefix %q for key %q", hash, prefix, key)
		}
	})
}

func TestPostgresDSN(t *testing.T) {
	cfg := config.Default().DB
	cfg.Password = "p@ss word"
	for _, schema := range []string{"", "hookd_test_1"} {
		pc, err := pgx.ParseConfig(store.PostgresDSN(cfg, schema))
		if err != nil {
			t.Fatal(err)
		}
		if pc.Host != cfg.Host || int(pc.Port) != cfg.Port || pc.Database != cfg.Name ||
			pc.User != cfg.User || pc.Password != cfg.Password {
			t.Errorf("schema %q: parsed %s@%s:%d/%s", schema, pc.User, pc.Host, pc.Port, pc.Database)
		}
		got, ok := pc.RuntimeParams["search_path"]
		if schema == "" && ok {
			t.Errorf("search_path = %q, want it unset", got)
		}
		if schema != "" && got != schema {
			t.Errorf("search_path = %q, want %q", got, schema)
		}
	}
}
