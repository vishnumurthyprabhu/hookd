package store_test

import (
	"context"
	"database/sql"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"hookd/internal/config"
	"hookd/internal/ids"
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
		mustPending(t, s, []string{"0001_init", "0002_admin", "0003_retries"})

		applied, err := s.Migrate(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if want := []string{"0001_init", "0002_admin", "0003_retries"}; !reflect.DeepEqual(applied, want) {
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
		mustPending(t, s, []string{"0002_admin", "0003_retries"})

		// Data written at 0001 survives the migration.
		acct := mustAccount(t, s)
		ep := mustEndpoint(t, s, acct.ID, store.NewEndpoint{URL: "https://example.com/hook", EventTypes: []string{}})
		ev := insertOldDelivery(t, db, acct.ID, ep.ID, store.DeliveryPending, nil)

		applied, err = db.Open(t).Migrate(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if want := []string{"0002_admin", "0003_retries"}; !reflect.DeepEqual(applied, want) {
			t.Fatalf("Migrate applied %v, want %v", applied, want)
		}
		mustPending(t, s, []string{})

		got, err := s.GetEvent(ctx, acct.ID, ev.EventID)
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

func TestMigrateFrom0002(t *testing.T) {
	storetest.Run(t, func(t *testing.T, db *storetest.DB) {
		ctx := context.Background()
		s := db.Open(t)
		if _, err := s.MigrateThrough(ctx, "0002_admin"); err != nil {
			t.Fatal(err)
		}
		mustPending(t, s, []string{"0003_retries"})

		// Four deliveries written at 0002: one succeeded, one failed, one in
		// progress and one pending.
		acct := mustAccount(t, s)
		ep := mustEndpoint(t, s, acct.ID, store.NewEndpoint{URL: "https://example.com/hook", EventTypes: []string{}})
		succeeded := insertOldDelivery(t, db, acct.ID, ep.ID, store.DeliverySucceeded, &oldResult{204, "", 12})
		failed := insertOldDelivery(t, db, acct.ID, ep.ID, store.DeliveryFailed, &oldResult{500, "http_500", 34})
		inProgress := insertOldDelivery(t, db, acct.ID, ep.ID, store.DeliveryInProgress, nil)
		insertOldDelivery(t, db, acct.ID, ep.ID, store.DeliveryPending, nil)

		applied, err := db.Open(t).Migrate(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if want := []string{"0003_retries"}; !reflect.DeepEqual(applied, want) {
			t.Fatalf("Migrate applied %v, want %v", applied, want)
		}
		mustPending(t, s, []string{})

		conn := db.SQL(t)
		q := func(query string) string { return placeholders(db, query) }
		count := func(query string, args ...any) int {
			t.Helper()
			var n int
			if err := conn.QueryRow(q(query), args...).Scan(&n); err != nil {
				t.Fatalf("%s: %v", query, err)
			}
			return n
		}

		if n := count(`SELECT count(*) FROM deliveries WHERE next_attempt_at = created_at`); n != 4 {
			t.Errorf("%d deliveries have next_attempt_at = created_at, want 4", n)
		}
		for id, want := range map[string]int{succeeded.ID: 1, failed.ID: 1, inProgress.ID: 0} {
			if n := count(`SELECT attempts FROM deliveries WHERE id = ?`, id); n != want {
				t.Errorf("delivery %s has attempts = %d, want %d", id, n, want)
			}
		}
		if n := count(`SELECT count(*) FROM deliveries WHERE status = 'pending' AND attempts = 0`); n != 1 {
			t.Errorf("%d pending deliveries with no attempts, want 1", n)
		}
		if n := count(`SELECT count(*) FROM attempts`); n != 2 {
			t.Errorf("attempts has %d rows, want 2", n)
		}
		if n := count(`SELECT count(*) FROM attempts a JOIN deliveries d ON d.id = a.delivery_id
			WHERE a.started_at = d.attempted_at`); n != 2 {
			t.Errorf("%d attempts start at their delivery's attempted_at, want 2", n)
		}

		type row struct {
			id, account, event, endpoint, outcome string
			number, latency                       int
			respStatus                            sql.NullInt64
			errText                               sql.NullString
		}
		for _, c := range []struct {
			d    oldDelivery
			want row
		}{
			{succeeded, row{outcome: store.DeliverySucceeded, number: 1, latency: 12,
				respStatus: sql.NullInt64{Int64: 204, Valid: true}}},
			{failed, row{outcome: store.DeliveryFailed, number: 1, latency: 34,
				respStatus: sql.NullInt64{Int64: 500, Valid: true}, errText: sql.NullString{String: "http_500", Valid: true}}},
		} {
			var got row
			err := conn.QueryRow(q(`SELECT id, account_id, event_id, endpoint_id, outcome, number, latency_ms, response_status, error
				FROM attempts WHERE delivery_id = ?`), c.d.ID).Scan(
				&got.id, &got.account, &got.event, &got.endpoint, &got.outcome, &got.number, &got.latency, &got.respStatus, &got.errText)
			if err != nil {
				t.Fatalf("attempt of %s: %v", c.d.ID, err)
			}
			want := c.want
			want.id = "att_" + strings.TrimPrefix(c.d.ID, "dlv_")
			want.account, want.event, want.endpoint = acct.ID, c.d.EventID, ep.ID
			if got != want {
				t.Errorf("attempt of %s = %+v, want %+v", c.d.ID, got, want)
			}
		}

		for _, name := range []string{
			"attempts_event_id", "attempts_endpoint_id", "attempts_endpoint_id_outcome",
			"deliveries_status_next_attempt_at", "deliveries_endpoint_id_status", "events_created_at",
		} {
			query := `SELECT count(*) FROM sqlite_master WHERE name = ?`
			if db.Driver() == config.DriverPostgres {
				query = `SELECT count(*) FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
					WHERE c.relname = ? AND n.nspname = current_schema()`
			}
			if count(query, name) != 1 {
				t.Errorf("schema object %q missing", name)
			}
		}

		// The same attempt number is not recorded twice for a delivery.
		if _, err := conn.Exec(q(`INSERT INTO attempts (id, account_id, delivery_id, event_id, endpoint_id, number,
			started_at, outcome, latency_ms) SELECT 'att_dup', account_id, delivery_id, event_id, endpoint_id, number,
			started_at, outcome, latency_ms FROM attempts WHERE delivery_id = ?`), succeeded.ID); err == nil {
			t.Error("a second attempt with the same number was stored")
		}
	})
}

// placeholders rewrites the ? placeholders of query as $1, $2, … on
// PostgreSQL, for tests that use a plain connection.
func placeholders(db *storetest.DB, query string) string {
	if db.Driver() == config.DriverPostgres {
		for i := 1; strings.Contains(query, "?"); i++ {
			query = strings.Replace(query, "?", "$"+strconv.Itoa(i), 1)
		}
	}
	return query
}

// oldDelivery is a delivery written by insertOldDelivery.
type oldDelivery struct{ ID, EventID string }

// oldResult is the recorded outcome of a finished oldDelivery.
type oldResult struct {
	responseStatus int
	errText        string
	latencyMS      int
}

// insertOldDelivery writes an event with one delivery in the given status
// using only the columns of migration 0001, as the store did before
// 0003_retries. The Store methods cannot do it once they use newer columns.
// r is the outcome of a succeeded or failed delivery, and nil otherwise.
func insertOldDelivery(t *testing.T, db *storetest.DB, accountID, endpointID, status string, r *oldResult) oldDelivery {
	t.Helper()
	conn := db.SQL(t)
	now := time.Now().UTC().Format("2006-01-02T15:04:05.000000Z")
	d := oldDelivery{ID: ids.NewDeliveryID(), EventID: ids.NewMessageID()}
	if _, err := conn.Exec(placeholders(db,
		`INSERT INTO events (id, account_id, event_type, payload, created_at) VALUES (?, ?, 'order.paid', '{}', ?)`),
		d.EventID, accountID, now); err != nil {
		t.Fatalf("insert event: %v", err)
	}
	var respStatus, errText, latency, attempted, locked any
	switch {
	case r != nil:
		respStatus, latency, attempted = r.responseStatus, r.latencyMS, now
		if r.errText != "" {
			errText = r.errText
		}
	case status == store.DeliveryInProgress:
		locked = time.Now().UTC().Add(time.Hour).Format("2006-01-02T15:04:05.000000Z")
	}
	if _, err := conn.Exec(placeholders(db,
		`INSERT INTO deliveries (id, event_id, endpoint_id, status, response_status, error, latency_ms, attempted_at, locked_until, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`),
		d.ID, d.EventID, endpointID, status, respStatus, errText, latency, attempted, locked, now); err != nil {
		t.Fatalf("insert delivery: %v", err)
	}
	return d
}
