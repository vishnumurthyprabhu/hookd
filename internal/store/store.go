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

	// CreateEvent stores an event for an account and, in the same
	// transaction, one pending delivery for each of the account's enabled,
	// non-deleted endpoints that subscribe to its type. If in has an
	// idempotency key and the account has an event with that key created
	// within the idempotency window, that event is returned instead and
	// nothing is created; created is false then. An older event with the key
	// loses it. It returns ErrNotFound if the account does not exist.
	CreateEvent(ctx context.Context, accountID string, in NewEvent) (e Event, created bool, err error)
	// GetEvent returns one of the account's events with its deliveries, or
	// ErrNotFound if it does not exist or belongs to another account.
	GetEvent(ctx context.Context, accountID, id string) (Event, error)

	// ClaimDelivery atomically takes the oldest delivery that is pending, or
	// in progress with an expired lease, marks it in progress and locks it
	// for lease. No other caller can claim it until the lease expires. ok is
	// false when there is nothing to claim. lease must be positive.
	ClaimDelivery(ctx context.Context, lease time.Duration) (d ClaimedDelivery, ok bool, err error)
	// GetDeliveryJob loads what a worker needs to send a delivery: its event,
	// its endpoint and the endpoint's signing secret. The endpoint is
	// returned even if it has been disabled or deleted since the delivery was
	// created; see DeliveryJob.EndpointDeleted. It returns ErrNotFound if the
	// delivery does not exist.
	GetDeliveryJob(ctx context.Context, deliveryID string) (DeliveryJob, error)
	// RecordDeliveryResult stores the outcome of an in-progress delivery and
	// releases its lease. r.Status must be DeliverySucceeded or
	// DeliveryFailed. It returns ErrNotFound if the delivery does not exist
	// or is not in progress.
	RecordDeliveryResult(ctx context.Context, deliveryID string, r DeliveryResult) error
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

// NewEvent holds the fields of an event to create. Input is checked by the
// API before it reaches the store.
type NewEvent struct {
	EventType string
	// Payload is stored and sent exactly as given.
	Payload []byte
	// IdempotencyKey is optional. Empty means none.
	IdempotencyKey string
}

// Event is a message sent by an account, with its delivery to each target
// endpoint.
type Event struct {
	ID             string
	AccountID      string
	EventType      string
	Payload        []byte
	IdempotencyKey string
	CreatedAt      time.Time
	// Deliveries holds one entry per target endpoint, ordered by endpoint
	// ID. It is never nil.
	Deliveries []DeliveryStatus
}

// Delivery states.
const (
	DeliveryPending    = "pending"
	DeliveryInProgress = "in_progress"
	DeliverySucceeded  = "succeeded"
	DeliveryFailed     = "failed"
)

// DeliveryStatus is where the delivery of an event to one endpoint stands.
type DeliveryStatus struct {
	EndpointID string
	// Status is one of the Delivery* states.
	Status string
	// ResponseStatus is the endpoint's HTTP status, or 0 if there was none.
	ResponseStatus int
	// Error describes why the delivery failed, or is empty.
	Error string
}

// ClaimedDelivery is a delivery taken by ClaimDelivery.
type ClaimedDelivery struct {
	ID         string
	EventID    string
	EndpointID string
	// LockedUntil is when the lease expires and the delivery can be claimed
	// again.
	LockedUntil time.Time
}

// DeliveryJob is what a worker needs to send one delivery.
type DeliveryJob struct {
	DeliveryID string
	// Event is the event to send. Its Deliveries is empty.
	Event Event
	// Endpoint is the target, even if it is disabled or deleted.
	Endpoint Endpoint
	// Secret is the endpoint's signing secret.
	Secret string
	// EndpointDeleted is true if the endpoint has been deleted.
	EndpointDeleted bool
}

// DeliveryResult is the outcome of one delivery attempt.
type DeliveryResult struct {
	// Status is DeliverySucceeded or DeliveryFailed.
	Status string
	// ResponseStatus is the endpoint's HTTP status, or 0 if there was none.
	ResponseStatus int
	// Error describes why the delivery failed, or is empty.
	Error string
	// Latency is how long the attempt took.
	Latency time.Duration
	// AttemptedAt is when the attempt started. Zero means the store's
	// current time.
	AttemptedAt time.Time
}

// EndpointUpdate holds the fields to change on an endpoint. Nil fields are
// left as they are.
type EndpointUpdate struct {
	URL         *string
	Description *string
	EventTypes  *[]string
	Enabled     *bool
}
