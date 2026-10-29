// Package storetest gives tests a fresh, migrated store of their own.
package storetest

import (
	"context"
	"path/filepath"
	"testing"

	"hookd/internal/config"
	"hookd/internal/store"
)

// New opens a new SQLite database in t.TempDir(), applies every migration
// and closes the store when the test ends. No two tests share data.
func New(t testing.TB, opts ...store.Option) *store.SQLStore {
	t.Helper()
	cfg := config.Default().DB
	cfg.Driver = config.DriverSQLite
	cfg.Path = filepath.Join(t.TempDir(), "hookd.db")

	ctx := context.Background()
	s, err := store.Open(ctx, cfg, opts...)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	if _, err := s.Migrate(ctx); err != nil {
		t.Fatalf("migrate store: %v", err)
	}
	return s
}
