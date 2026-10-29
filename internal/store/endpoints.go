package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"

	"hookd/internal/ids"
)

// endpointColumns are the columns read by scanEndpoint, in order. The secret
// is not among them.
const endpointColumns = `id, account_id, url, description, event_types, enabled, created_at, updated_at`

// CreateEndpoint implements Store.
func (s *SQLStore) CreateEndpoint(ctx context.Context, accountID string, in NewEndpoint) (Endpoint, string, error) {
	var (
		e      Endpoint
		secret string
	)
	err := s.inTx(ctx, func(tx *sql.Tx) error {
		var err error
		e, secret, err = s.insertEndpoint(ctx, tx, accountID, in)
		return err
	})
	if err != nil {
		return Endpoint{}, "", err
	}
	return e, secret, nil
}

// insertEndpoint creates an endpoint in tx as CreateEndpoint describes.
func (s *SQLStore) insertEndpoint(ctx context.Context, tx *sql.Tx, accountID string, in NewEndpoint) (Endpoint, string, error) {
	secret, err := ids.NewSecret()
	if err != nil {
		return Endpoint{}, "", fmt.Errorf("store: create endpoint: %w", err)
	}
	now := s.nowUTC()
	e := Endpoint{
		ID:          ids.NewEndpointID(),
		AccountID:   accountID,
		URL:         in.URL,
		Description: in.Description,
		EventTypes:  normalizeEventTypes(in.EventTypes),
		Enabled:     true,
		CreatedAt:   now,
		UpdatedAt:   now,
	}
	types, err := json.Marshal(e.EventTypes)
	if err != nil {
		return Endpoint{}, "", fmt.Errorf("store: create endpoint: %w", err)
	}

	var id string
	err = s.queryRow(ctx, tx, `SELECT id FROM accounts WHERE id = ?`, accountID).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return Endpoint{}, "", ErrNotFound
	}
	if err != nil {
		return Endpoint{}, "", fmt.Errorf("store: create endpoint: %w", err)
	}
	if _, err := s.exec(ctx, tx,
		`INSERT INTO endpoints (id, account_id, url, description, event_types, enabled, secret, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		e.ID, e.AccountID, e.URL, e.Description, string(types), e.Enabled, secret,
		s.d.timeArg(e.CreatedAt), s.d.timeArg(e.UpdatedAt)); err != nil {
		return Endpoint{}, "", fmt.Errorf("store: create endpoint: %w", err)
	}
	return e, secret, nil
}

// GetEndpoint implements Store.
func (s *SQLStore) GetEndpoint(ctx context.Context, accountID, id string) (Endpoint, error) {
	return s.getEndpoint(ctx, s.db, accountID, id)
}

func (s *SQLStore) getEndpoint(ctx context.Context, q queryer, accountID, id string) (Endpoint, error) {
	row := s.queryRow(ctx, q,
		`SELECT `+endpointColumns+` FROM endpoints WHERE account_id = ? AND id = ? AND deleted_at IS NULL`,
		accountID, id)
	e, err := scanEndpoint(row)
	if errors.Is(err, sql.ErrNoRows) {
		return Endpoint{}, ErrNotFound
	}
	if err != nil {
		return Endpoint{}, fmt.Errorf("store: get endpoint: %w", err)
	}
	return e, nil
}

// GetEndpointSecret implements Store.
func (s *SQLStore) GetEndpointSecret(ctx context.Context, accountID, id string) (string, error) {
	var secret string
	err := s.queryRow(ctx, s.db,
		`SELECT secret FROM endpoints WHERE account_id = ? AND id = ? AND deleted_at IS NULL`,
		accountID, id).Scan(&secret)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrNotFound
	}
	if err != nil {
		return "", fmt.Errorf("store: get endpoint secret: %w", err)
	}
	return secret, nil
}

// ListEndpoints implements Store. IDs are ULIDs, so ordering by ID is
// ordering by creation time.
func (s *SQLStore) ListEndpoints(ctx context.Context, accountID string, limit int, after string) ([]Endpoint, error) {
	if limit <= 0 {
		return nil, fmt.Errorf("store: list endpoints: limit %d is not positive", limit)
	}
	rows, err := s.query(ctx, s.db,
		`SELECT `+endpointColumns+` FROM endpoints
		WHERE account_id = ? AND deleted_at IS NULL AND id > ?
		ORDER BY id
		LIMIT ?`,
		accountID, after, limit)
	if err != nil {
		return nil, fmt.Errorf("store: list endpoints: %w", err)
	}
	defer rows.Close()

	eps := []Endpoint{}
	for rows.Next() {
		e, err := scanEndpoint(rows)
		if err != nil {
			return nil, fmt.Errorf("store: list endpoints: %w", err)
		}
		eps = append(eps, e)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: list endpoints: %w", err)
	}
	return eps, nil
}

// UpdateEndpoint implements Store.
func (s *SQLStore) UpdateEndpoint(ctx context.Context, accountID, id string, u EndpointUpdate) (Endpoint, error) {
	var e Endpoint
	err := s.inTx(ctx, func(tx *sql.Tx) error {
		var err error
		e, err = s.getEndpoint(ctx, tx, accountID, id)
		if err != nil {
			return err
		}
		if u.URL != nil {
			e.URL = *u.URL
		}
		if u.Description != nil {
			e.Description = *u.Description
		}
		if u.EventTypes != nil {
			e.EventTypes = normalizeEventTypes(*u.EventTypes)
		}
		if u.Enabled != nil {
			e.Enabled = *u.Enabled
		}
		e.UpdatedAt = s.nowUTC()

		types, err := json.Marshal(e.EventTypes)
		if err != nil {
			return fmt.Errorf("store: update endpoint: %w", err)
		}
		if _, err := s.exec(ctx, tx,
			`UPDATE endpoints SET url = ?, description = ?, event_types = ?, enabled = ?, updated_at = ?
			WHERE account_id = ? AND id = ? AND deleted_at IS NULL`,
			e.URL, e.Description, string(types), e.Enabled, s.d.timeArg(e.UpdatedAt),
			accountID, id); err != nil {
			return fmt.Errorf("store: update endpoint: %w", err)
		}
		return nil
	})
	if err != nil {
		return Endpoint{}, err
	}
	return e, nil
}

// DeleteEndpoint implements Store.
func (s *SQLStore) DeleteEndpoint(ctx context.Context, accountID, id string) error {
	now := s.d.timeArg(s.nowUTC())
	return s.inTx(ctx, func(tx *sql.Tx) error {
		res, err := s.exec(ctx, tx,
			`UPDATE endpoints SET deleted_at = ?, updated_at = ?
			WHERE account_id = ? AND id = ? AND deleted_at IS NULL`,
			now, now, accountID, id)
		if err != nil {
			return fmt.Errorf("store: delete endpoint: %w", err)
		}
		n, err := res.RowsAffected()
		if err != nil {
			return fmt.Errorf("store: delete endpoint: %w", err)
		}
		if n == 0 {
			return ErrNotFound
		}
		// The test_receivers row stays, so that its URL answers 404.
		if _, err := s.exec(ctx, tx,
			`DELETE FROM test_requests
			WHERE receiver_id IN (SELECT id FROM test_receivers WHERE endpoint_id = ?)`,
			id); err != nil {
			return fmt.Errorf("store: delete endpoint: test requests: %w", err)
		}
		return nil
	})
}

// scanner is satisfied by both *sql.Row and *sql.Rows.
type scanner interface {
	Scan(dest ...any) error
}

// scanEndpoint reads one row of endpointColumns.
func scanEndpoint(sc scanner) (Endpoint, error) {
	var (
		e                Endpoint
		types            string
		created, updated dbTime
	)
	if err := sc.Scan(&e.ID, &e.AccountID, &e.URL, &e.Description, &types, &e.Enabled, &created, &updated); err != nil {
		return Endpoint{}, err
	}
	if err := json.Unmarshal([]byte(types), &e.EventTypes); err != nil {
		return Endpoint{}, fmt.Errorf("decode event_types of %s: %w", e.ID, err)
	}
	e.EventTypes = normalizeEventTypes(e.EventTypes)
	e.CreatedAt = created.Time
	e.UpdatedAt = updated.Time
	return e, nil
}

// normalizeEventTypes returns a copy of types that is never nil, so that it
// is stored as "[]" rather than "null" and does not share the caller's array.
func normalizeEventTypes(types []string) []string {
	return append([]string{}, types...)
}
