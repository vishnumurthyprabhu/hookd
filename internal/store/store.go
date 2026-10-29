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
	// PendingMigrations returns the versions of the migrations that have not
	// been applied yet, in order. It returns an empty slice when the
	// database is up to date, and every version when it has never been
	// migrated.
	PendingMigrations(ctx context.Context) ([]string, error)
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
	// ListAPIKeys returns the records of an account's API keys, oldest
	// first. It returns ErrNotFound if the account does not exist, and an
	// empty slice if it has no keys.
	ListAPIKeys(ctx context.Context, accountID string) ([]APIKey, error)
	// DeleteAPIKey deletes the API key with the given record ID, so the key
	// no longer authenticates, and returns the deleted record. It returns
	// ErrNotFound if there is no such key.
	DeleteAPIKey(ctx context.Context, id string) (APIKey, error)
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
	// returned or changed again. If it is a test endpoint, the requests its
	// receiver got are deleted too.
	DeleteEndpoint(ctx context.Context, accountID, id string) error

	// CreateTestEndpoint creates, in one transaction, a built-in test
	// receiver and the endpoint that delivers to it, with the URL
	// <in.BaseURL>/admin/test-receiver/<receiver ID> and a new signing
	// secret. The endpoint is an ordinary endpoint in every other way. It
	// returns ErrNotFound if the account does not exist.
	CreateTestEndpoint(ctx context.Context, accountID string, in NewTestEndpoint) (TestEndpoint, error)
	// GetTestReceiver returns a test receiver by its ID alone, which is what
	// the receiver's URL carries, with its endpoint and the endpoint's
	// signing secret. The endpoint is returned even if it has been deleted;
	// see TestReceiverDetail.EndpointDeleted. It returns ErrNotFound if
	// there is no such receiver.
	GetTestReceiver(ctx context.Context, id string) (TestReceiverDetail, error)
	// ListTestReceivers returns the account's test receivers whose endpoint
	// is not deleted, ordered by ID. It returns an empty slice if there are
	// none.
	ListTestReceivers(ctx context.Context, accountID string) ([]TestReceiver, error)
	// SetTestReceiverStatus sets the HTTP status that the receiver of one of
	// the account's test endpoints answers with. Status is checked by the
	// caller. It returns ErrNotFound if the endpoint does not exist, belongs
	// to another account, has been deleted or is not a test endpoint.
	SetTestReceiverStatus(ctx context.Context, accountID, endpointID string, status int) error
	// RecordTestRequest stores a request received by a test receiver and, in
	// the same transaction, deletes that receiver's requests beyond the
	// newest MaxTestRequests. The event type is taken from the receiver's
	// account's event whose ID is in.WebhookID, and is empty if there is
	// none. It returns ErrNotFound, storing nothing, if the receiver does
	// not exist or its endpoint has been deleted.
	RecordTestRequest(ctx context.Context, receiverID string, in NewTestRequest) (TestRequest, error)
	// ListTestRequests returns up to limit of the requests received by one of
	// the account's test endpoints, newest first, with the same ErrNotFound
	// rules as SetTestReceiverStatus. limit must be positive.
	ListTestRequests(ctx context.Context, accountID, endpointID string, limit int) ([]TestRequest, error)
	// ClearTestRequests deletes the requests received by one of the
	// account's test endpoints, with the same ErrNotFound rules as
	// SetTestReceiverStatus.
	ClearTestRequests(ctx context.Context, accountID, endpointID string) error

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
	// CountDeliveriesByStatus returns the number of deliveries in each of
	// the Delivery* states, across all accounts. Every state is in the map,
	// with 0 if no delivery is in it.
	CountDeliveriesByStatus(ctx context.Context) (map[string]int, error)

	// ListSettings returns the settings saved in the UI, ordered by name. It
	// returns an empty slice if there are none.
	ListSettings(ctx context.Context) ([]Setting, error)
	// SetSetting saves the value of a setting, replacing any value it had.
	// Values are checked by the caller before they reach the store.
	SetSetting(ctx context.Context, name, value string) error
	// DeleteSetting removes a saved setting, so the environment or the
	// default applies again. Deleting a setting that is not saved is not an
	// error.
	DeleteSetting(ctx context.Context, name string) error
}

// Setting is a setting saved in the UI, which overrides the environment.
type Setting struct {
	// Name is the setting's environment variable, such as HOOKD_WORKERS.
	Name      string
	Value     string
	UpdatedAt time.Time
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

// MaxTestRequests is the number of requests kept for each test receiver.
const MaxTestRequests = 100

// DefaultTestResponseStatus is the status a new test receiver answers with
// when NewTestEndpoint.ResponseStatus is 0.
const DefaultTestResponseStatus = 200

// NewTestEndpoint holds the fields of a test endpoint to create. Input is
// checked by the caller before it reaches the store.
type NewTestEndpoint struct {
	Description string
	EventTypes  []string
	// ResponseStatus is the HTTP status the receiver answers with. 0 means
	// DefaultTestResponseStatus.
	ResponseStatus int
	// BaseURL is where the workers reach Hookd's admin routes, such as
	// http://127.0.0.1:8080 (config.Config.ReceiverBaseURL).
	BaseURL string
}

// TestEndpoint is what CreateTestEndpoint made.
type TestEndpoint struct {
	Endpoint Endpoint
	// Secret is the endpoint's signing secret.
	Secret   string
	Receiver TestReceiver
}

// TestReceiver is Hookd's built-in receiver behind a test endpoint.
type TestReceiver struct {
	ID         string
	AccountID  string
	EndpointID string
	// ResponseStatus is the HTTP status it answers with.
	ResponseStatus int
	CreatedAt      time.Time
}

// TestReceiverDetail is what the receiver needs to handle a request.
type TestReceiverDetail struct {
	Receiver TestReceiver
	// Endpoint is the test endpoint, even if it is deleted.
	Endpoint Endpoint
	// Secret is the endpoint's signing secret.
	Secret string
	// EndpointDeleted is true if the endpoint has been deleted.
	EndpointDeleted bool
}

// NewTestRequest holds a request received by a test receiver. Text that is
// not valid UTF-8, and NUL bytes, are stored as U+FFFD.
type NewTestRequest struct {
	// WebhookID is the request's webhook-id header, or empty.
	WebhookID string
	Headers   map[string][]string
	Body      []byte
	// Verified is true if the signature checked out.
	Verified bool
	// VerifyError says why it did not, or is empty.
	VerifyError string
	// ResponseStatus is the HTTP status the receiver answered with.
	ResponseStatus int
}

// TestRequest is a request received by a test receiver.
type TestRequest struct {
	ID         string
	ReceiverID string
	ReceivedAt time.Time
	WebhookID  string
	// EventType is the type of the account's event whose ID is WebhookID,
	// or empty if there is none.
	EventType string
	// Headers is never nil.
	Headers        map[string][]string
	Body           []byte
	Verified       bool
	VerifyError    string
	ResponseStatus int
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
