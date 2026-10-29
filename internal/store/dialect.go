package store

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// dialect holds what differs between SQLite and PostgreSQL. Queries are
// written once with "?" placeholders and passed through rebind.
type dialect struct {
	name string
	// driver is the database/sql driver name.
	driver string
	// migrationsDir is the directory under migrations/ holding this dialect's
	// migration files.
	migrationsDir string
	// createMigrationsTable creates the table recording applied migrations.
	createMigrationsTable string
	// lockMigrations, if set, is run at the start of each migration's
	// transaction so that several instances do not migrate at once.
	lockMigrations string
	// claimDelivery atomically takes one delivery that is pending, or in
	// progress with an expired lease, and marks it in progress. Its arguments
	// are the new locked_until and the current time.
	claimDelivery string
	// numberedPlaceholders is true when placeholders are $1, $2, … rather
	// than ?.
	numberedPlaceholders bool
	// timesAsText is true when timestamps are stored as fixed-width UTC text,
	// which sorts and compares in time order.
	timesAsText bool
}

// sqliteTimeLayout is fixed-width, so stored times compare correctly as text.
const sqliteTimeLayout = "2006-01-02T15:04:05.000000Z"

var sqliteDialect = dialect{
	name:          "sqlite",
	driver:        "sqlite",
	migrationsDir: "sqlite",
	createMigrationsTable: `CREATE TABLE IF NOT EXISTS schema_migrations (
		version    TEXT PRIMARY KEY,
		applied_at TEXT NOT NULL
	)`,
	// SQLite allows one writer at a time, so a single UPDATE … RETURNING is
	// atomic.
	claimDelivery: `UPDATE deliveries SET status = 'in_progress', locked_until = ?
		WHERE id = (
			SELECT id FROM deliveries
			WHERE status = 'pending' OR (status = 'in_progress' AND locked_until < ?)
			ORDER BY created_at, id
			LIMIT 1
		)
		RETURNING id, event_id, endpoint_id`,
	timesAsText: true,
}

var postgresDialect = dialect{
	name:          "postgres",
	driver:        "pgx",
	migrationsDir: "postgres",
	createMigrationsTable: `CREATE TABLE IF NOT EXISTS schema_migrations (
		version    TEXT PRIMARY KEY,
		applied_at TIMESTAMPTZ NOT NULL
	)`,
	lockMigrations: `SELECT pg_advisory_xact_lock(7238465100)`,
	// SKIP LOCKED lets several instances share the queue without claiming
	// the same row.
	claimDelivery: `UPDATE deliveries SET status = 'in_progress', locked_until = ?
		WHERE id = (
			SELECT id FROM deliveries
			WHERE status = 'pending' OR (status = 'in_progress' AND locked_until < ?)
			ORDER BY created_at, id
			LIMIT 1
			FOR UPDATE SKIP LOCKED
		)
		RETURNING id, event_id, endpoint_id`,
	numberedPlaceholders: true,
}

// rebind rewrites the "?" placeholders in query for this dialect. Queries
// must not contain "?" anywhere else.
func (d dialect) rebind(query string) string {
	if !d.numberedPlaceholders {
		return query
	}
	var b strings.Builder
	n := 0
	for _, r := range query {
		if r == '?' {
			n++
			b.WriteByte('$')
			b.WriteString(strconv.Itoa(n))
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}

// timeArg converts t into a query argument for this dialect. Times are kept
// in UTC with microsecond precision, which both databases can hold exactly.
func (d dialect) timeArg(t time.Time) any {
	t = t.UTC().Truncate(time.Microsecond)
	if d.timesAsText {
		return t.Format(sqliteTimeLayout)
	}
	return t
}

// nullTimeArg is timeArg for a nullable column; a nil t is stored as NULL.
func (d dialect) nullTimeArg(t *time.Time) any {
	if t == nil {
		return nil
	}
	return d.timeArg(*t)
}

// dbTime scans a timestamp stored by either dialect. Valid is false for NULL.
type dbTime struct {
	Time  time.Time
	Valid bool
}

// Scan implements sql.Scanner.
func (t *dbTime) Scan(src any) error {
	switch v := src.(type) {
	case nil:
		*t = dbTime{}
		return nil
	case time.Time:
		*t = dbTime{Time: v.UTC(), Valid: true}
		return nil
	case string:
		return t.parse(v)
	case []byte:
		return t.parse(string(v))
	default:
		return fmt.Errorf("store: cannot scan %T into a time", src)
	}
}

func (t *dbTime) parse(s string) error {
	v, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		return fmt.Errorf("store: parse time %q: %w", s, err)
	}
	*t = dbTime{Time: v.UTC(), Valid: true}
	return nil
}
