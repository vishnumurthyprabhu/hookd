package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"hookd/internal/ids"
)

// CreateAccount implements Store.
func (s *SQLStore) CreateAccount(ctx context.Context, name string) (Account, error) {
	a := Account{ID: ids.NewAccountID(), Name: name, CreatedAt: s.nowUTC()}
	if _, err := s.exec(ctx, s.db, `INSERT INTO accounts (id, name, created_at) VALUES (?, ?, ?)`,
		a.ID, a.Name, s.d.timeArg(a.CreatedAt)); err != nil {
		return Account{}, fmt.Errorf("store: create account: %w", err)
	}
	return a, nil
}

// GetAccount implements Store.
func (s *SQLStore) GetAccount(ctx context.Context, id string) (Account, error) {
	var (
		a       Account
		created dbTime
	)
	err := s.queryRow(ctx, s.db, `SELECT id, name, created_at FROM accounts WHERE id = ?`, id).
		Scan(&a.ID, &a.Name, &created)
	if errors.Is(err, sql.ErrNoRows) {
		return Account{}, ErrNotFound
	}
	if err != nil {
		return Account{}, fmt.Errorf("store: get account: %w", err)
	}
	a.CreatedAt = created.Time
	return a, nil
}

// ListAccounts implements Store. IDs are ULIDs, so ordering by ID is ordering
// by creation time. The counts use the (account_id, …) indexes of endpoints
// and api_keys.
func (s *SQLStore) ListAccounts(ctx context.Context, limit int, after string) ([]AccountSummary, error) {
	if limit <= 0 {
		return nil, fmt.Errorf("store: list accounts: limit %d is not positive", limit)
	}
	rows, err := s.query(ctx, s.db,
		`SELECT a.id, a.name, a.created_at,
			(SELECT count(*) FROM endpoints e WHERE e.account_id = a.id AND e.deleted_at IS NULL),
			(SELECT count(*) FROM api_keys k WHERE k.account_id = a.id)
		FROM accounts a
		WHERE a.id > ?
		ORDER BY a.id
		LIMIT ?`,
		after, limit)
	if err != nil {
		return nil, fmt.Errorf("store: list accounts: %w", err)
	}
	defer rows.Close()

	accts := []AccountSummary{}
	for rows.Next() {
		var (
			a       AccountSummary
			created dbTime
		)
		if err := rows.Scan(&a.ID, &a.Name, &created, &a.Endpoints, &a.APIKeys); err != nil {
			return nil, fmt.Errorf("store: list accounts: %w", err)
		}
		a.CreatedAt = created.Time
		accts = append(accts, a)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: list accounts: %w", err)
	}
	return accts, nil
}

// CreateAPIKey implements Store.
func (s *SQLStore) CreateAPIKey(ctx context.Context, accountID string) (APIKey, string, error) {
	key, err := ids.NewAPIKey()
	if err != nil {
		return APIKey{}, "", fmt.Errorf("store: create api key: %w", err)
	}
	hash, prefix := ids.APIKeyHashAndPrefix(key)
	k := APIKey{ID: ids.NewAPIKeyID(), AccountID: accountID, Prefix: prefix, CreatedAt: s.nowUTC()}

	err = s.inTx(ctx, func(tx *sql.Tx) error {
		var id string
		err := s.queryRow(ctx, tx, `SELECT id FROM accounts WHERE id = ?`, accountID).Scan(&id)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return fmt.Errorf("store: create api key: %w", err)
		}
		if _, err := s.exec(ctx, tx,
			`INSERT INTO api_keys (id, account_id, key_hash, key_prefix, created_at) VALUES (?, ?, ?, ?, ?)`,
			k.ID, k.AccountID, hash, k.Prefix, s.d.timeArg(k.CreatedAt)); err != nil {
			return fmt.Errorf("store: create api key: %w", err)
		}
		return nil
	})
	if err != nil {
		return APIKey{}, "", err
	}
	return k, key, nil
}

// ListAPIKeys implements Store. IDs are ULIDs, so ordering by ID is ordering
// by creation time.
func (s *SQLStore) ListAPIKeys(ctx context.Context, accountID string) ([]APIKey, error) {
	keys := []APIKey{}
	err := s.inTx(ctx, func(tx *sql.Tx) error {
		var id string
		err := s.queryRow(ctx, tx, `SELECT id FROM accounts WHERE id = ?`, accountID).Scan(&id)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return fmt.Errorf("store: list api keys: %w", err)
		}
		rows, err := s.query(ctx, tx,
			`SELECT id, account_id, key_prefix, created_at FROM api_keys WHERE account_id = ? ORDER BY id`,
			accountID)
		if err != nil {
			return fmt.Errorf("store: list api keys: %w", err)
		}
		defer rows.Close()
		for rows.Next() {
			k, err := scanAPIKey(rows)
			if err != nil {
				return fmt.Errorf("store: list api keys: %w", err)
			}
			keys = append(keys, k)
		}
		if err := rows.Err(); err != nil {
			return fmt.Errorf("store: list api keys: %w", err)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return keys, nil
}

// DeleteAPIKey implements Store.
func (s *SQLStore) DeleteAPIKey(ctx context.Context, id string) (APIKey, error) {
	var k APIKey
	err := s.inTx(ctx, func(tx *sql.Tx) error {
		var err error
		k, err = scanAPIKey(s.queryRow(ctx, tx,
			`SELECT id, account_id, key_prefix, created_at FROM api_keys WHERE id = ?`, id))
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return fmt.Errorf("store: delete api key: %w", err)
		}
		if _, err := s.exec(ctx, tx, `DELETE FROM api_keys WHERE id = ?`, id); err != nil {
			return fmt.Errorf("store: delete api key: %w", err)
		}
		return nil
	})
	if err != nil {
		return APIKey{}, err
	}
	return k, nil
}

// scanAPIKey reads one row of id, account_id, key_prefix, created_at.
func scanAPIKey(sc scanner) (APIKey, error) {
	var (
		k       APIKey
		created dbTime
	)
	if err := sc.Scan(&k.ID, &k.AccountID, &k.Prefix, &created); err != nil {
		return APIKey{}, err
	}
	k.CreatedAt = created.Time
	return k, nil
}

// AccountByAPIKeyHash implements Store.
func (s *SQLStore) AccountByAPIKeyHash(ctx context.Context, keyHash string) (Account, error) {
	var (
		a       Account
		created dbTime
	)
	err := s.queryRow(ctx, s.db,
		`SELECT a.id, a.name, a.created_at FROM api_keys k JOIN accounts a ON a.id = k.account_id WHERE k.key_hash = ?`,
		keyHash).Scan(&a.ID, &a.Name, &created)
	if errors.Is(err, sql.ErrNoRows) {
		return Account{}, ErrNotFound
	}
	if err != nil {
		return Account{}, fmt.Errorf("store: look up api key: %w", err)
	}
	a.CreatedAt = created.Time
	return a, nil
}
