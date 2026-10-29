package store_test

import (
	"context"
	"reflect"
	"testing"

	"hookd/internal/config"
	"hookd/internal/store"
	"hookd/internal/store/storetest"
)

func mustPending(t *testing.T, s store.Store, want []string) {
	t.Helper()
	got, err := s.PendingMigrations(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("PendingMigrations = %#v, want %#v", got, want)
	}
}

func TestPendingMigrationsOnFreshDatabase(t *testing.T) {
	storetest.Run(t, func(t *testing.T, db *storetest.DB) {
		s := db.Open(t)
		mustPending(t, s, []string{"0001_init", "0002_admin"})

		applied, err := s.Migrate(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if want := []string{"0001_init", "0002_admin"}; !reflect.DeepEqual(applied, want) {
			t.Fatalf("Migrate applied %v, want %v", applied, want)
		}
		mustPending(t, s, []string{})
	})
}

func TestMigrateFrom0001(t *testing.T) {
	storetest.Run(t, func(t *testing.T, db *storetest.DB) {
		ctx := context.Background()
		s := db.Open(t)
		applied, err := s.MigrateThrough(ctx, "0001_init")
		if err != nil {
			t.Fatal(err)
		}
		if want := []string{"0001_init"}; !reflect.DeepEqual(applied, want) {
			t.Fatalf("MigrateThrough applied %v, want %v", applied, want)
		}
		mustPending(t, s, []string{"0002_admin"})

		// Data written at 0001 survives the migration.
		acct := mustAccount(t, s)
		ep := mustEndpoint(t, s, acct.ID, store.NewEndpoint{URL: "https://example.com/hook", EventTypes: []string{}})
		ev, _ := mustEvent(t, s, acct.ID, store.NewEvent{EventType: "order.paid", Payload: []byte(`{}`)})

		applied, err = db.Open(t).Migrate(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if want := []string{"0002_admin"}; !reflect.DeepEqual(applied, want) {
			t.Fatalf("Migrate applied %v, want %v", applied, want)
		}
		mustPending(t, s, []string{})

		got, err := s.GetEvent(ctx, acct.ID, ev.ID)
		if err != nil {
			t.Fatal(err)
		}
		if len(got.Deliveries) != 1 || got.Deliveries[0].EndpointID != ep.ID {
			t.Fatalf("event after migration = %+v", got)
		}
		if err := s.SetSetting(ctx, "HOOKD_WORKERS", "8"); err != nil {
			t.Fatalf("settings table after migration: %v", err)
		}
	})
}

func TestMigrateThroughUnknownVersion(t *testing.T) {
	s := storetest.SQLite(t).Open(t)
	if _, err := s.MigrateThrough(context.Background(), "0999_missing"); err == nil {
		t.Fatal("MigrateThrough an unknown version succeeded")
	}
}

func TestMigrate0002CreatesTables(t *testing.T) {
	storetest.Run(t, func(t *testing.T, db *storetest.DB) {
		db.New(t)

		conn := db.SQL(t)
		query := `SELECT count(*) FROM sqlite_master WHERE name = ?`
		if db.Driver() == config.DriverPostgres {
			query = `SELECT count(*) FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
				WHERE c.relname = $1 AND n.nspname = current_schema()`
		}
		for _, name := range []string{
			"settings", "test_receivers", "test_requests",
			"test_receivers_account_id", "test_requests_receiver_id", "events_account_event_type",
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
		if err := conn.QueryRow(`SELECT count(*) FROM schema_migrations WHERE version = '0002_admin'`).Scan(&versions); err != nil {
			t.Fatal(err)
		}
		if versions != 1 {
			t.Errorf("schema_migrations has %d rows for 0002_admin, want 1", versions)
		}
	})
}
