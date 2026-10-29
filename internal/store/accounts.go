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
