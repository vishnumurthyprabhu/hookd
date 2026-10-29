package store

import (
	"context"
	"errors"
	"time"
)

// ErrNotFound is returned when a record does not exist or belongs to another
// account.
var ErrNotFound = errors.New("not found")

// Store holds all of Hookd's state. Every method that reads or writes an
// account's data is scoped by account ID.
type Store interface {
	// Migrate applies the migrations that have not been applied yet and
	// returns their versions, in order. It returns none when the database is
	// up to date.
	Migrate(ctx context.Context) ([]string, error)
	// Ping checks that the database can be reached.
	Ping(ctx context.Context) error
	// Close releases the database connections.
	Close() error

	// CreateAccount creates an account with the given name.
	CreateAccount(ctx context.Context, name string) (Account, error)
	// CreateAPIKey creates an API key for an account and returns its record
	// and the plain key. Only the key's hash is stored, so the plain key
	// cannot be read again. It returns ErrNotFound if the account does not
	// exist.
	CreateAPIKey(ctx context.Context, accountID string) (APIKey, string, error)
	// AccountByAPIKeyHash returns the account that owns the API key with the
	// given hash (see ids.HashAPIKey), or ErrNotFound.
	AccountByAPIKeyHash(ctx context.Context, keyHash string) (Account, error)
}

// Account is a customer of Hookd. All other records belong to one.
type Account struct {
	ID        string
	Name      string
	CreatedAt time.Time
}

// APIKey is the stored record of an API key. It holds the key's display
// prefix but never the key itself.
type APIKey struct {
	ID        string
	AccountID string
	Prefix    string
	CreatedAt time.Time
}
