package ids

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"regexp"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/oklog/ulid/v2"
)

var ulidPattern = regexp.MustCompile(`^[0-9A-HJKMNP-TV-Z]{26}$`)

func TestIDFormats(t *testing.T) {
	cases := []struct {
		prefix string
		gen    func() string
	}{
		{"acct_", NewAccountID},
		{"ep_", NewEndpointID},
		{"msg_", NewMessageID},
		{"dlv_", NewDeliveryID},
		{"key_", NewAPIKeyID},
		{"trcv_", NewTestReceiverID},
		{"treq_", NewTestRequestID},
	}
	for _, c := range cases {
		id := c.gen()
		rest, ok := strings.CutPrefix(id, c.prefix)
		if !ok {
			t.Errorf("%q does not start with %q", id, c.prefix)
			continue
		}
		if !ulidPattern.MatchString(rest) {
			t.Errorf("%q: %q is not a ULID", id, rest)
		}
		u, err := ulid.ParseStrict(rest)
		if err != nil {
			t.Errorf("%q: parse ULID: %v", id, err)
			continue
		}
		if d := time.Since(ulid.Time(u.Time())); d < -time.Second || d > time.Minute {
			t.Errorf("%q: ULID time is %v away from now", id, d)
		}
	}
}

func TestIDsSortByCreationTime(t *testing.T) {
	var ids []string
	for i := 0; i < 1000; i++ {
		ids = append(ids, NewMessageID())
		if i%250 == 0 {
			time.Sleep(2 * time.Millisecond)
		}
	}
	if !sort.StringsAreSorted(ids) {
		t.Fatal("IDs are not in creation order when sorted as strings")
	}
}

func TestIDsUnique(t *testing.T) {
	seen := make(map[string]bool)
	for i := 0; i < 10000; i++ {
		id := NewEndpointID()
		if seen[id] {
			t.Fatalf("duplicate ID %q", id)
		}
		seen[id] = true
	}
}

func TestNewSecret(t *testing.T) {
	seen := make(map[string]bool)
	for i := 0; i < 1000; i++ {
		s, err := NewSecret()
		if err != nil {
			t.Fatal(err)
		}
		rest, ok := strings.CutPrefix(s, "whsec_")
		if !ok {
			t.Fatalf("%q does not start with whsec_", s)
		}
		b, err := base64.StdEncoding.DecodeString(rest)
		if err != nil {
			t.Fatalf("%q: decode: %v", s, err)
		}
		if len(b) != 24 {
			t.Fatalf("%q decodes to %d bytes, want 24", s, len(b))
		}
		if seen[s] {
			t.Fatalf("duplicate secret %q", s)
		}
		seen[s] = true
	}
}

func TestNewAPIKey(t *testing.T) {
	seen := make(map[string]bool)
	for i := 0; i < 1000; i++ {
		k, err := NewAPIKey()
		if err != nil {
			t.Fatal(err)
		}
		rest, ok := strings.CutPrefix(k, "hk_")
		if !ok {
			t.Fatalf("%q does not start with hk_", k)
		}
		b, err := base64.RawURLEncoding.DecodeString(rest)
		if err != nil {
			t.Fatalf("%q: decode: %v", k, err)
		}
		if len(b) != 32 {
			t.Fatalf("%q decodes to %d bytes, want 32", k, len(b))
		}
		if seen[k] {
			t.Fatalf("duplicate API key %q", k)
		}
		seen[k] = true
	}
}

func TestAPIKeyHashAndPrefix(t *testing.T) {
	k, err := NewAPIKey()
	if err != nil {
		t.Fatal(err)
	}
	hash, prefix := APIKeyHashAndPrefix(k)

	sum := sha256.Sum256([]byte(k))
	if want := hex.EncodeToString(sum[:]); hash != want {
		t.Errorf("hash = %q, want %q", hash, want)
	}
	if hash != HashAPIKey(k) {
		t.Error("HashAPIKey is not deterministic")
	}
	if !strings.HasPrefix(k, prefix) || len(prefix) != len("hk_")+8 {
		t.Errorf("prefix = %q, want the first 11 characters of %q", prefix, k)
	}

	other, err := NewAPIKey()
	if err != nil {
		t.Fatal(err)
	}
	if HashAPIKey(other) == hash {
		t.Error("two keys have the same hash")
	}
	if got := APIKeyDisplayPrefix("hk_ab"); got != "hk_ab" {
		t.Errorf("short key prefix = %q, want %q", got, "hk_ab")
	}
}
