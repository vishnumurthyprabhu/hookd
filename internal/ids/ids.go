package ids

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"

	"github.com/oklog/ulid/v2"
)

// Type prefixes for IDs. The ULID that follows sorts by creation time.
const (
	PrefixAccount      = "acct_"
	PrefixEndpoint     = "ep_"
	PrefixMessage      = "msg_"
	PrefixDelivery     = "dlv_"
	PrefixAPIKey       = "key_"
	PrefixTestReceiver = "trcv_"
	PrefixTestRequest  = "treq_"
)

const (
	// SecretPrefix starts every endpoint signing secret (Standard Webhooks format).
	SecretPrefix = "whsec_"
	// SecretBytes is the number of random bytes in an endpoint signing secret.
	SecretBytes = 24

	// APIKeyPrefix starts every API key.
	APIKeyPrefix = "hk_"
	// APIKeyBytes is the number of random bytes in an API key.
	APIKeyBytes = 32
	// APIKeyDisplayLen is the length of the key prefix kept for display.
	APIKeyDisplayLen = len(APIKeyPrefix) + 8
)

// New returns prefix followed by a new ULID. IDs made by one process sort by
// creation time, including several made within the same millisecond.
func New(prefix string) string {
	return prefix + ulid.Make().String()
}

// NewAccountID returns a new account ID (acct_…).
func NewAccountID() string { return New(PrefixAccount) }

// NewEndpointID returns a new endpoint ID (ep_…).
func NewEndpointID() string { return New(PrefixEndpoint) }

// NewMessageID returns a new event ID (msg_…), used as the webhook-id.
func NewMessageID() string { return New(PrefixMessage) }

// NewDeliveryID returns a new delivery ID (dlv_…).
func NewDeliveryID() string { return New(PrefixDelivery) }

// NewAPIKeyID returns a new API key record ID (key_…).
func NewAPIKeyID() string { return New(PrefixAPIKey) }

// NewTestReceiverID returns a new built-in test receiver ID (trcv_…).
func NewTestReceiverID() string { return New(PrefixTestReceiver) }

// NewTestRequestID returns a new ID (treq_…) for a request received by a
// test receiver.
func NewTestRequestID() string { return New(PrefixTestRequest) }

// NewSecret returns a new endpoint signing secret: "whsec_" followed by the
// standard base64 encoding of 24 random bytes.
func NewSecret() (string, error) {
	b, err := randomBytes(SecretBytes)
	if err != nil {
		return "", err
	}
	return SecretPrefix + base64.StdEncoding.EncodeToString(b), nil
}

// NewAPIKey returns a new API key: "hk_" followed by the unpadded base64url
// encoding of 32 random bytes.
func NewAPIKey() (string, error) {
	b, err := randomBytes(APIKeyBytes)
	if err != nil {
		return "", err
	}
	return APIKeyPrefix + base64.RawURLEncoding.EncodeToString(b), nil
}

// HashAPIKey returns the hex-encoded SHA-256 hash of key, which is what is
// stored and looked up.
func HashAPIKey(key string) string {
	sum := sha256.Sum256([]byte(key))
	return hex.EncodeToString(sum[:])
}

// APIKeyDisplayPrefix returns the start of key that may be stored and shown
// to identify it, such as "hk_AbCdEfGh".
func APIKeyDisplayPrefix(key string) string {
	if len(key) <= APIKeyDisplayLen {
		return key
	}
	return key[:APIKeyDisplayLen]
}

// APIKeyHashAndPrefix returns the SHA-256 hash and the display prefix of key.
func APIKeyHashAndPrefix(key string) (hash, prefix string) {
	return HashAPIKey(key), APIKeyDisplayPrefix(key)
}

func randomBytes(n int) ([]byte, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return nil, fmt.Errorf("read random bytes: %w", err)
	}
	return b, nil
}
