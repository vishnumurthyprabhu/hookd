package store

import (
	"context"
	"database/sql"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"path"
	"sort"
	"strings"
)

//go:embed migrations
var migrationFiles embed.FS

// migration is one embedded SQL file. Its version is the file name without
// ".sql", such as "0001_init"; versions are applied in sorted order.
type migration struct {
	version string
	sql     string
}

func loadMigrations(d dialect) ([]migration, error) {
	dir := path.Join("migrations", d.migrationsDir)
	entries, err := fs.ReadDir(migrationFiles, dir)
	if err != nil {
		return nil, fmt.Errorf("store: read %s migrations: %w", d.name, err)
	}
	var ms []migration
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".sql") {
			continue
		}
		b, err := fs.ReadFile(migrationFiles, path.Join(dir, e.Name()))
		if err != nil {
			return nil, fmt.Errorf("store: read migration %s: %w", e.Name(), err)
		}
		ms = append(ms, migration{version: strings.TrimSuffix(e.Name(), ".sql"), sql: string(b)})
	}
	sort.Slice(ms, func(i, j int) bool { return ms[i].version < ms[j].version })
	return ms, nil
}

// Migrate implements Store. Each migration runs in its own transaction
// together with the row that records it, so a failed migration leaves no
// trace and is tried again next time.
func (s *SQLStore) Migrate(ctx context.Context) ([]string, error) {
	ms, err := loadMigrations(s.d)
	if err != nil {
		return nil, err
	}
	if _, err := s.db.ExecContext(ctx, s.d.createMigrationsTable); err != nil {
		return nil, fmt.Errorf("store: create schema_migrations: %w", err)
	}

	var applied []string
	for _, m := range ms {
		ran := false
		err := s.inTx(ctx, func(tx *sql.Tx) error {
			if s.d.lockMigrations != "" {
				if _, err := tx.ExecContext(ctx, s.d.lockMigrations); err != nil {
					return fmt.Errorf("lock: %w", err)
				}
			}
			var v string
			err := s.queryRow(ctx, tx, `SELECT version FROM schema_migrations WHERE version = ?`, m.version).Scan(&v)
			if err == nil {
				return nil // already applied
			}
			if !errors.Is(err, sql.ErrNoRows) {
				return err
			}
			if _, err := tx.ExecContext(ctx, m.sql); err != nil {
				return err
			}
			if _, err := s.exec(ctx, tx, `INSERT INTO schema_migrations (version, applied_at) VALUES (?, ?)`,
				m.version, s.d.timeArg(s.nowUTC())); err != nil {
				return err
			}
			ran = true
			return nil
		})
		if err != nil {
			return applied, fmt.Errorf("store: migration %s: %w", m.version, err)
		}
		if ran {
			applied = append(applied, m.version)
		}
	}
	return applied, nil
}
