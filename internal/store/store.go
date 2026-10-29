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

	// CreateEndpoint creates an enabled endpoint for an account with a new
	// signing secret, and returns the endpoint and the secret. It returns
	// ErrNotFound if the account does not exist.
	CreateEndpoint(ctx context.Context, accountID string, in NewEndpoint) (Endpoint, string, error)
	// GetEndpoint returns one of the account's endpoints, or ErrNotFound if
	// it does not exist, belongs to another account or has been deleted.
	GetEndpoint(ctx context.Context, accountID, id string) (Endpoint, error)
	// GetEndpointSecret returns the signing secret of one of the account's
	// endpoints, with the same ErrNotFound rules as GetEndpoint.
	GetEndpointSecret(ctx context.Context, accountID, id string) (string, error)
	// ListEndpoints returns up to limit of the account's endpoints, oldest
	// first, leaving out deleted ones. If after is not empty, only endpoints
	// whose ID sorts after it are returned, so the last ID of one page is the
	// cursor for the next. limit must be positive.
	ListEndpoints(ctx context.Context, accountID string, limit int, after string) ([]Endpoint, error)
	// UpdateEndpoint changes the fields set in u and returns the updated
	// endpoint, with the same ErrNotFound rules as GetEndpoint.
	UpdateEndpoint(ctx context.Context, accountID, id string, u EndpointUpdate) (Endpoint, error)
	// DeleteEndpoint soft-deletes one of the account's endpoints, with the
	// same ErrNotFound rules as GetEndpoint. A deleted endpoint is never
	// returned or changed again.
	DeleteEndpoint(ctx context.Context, accountID, id string) error
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

// Endpoint is a URL that receives an account's events. Its signing secret is
// left out; read it with GetEndpointSecret.
type Endpoint struct {
	ID          string
	AccountID   string
	URL         string
	Description string
	// EventTypes lists the event types sent to the endpoint. Empty means all.
	// It is never nil.
	EventTypes []string
	Enabled    bool
	CreatedAt  time.Time
	UpdatedAt  time.Time
}

// NewEndpoint holds the fields of an endpoint to create. Input is checked by
// the API before it reaches the store.
type NewEndpoint struct {
	URL         string
	Description string
	EventTypes  []string
}

// EndpointUpdate holds the fields to change on an endpoint. Nil fields are
// left as they are.
type EndpointUpdate struct {
	URL         *string
	Description *string
	EventTypes  *[]string
	Enabled     *bool
}
