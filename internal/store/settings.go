package store

import (
	"context"
	"fmt"
)

// ListSettings implements Store.
func (s *SQLStore) ListSettings(ctx context.Context) ([]Setting, error) {
	rows, err := s.query(ctx, s.db, `SELECT name, value, updated_at FROM settings ORDER BY name`)
	if err != nil {
		return nil, fmt.Errorf("store: list settings: %w", err)
	}
	defer rows.Close()
	settings := []Setting{}
	for rows.Next() {
		var (
			st      Setting
			updated dbTime
		)
		if err := rows.Scan(&st.Name, &st.Value, &updated); err != nil {
			return nil, fmt.Errorf("store: list settings: %w", err)
		}
		st.UpdatedAt = updated.Time
		settings = append(settings, st)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: list settings: %w", err)
	}
	return settings, nil
}

// SetSetting implements Store. Both dialects support this upsert.
func (s *SQLStore) SetSetting(ctx context.Context, name, value string) error {
	if _, err := s.exec(ctx, s.db,
		`INSERT INTO settings (name, value, updated_at) VALUES (?, ?, ?)
		ON CONFLICT (name) DO UPDATE SET value = excluded.value, updated_at = excluded.updated_at`,
		name, value, s.d.timeArg(s.nowUTC())); err != nil {
		return fmt.Errorf("store: set setting %s: %w", name, err)
	}
	return nil
}

// DeleteSetting implements Store.
func (s *SQLStore) DeleteSetting(ctx context.Context, name string) error {
	if _, err := s.exec(ctx, s.db, `DELETE FROM settings WHERE name = ?`, name); err != nil {
		return fmt.Errorf("store: delete setting %s: %w", name, err)
	}
	return nil
}
