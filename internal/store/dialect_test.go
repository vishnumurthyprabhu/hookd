package store

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"hookd/internal/config"
)

func TestRebind(t *testing.T) {
	q := `SELECT a FROM t WHERE b = ? AND c IN (?, ?)`
	if got := sqliteDialect.rebind(q); got != q {
		t.Errorf("sqlite rebind = %q, want it unchanged", got)
	}
	want := `SELECT a FROM t WHERE b = $1 AND c IN ($2, $3)`
	if got := postgresDialect.rebind(q); got != want {
		t.Errorf("postgres rebind = %q, want %q", got, want)
	}
}

func TestTimeRoundTrip(t *testing.T) {
	in := time.Date(2026, 1, 2, 3, 4, 5, 6789, time.FixedZone("x", 3600))
	want := in.UTC().Truncate(time.Microsecond)

	for _, d := range []dialect{sqliteDialect, postgresDialect} {
		var got dbTime
		if err := got.Scan(d.timeArg(in)); err != nil {
			t.Fatalf("%s: %v", d.name, err)
		}
		if !got.Valid || !got.Time.Equal(want) || got.Time.Location() != time.UTC {
			t.Errorf("%s: round trip = %v, want %v", d.name, got.Time, want)
		}
	}

	var null dbTime
	if err := null.Scan(sqliteDialect.nullTimeArg(nil)); err != nil || null.Valid {
		t.Errorf("NULL: got %+v, %v", null, err)
	}
}

func TestSQLiteTimesSortAsText(t *testing.T) {
	early := sqliteDialect.timeArg(time.Date(2026, 1, 1, 0, 0, 9, 0, time.UTC)).(string)
	late := sqliteDialect.timeArg(time.Date(2026, 1, 1, 0, 0, 10, 500, time.UTC)).(string)
	if len(early) != len(late) || early >= late {
		t.Errorf("%q should sort before %q and have the same length", early, late)
	}
}

func TestSQLiteClaimQueryMatchesSchema(t *testing.T) {
	ctx := context.Background()
	cfg := config.Default().DB
	cfg.Path = filepath.Join(t.TempDir(), "hookd.db")
	s, err := Open(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if _, err := s.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	var (
		id, eventID, endpointID string
		attempts                int
	)
	err = s.queryRow(ctx, s.db, s.d.claimDelivery, s.d.timeArg(now.Add(time.Minute)), s.d.timeArg(now), s.d.timeArg(now)).
		Scan(&id, &eventID, &endpointID, &attempts)
	if !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("claim on an empty queue: err = %v, want sql.ErrNoRows", err)
	}
}
