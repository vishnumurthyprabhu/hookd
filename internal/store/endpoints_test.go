package store_test

import (
	"context"
	"database/sql"
	"encoding/base64"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	_ "modernc.org/sqlite"

	"hookd/internal/ids"
	"hookd/internal/store"
	"hookd/internal/store/storetest"
)

// clock is a settable time source for store.WithClock.
type clock struct{ t time.Time }

func (c *clock) now() time.Time          { return c.t }
func (c *clock) advance(d time.Duration) { c.t = c.t.Add(d) }
func ptr[T any](v T) *T                  { return &v }

func mustAccount(t *testing.T, s store.Store) store.Account {
	t.Helper()
	a, err := s.CreateAccount(context.Background(), "acme")
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func mustEndpoint(t *testing.T, s store.Store, accountID string, in store.NewEndpoint) store.Endpoint {
	t.Helper()
	e, _, err := s.CreateEndpoint(context.Background(), accountID, in)
	if err != nil {
		t.Fatal(err)
	}
	return e
}

func TestCreateAndGetEndpoint(t *testing.T) {
	ctx := context.Background()
	c := &clock{t: time.Date(2026, 10, 29, 12, 0, 0, 987654321, time.UTC)}
	s := storetest.New(t, store.WithClock(c.now))
	acct := mustAccount(t, s)

	e, secret, err := s.CreateEndpoint(ctx, acct.ID, store.NewEndpoint{
		URL:         "https://example.com/hook",
		Description: "orders",
		EventTypes:  []string{"order.created", "order.paid"},
	})
	if err != nil {
		t.Fatal(err)
	}
	ts := c.t.Truncate(time.Microsecond)
	want := store.Endpoint{
		ID:          e.ID,
		AccountID:   acct.ID,
		URL:         "https://example.com/hook",
		Description: "orders",
		EventTypes:  []string{"order.created", "order.paid"},
		Enabled:     true,
		CreatedAt:   ts,
		UpdatedAt:   ts,
	}
	if !strings.HasPrefix(e.ID, ids.PrefixEndpoint) {
		t.Errorf("ID %q lacks prefix %q", e.ID, ids.PrefixEndpoint)
	}
	if !reflect.DeepEqual(e, want) {
		t.Fatalf("CreateEndpoint = %+v, want %+v", e, want)
	}

	// The secret is in Standard Webhooks format.
	raw, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(secret, ids.SecretPrefix))
	if !strings.HasPrefix(secret, ids.SecretPrefix) || err != nil || len(raw) < 24 {
		t.Fatalf("secret %q is not whsec_ + base64 of 24+ bytes (err %v)", secret, err)
	}

	got, err := s.GetEndpoint(ctx, acct.ID, e.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("GetEndpoint = %+v, want %+v", got, want)
	}

	gotSecret, err := s.GetEndpointSecret(ctx, acct.ID, e.ID)
	if err != nil {
		t.Fatal(err)
	}
	if gotSecret != secret {
		t.Fatalf("GetEndpointSecret = %q, want %q", gotSecret, secret)
	}

	// Each endpoint gets its own secret.
	_, secret2, err := s.CreateEndpoint(ctx, acct.ID, store.NewEndpoint{URL: "https://example.com/2"})
	if err != nil {
		t.Fatal(err)
	}
	if secret2 == secret {
		t.Fatal("two endpoints share a secret")
	}
}

