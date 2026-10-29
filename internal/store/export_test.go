package store

import "context"

// MigrateThrough applies the migrations up to and including version last,
// so tests can build a database at an older version.
func (s *SQLStore) MigrateThrough(ctx context.Context, last string) ([]string, error) {
	return s.migrateThrough(ctx, last)
}
