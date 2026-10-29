package store_test

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"hookd/internal/ids"
	"hookd/internal/store"
	"hookd/internal/store/storetest"
)

func mustEvent(t *testing.T, s store.Store, accountID string, in store.NewEvent) (store.Event, bool) {
	t.Helper()
	e, created, err := s.CreateEvent(context.Background(), accountID, in)
	if err != nil {
		t.Fatal(err)
	}
	return e, created
}

func deliveryEndpoints(e store.Event) []string {
	eps := []string{}
	for _, d := range e.Deliveries {
		eps = append(eps, d.EndpointID)
	}
	return eps
}

func TestCreateAndGetEvent(t *testing.T) {
	ctx := context.Background()
	c := &clock{t: time.Date(2026, 10, 29, 12, 0, 0, 987654321, time.UTC)}
	s := storetest.New(t, store.WithClock(c.now))
	acct := mustAccount(t, s)
	ep := mustEndpoint(t, s, acct.ID, store.NewEndpoint{URL: "https://example.com/hook"})

	payload := []byte(`{"order_id": "o_1",  "total": 12.5}`)
	e, created := mustEvent(t, s, acct.ID, store.NewEvent{EventType: "order.paid", Payload: payload})
	if !created {
		t.Fatal("CreateEvent reported an existing event")
	}
	if !strings.HasPrefix(e.ID, ids.PrefixMessage) {
		t.Errorf("ID %q lacks prefix %q", e.ID, ids.PrefixMessage)
	}
	want := store.Event{
		ID:         e.ID,
		AccountID:  acct.ID,
		EventType:  "order.paid",
		Payload:    payload,
		CreatedAt:  c.t.Truncate(time.Microsecond),
		Deliveries: []store.DeliveryStatus{{EndpointID: ep.ID, Status: store.DeliveryPending}},
	}
	if !reflect.DeepEqual(e, want) {
		t.Fatalf("CreateEvent = %+v, want %+v", e, want)
	}

	got, err := s.GetEvent(ctx, acct.ID, e.ID)
	if err != nil {
		t.Fatal(err)
	}
	// The payload comes back byte for byte, spacing included.
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("GetEvent = %+v, want %+v", got, want)
	}
}

func TestCreateEventUnknownAccount(t *testing.T) {
	s := storetest.New(t)
	_, _, err := s.CreateEvent(context.Background(), "acct_missing", store.NewEvent{EventType: "a", Payload: []byte(`{}`)})
	if !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("CreateEvent for unknown account: err = %v, want ErrNotFound", err)
	}
}