func TestCreateEndpointWithoutEventTypes(t *testing.T) {
	ctx := context.Background()
	s := storetest.New(t)
	acct := mustAccount(t, s)

	e := mustEndpoint(t, s, acct.ID, store.NewEndpoint{URL: "https://example.com/hook"})
	if e.EventTypes == nil || len(e.EventTypes) != 0 {
		t.Fatalf("created EventTypes = %#v, want empty non-nil", e.EventTypes)
	}
	got, err := s.GetEndpoint(ctx, acct.ID, e.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.EventTypes == nil || len(got.EventTypes) != 0 {
		t.Fatalf("read EventTypes = %#v, want empty non-nil", got.EventTypes)
	}
}

func TestEndpointEventTypesStoredAsJSONArray(t *testing.T) {
	ctx := context.Background()
	cfg := sqliteConfig(t)
	s, err := store.Open(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	acct := mustAccount(t, s)
	all := mustEndpoint(t, s, acct.ID, store.NewEndpoint{URL: "https://example.com/all"})
	some := mustEndpoint(t, s, acct.ID, store.NewEndpoint{URL: "https://example.com/some", EventTypes: []string{"a.b", "c"}})
	s.Close()

	db, err := sql.Open("sqlite", cfg.Path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for id, want := range map[string]string{all.ID: `[]`, some.ID: `["a.b","c"]`} {
		var got string
		if err := db.QueryRow(`SELECT event_types FROM endpoints WHERE id = ?`, id).Scan(&got); err != nil {
			t.Fatal(err)
		}
		if got != want {
			t.Errorf("event_types of %s = %s, want %s", id, got, want)
		}
	}
}

func TestCreateEndpointUnknownAccount(t *testing.T) {
	s := storetest.New(t)
	_, _, err := s.CreateEndpoint(context.Background(), "acct_missing", store.NewEndpoint{URL: "https://example.com"})
	if !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}

func TestGetEndpointUnknown(t *testing.T) {
	ctx := context.Background()
	s := storetest.New(t)
	acct := mustAccount(t, s)
	if _, err := s.GetEndpoint(ctx, acct.ID, "ep_missing"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("GetEndpoint: err = %v, want ErrNotFound", err)
	}
	if _, err := s.GetEndpointSecret(ctx, acct.ID, "ep_missing"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("GetEndpointSecret: err = %v, want ErrNotFound", err)
	}
	if _, err := s.UpdateEndpoint(ctx, acct.ID, "ep_missing", store.EndpointUpdate{Enabled: ptr(false)}); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("UpdateEndpoint: err = %v, want ErrNotFound", err)
	}
	if err := s.DeleteEndpoint(ctx, acct.ID, "ep_missing"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("DeleteEndpoint: err = %v, want ErrNotFound", err)
	}
}

func TestListEndpointsPagination(t *testing.T) {
	ctx := context.Background()
	s := storetest.New(t)
	acct := mustAccount(t, s)
	other := mustAccount(t, s)

	var want []string
	for i := 0; i < 5; i++ {
		want = append(want, mustEndpoint(t, s, acct.ID, store.NewEndpoint{URL: "https://example.com/a"}).ID)
		mustEndpoint(t, s, other.ID, store.NewEndpoint{URL: "https://example.com/b"})
	}

	// Pages of 2, following the cursor, give every endpoint once, in order.
	var got []string
	after := ""
	for pages := 0; ; pages++ {
		if pages > 5 {
			t.Fatal("pagination does not end")
		}
		page, err := s.ListEndpoints(ctx, acct.ID, 2, after)
		if err != nil {
			t.Fatal(err)
		}
		if len(page) > 2 {
			t.Fatalf("page has %d endpoints, limit 2", len(page))
		}
		if len(page) == 0 {
			break
		}
		for _, e := range page {
			if e.AccountID != acct.ID {
				t.Fatalf("listed endpoint %s of account %s", e.ID, e.AccountID)
			}
			got = append(got, e.ID)
		}
		after = page[len(page)-1].ID
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("paged IDs = %v, want %v", got, want)
	}

	// A limit larger than the count returns everything.
	all, err := s.ListEndpoints(ctx, acct.ID, 100, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 5 {
		t.Fatalf("listed %d endpoints, want 5", len(all))
	}

	// The cursor of the last endpoint gives an empty, non-nil page.
	rest, err := s.ListEndpoints(ctx, acct.ID, 10, want[4])
	if err != nil {
		t.Fatal(err)
	}
	if rest == nil || len(rest) != 0 {
		t.Fatalf("page after last = %#v, want empty non-nil", rest)
	}

	// An account with no endpoints lists none.
	empty := mustAccount(t, s)
	none, err := s.ListEndpoints(ctx, empty.ID, 10, "")
	if err != nil || len(none) != 0 {
		t.Fatalf("empty account listed %v, err %v", none, err)
	}

	if _, err := s.ListEndpoints(ctx, acct.ID, 0, ""); err == nil {
		t.Fatal("limit 0 accepted")
	}
}

func TestUpdateEndpoint(t *testing.T) {
	ctx := context.Background()
	c := &clock{t: time.Date(2026, 10, 29, 12, 0, 0, 0, time.UTC)}
	s := storetest.New(t, store.WithClock(c.now))
	acct := mustAccount(t, s)
	e := mustEndpoint(t, s, acct.ID, store.NewEndpoint{
		URL: "https://example.com/old", Description: "old", EventTypes: []string{"a"},
	})
	secret, err := s.GetEndpointSecret(ctx, acct.ID, e.ID)
	if err != nil {
		t.Fatal(err)
	}

	// An update changes only the fields that are set.
	c.advance(time.Minute)
	got, err := s.UpdateEndpoint(ctx, acct.ID, e.ID, store.EndpointUpdate{Enabled: ptr(false)})
	if err != nil {
		t.Fatal(err)
	}
	want := e
	want.Enabled = false
	want.UpdatedAt = c.t
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("after disabling: %+v, want %+v", got, want)
	}

	c.advance(time.Minute)
	got, err = s.UpdateEndpoint(ctx, acct.ID, e.ID, store.EndpointUpdate{
		URL:         ptr("https://example.com/new"),
		Description: ptr(""),
		EventTypes:  ptr([]string{}),
		Enabled:     ptr(true),
	})
	if err != nil {
		t.Fatal(err)
	}
	want = store.Endpoint{
		ID: e.ID, AccountID: acct.ID, URL: "https://example.com/new", Description: "",
		EventTypes: []string{}, Enabled: true, CreatedAt: e.CreatedAt, UpdatedAt: c.t,
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("after full update: %+v, want %+v", got, want)
	}

	// The change is stored, and the secret is untouched.
	read, err := s.GetEndpoint(ctx, acct.ID, e.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(read, want) {
		t.Fatalf("read back %+v, want %+v", read, want)
	}
	if s2, _ := s.GetEndpointSecret(ctx, acct.ID, e.ID); s2 != secret {
		t.Fatalf("secret changed from %q to %q", secret, s2)
	}

	// Event types can be set again.
	got, err = s.UpdateEndpoint(ctx, acct.ID, e.ID, store.EndpointUpdate{EventTypes: ptr([]string{"x", "y"})})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got.EventTypes, []string{"x", "y"}) {
		t.Fatalf("EventTypes = %v", got.EventTypes)
	}
}

func TestDeletedEndpointIsGone(t *testing.T) {
	ctx := context.Background()
	s := storetest.New(t)
	acct := mustAccount(t, s)
	keep := mustEndpoint(t, s, acct.ID, store.NewEndpoint{URL: "https://example.com/keep"})
	gone := mustEndpoint(t, s, acct.ID, store.NewEndpoint{URL: "https://example.com/gone"})

	if err := s.DeleteEndpoint(ctx, acct.ID, gone.ID); err != nil {
		t.Fatal(err)
	}

	if _, err := s.GetEndpoint(ctx, acct.ID, gone.ID); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("GetEndpoint: err = %v, want ErrNotFound", err)
	}
	if _, err := s.GetEndpointSecret(ctx, acct.ID, gone.ID); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("GetEndpointSecret: err = %v, want ErrNotFound", err)
	}
	if _, err := s.UpdateEndpoint(ctx, acct.ID, gone.ID, store.EndpointUpdate{Enabled: ptr(true)}); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("UpdateEndpoint: err = %v, want ErrNotFound", err)
	}
	if err := s.DeleteEndpoint(ctx, acct.ID, gone.ID); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("second DeleteEndpoint: err = %v, want ErrNotFound", err)
	}

	list, err := s.ListEndpoints(ctx, acct.ID, 10, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 || list[0].ID != keep.ID {
		t.Fatalf("list after delete = %v, want only %s", list, keep.ID)
	}
	// A deleted endpoint is skipped by the cursor too.
	list, err = s.ListEndpoints(ctx, acct.ID, 1, keep.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 0 {
		t.Fatalf("page after %s = %v, want empty", keep.ID, list)
	}
}

func TestEndpointsOfAnotherAccountAreNotFound(t *testing.T) {
	ctx := context.Background()
	s := storetest.New(t)
	owner := mustAccount(t, s)
	intruder := mustAccount(t, s)
	e := mustEndpoint(t, s, owner.ID, store.NewEndpoint{URL: "https://example.com/hook", EventTypes: []string{"a"}})

	// Reads.
	if _, err := s.GetEndpoint(ctx, intruder.ID, e.ID); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("GetEndpoint: err = %v, want ErrNotFound", err)
	}
	if _, err := s.GetEndpointSecret(ctx, intruder.ID, e.ID); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("GetEndpointSecret: err = %v, want ErrNotFound", err)
	}
	if list, err := s.ListEndpoints(ctx, intruder.ID, 10, ""); err != nil || len(list) != 0 {
		t.Errorf("ListEndpoints = %v, err %v, want none", list, err)
	}

	// Writes.
	if _, err := s.UpdateEndpoint(ctx, intruder.ID, e.ID, store.EndpointUpdate{
		URL: ptr("https://evil.example/"), Enabled: ptr(false),
	}); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("UpdateEndpoint: err = %v, want ErrNotFound", err)
	}
	if err := s.DeleteEndpoint(ctx, intruder.ID, e.ID); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("DeleteEndpoint: err = %v, want ErrNotFound", err)
	}

	// The owner's endpoint is unchanged.
	got, err := s.GetEndpoint(ctx, owner.ID, e.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, e) {
		t.Fatalf("owner's endpoint = %+v, want unchanged %+v", got, e)
	}
}