func TestEventFanOut(t *testing.T) {
	ctx := context.Background()
	s := storetest.New(t)
	acct := mustAccount(t, s)
	other := mustAccount(t, s)

	all := mustEndpoint(t, s, acct.ID, store.NewEndpoint{URL: "https://example.com/all"})
	paid := mustEndpoint(t, s, acct.ID, store.NewEndpoint{URL: "https://example.com/paid", EventTypes: []string{"user.created", "order.paid"}})
	mustEndpoint(t, s, acct.ID, store.NewEndpoint{URL: "https://example.com/users", EventTypes: []string{"user.created"}})
	disabled := mustEndpoint(t, s, acct.ID, store.NewEndpoint{URL: "https://example.com/disabled"})
	if _, err := s.UpdateEndpoint(ctx, acct.ID, disabled.ID, store.EndpointUpdate{Enabled: ptr(false)}); err != nil {
		t.Fatal(err)
	}
	deleted := mustEndpoint(t, s, acct.ID, store.NewEndpoint{URL: "https://example.com/deleted", EventTypes: []string{"order.paid"}})
	if err := s.DeleteEndpoint(ctx, acct.ID, deleted.ID); err != nil {
		t.Fatal(err)
	}
	mustEndpoint(t, s, other.ID, store.NewEndpoint{URL: "https://example.com/other"})

	e, _ := mustEvent(t, s, acct.ID, store.NewEvent{EventType: "order.paid", Payload: []byte(`{}`)})
	want := []string{all.ID, paid.ID}
	if got := deliveryEndpoints(e); !reflect.DeepEqual(got, want) {
		t.Fatalf("CreateEvent delivered to %v, want %v", got, want)
	}
	got, err := s.GetEvent(ctx, acct.ID, e.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(deliveryEndpoints(got), want) {
		t.Fatalf("GetEvent deliveries go to %v, want %v", deliveryEndpoints(got), want)
	}
	for _, d := range got.Deliveries {
		if d.Status != store.DeliveryPending || d.ResponseStatus != 0 || d.Error != "" {
			t.Errorf("new delivery = %+v, want pending with no status or error", d)
		}
	}
}

func TestEventWithNoMatchingEndpointHasNoDeliveries(t *testing.T) {
	ctx := context.Background()
	s := storetest.New(t)
	acct := mustAccount(t, s)
	mustEndpoint(t, s, acct.ID, store.NewEndpoint{URL: "https://example.com/users", EventTypes: []string{"user.created"}})

	e, created := mustEvent(t, s, acct.ID, store.NewEvent{EventType: "order.paid", Payload: []byte(`{}`)})
	if !created || e.Deliveries == nil || len(e.Deliveries) != 0 {
		t.Fatalf("CreateEvent = %+v (created %v), want a new event with no deliveries", e, created)
	}
	got, err := s.GetEvent(ctx, acct.ID, e.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Deliveries == nil || len(got.Deliveries) != 0 {
		t.Fatalf("GetEvent deliveries = %#v, want an empty list", got.Deliveries)
	}
}

func TestIdempotencyKeyWithinWindow(t *testing.T) {
	ctx := context.Background()
	c := &clock{t: time.Date(2026, 10, 29, 12, 0, 0, 0, time.UTC)}
	s := storetest.New(t, store.WithClock(c.now), store.WithIdempotencyWindow(time.Hour))
	acct := mustAccount(t, s)
	ep := mustEndpoint(t, s, acct.ID, store.NewEndpoint{URL: "https://example.com/hook"})

	in := store.NewEvent{EventType: "order.paid", Payload: []byte(`{"n":1}`), IdempotencyKey: "k1"}
	first, created := mustEvent(t, s, acct.ID, in)
	if !created || first.IdempotencyKey != "k1" {
		t.Fatalf("first CreateEvent = %+v (created %v), want a new event with key k1", first, created)
	}

	// An endpoint added later must not get a delivery from the repeat.
	mustEndpoint(t, s, acct.ID, store.NewEndpoint{URL: "https://example.com/late"})
	c.advance(time.Hour - time.Microsecond)
	repeat, created := mustEvent(t, s, acct.ID, store.NewEvent{EventType: "user.created", Payload: []byte(`{"n":2}`), IdempotencyKey: "k1"})
	if created {
		t.Fatal("repeat within the window reported a new event")
	}
	if !reflect.DeepEqual(repeat, first) {
		t.Fatalf("repeat = %+v, want the first event %+v", repeat, first)
	}
	got, err := s.GetEvent(ctx, acct.ID, first.ID)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{ep.ID}; !reflect.DeepEqual(deliveryEndpoints(got), want) {
		t.Fatalf("deliveries after repeat go to %v, want %v", deliveryEndpoints(got), want)
	}

	// The same key in another account is a different event.
	other := mustAccount(t, s)
	o, created := mustEvent(t, s, other.ID, in)
	if !created || o.ID == first.ID {
		t.Fatalf("same key in another account: %+v (created %v), want a new event", o, created)
	}
}

func TestIdempotencyKeyAfterWindow(t *testing.T) {
	ctx := context.Background()
	c := &clock{t: time.Date(2026, 10, 29, 12, 0, 0, 0, time.UTC)}
	s := storetest.New(t, store.WithClock(c.now), store.WithIdempotencyWindow(time.Hour))
	acct := mustAccount(t, s)
	mustEndpoint(t, s, acct.ID, store.NewEndpoint{URL: "https://example.com/hook"})

	in := store.NewEvent{EventType: "order.paid", Payload: []byte(`{}`), IdempotencyKey: "k1"}
	first, _ := mustEvent(t, s, acct.ID, in)

	c.advance(time.Hour)
	second, created := mustEvent(t, s, acct.ID, in)
	if !created || second.ID == first.ID {
		t.Fatalf("repeat after the window = %+v (created %v), want a new event", second, created)
	}
	if second.IdempotencyKey != "k1" || len(second.Deliveries) != 1 {
		t.Fatalf("new event = %+v, want key k1 and one delivery", second)
	}

	// The old event lost its key; the new one holds it now.
	old, err := s.GetEvent(ctx, acct.ID, first.ID)
	if err != nil {
		t.Fatal(err)
	}
	if old.IdempotencyKey != "" {
		t.Errorf("old event key = %q, want it cleared", old.IdempotencyKey)
	}
	c.advance(time.Minute)
	third, created := mustEvent(t, s, acct.ID, in)
	if created || third.ID != second.ID {
		t.Fatalf("repeat within the new window = %s (created %v), want %s", third.ID, created, second.ID)
	}
}

func TestEventsOfAnotherAccountAreNotFound(t *testing.T) {
	ctx := context.Background()
	s := storetest.New(t)
	acct := mustAccount(t, s)
	other := mustAccount(t, s)
	e, _ := mustEvent(t, s, acct.ID, store.NewEvent{EventType: "order.paid", Payload: []byte(`{}`)})

	if _, err := s.GetEvent(ctx, other.ID, e.ID); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("GetEvent from another account: err = %v, want ErrNotFound", err)
	}
	if _, err := s.GetEvent(ctx, acct.ID, "msg_missing"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("GetEvent of unknown ID: err = %v, want ErrNotFound", err)
	}
}
