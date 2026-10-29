package store_test

import (
	"context"
	"errors"
	"reflect"
	"slices"
	"testing"
	"time"

	"hookd/internal/store"
	"hookd/internal/store/storetest"
)

func TestGetAccount(t *testing.T) {
	storetest.Run(t, func(t *testing.T, db *storetest.DB) {
		ctx := context.Background()
		now := time.Date(2026, 10, 29, 12, 0, 0, 123456789, time.UTC)
		s := db.New(t, store.WithClock(func() time.Time { return now }))

		acct, err := s.CreateAccount(ctx, "acme")
		if err != nil {
			t.Fatal(err)
		}
		got, err := s.GetAccount(ctx, acct.ID)
		if err != nil {
			t.Fatal(err)
		}
		want := store.Account{ID: acct.ID, Name: "acme", CreatedAt: now.Truncate(time.Microsecond)}
		if got != want {
			t.Fatalf("GetAccount = %+v, want %+v", got, want)
		}
		if _, err := s.GetAccount(ctx, "acct_missing"); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("unknown account: err = %v, want ErrNotFound", err)
		}
	})
}

func TestListAccounts(t *testing.T) {
	storetest.Run(t, func(t *testing.T, db *storetest.DB) {
		ctx := context.Background()
		now := time.Date(2026, 10, 29, 12, 0, 0, 123456789, time.UTC)
		s := db.New(t, store.WithClock(func() time.Time { return now }))

		page, err := s.ListAccounts(ctx, 10, "")
		if err != nil || page == nil || len(page) != 0 {
			t.Fatalf("no accounts: ListAccounts = %#v, %v; want empty slice", page, err)
		}

		// Account i gets i endpoints, one of them deleted when i > 1, and
		// 2*i API keys.
		var want []store.AccountSummary
		for i := range 5 {
			a, err := s.CreateAccount(ctx, "acct")
			if err != nil {
				t.Fatal(err)
			}
			eps := 0
			for j := range i {
				ep := mustEndpoint(t, s, a.ID, store.NewEndpoint{URL: "https://example.com/hook"})
				if i > 1 && j == 0 {
					if err := s.DeleteEndpoint(ctx, a.ID, ep.ID); err != nil {
						t.Fatal(err)
					}
					continue
				}
				eps++
			}
			for range 2 * i {
				if _, _, err := s.CreateAPIKey(ctx, a.ID); err != nil {
					t.Fatal(err)
				}
			}
			want = append(want, store.AccountSummary{Account: a, Endpoints: eps, APIKeys: 2 * i})
		}

		// Pages of 2, following the cursor, give every account once, in order.
		var got []store.AccountSummary
		after := ""
		for pages := 0; ; pages++ {
			if pages > 5 {
				t.Fatal("pagination does not end")
			}
			page, err := s.ListAccounts(ctx, 2, after)
			if err != nil {
				t.Fatal(err)
			}
			if len(page) > 2 {
				t.Fatalf("page has %d accounts, limit 2", len(page))
			}
			if len(page) == 0 {
				break
			}
			got = append(got, page...)
			after = page[len(page)-1].ID
		}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("paged accounts = %+v, want %+v", got, want)
		}

		if _, err := s.ListAccounts(ctx, 0, ""); err == nil {
			t.Fatal("ListAccounts with limit 0 succeeded")
		}
	})
}

// eventIDs returns the IDs of the listed events, in order.
func eventIDs(es []store.EventSummary) []string {
	ids := []string{}
	for _, e := range es {
		ids = append(ids, e.ID)
	}
	return ids
}

// counts builds a Deliveries map of ListEvents from the non-zero counts.
func counts(nonZero map[string]int) map[string]int {
	m := map[string]int{
		store.DeliveryPending:    0,
		store.DeliveryInProgress: 0,
		store.DeliverySucceeded:  0,
		store.DeliveryFailed:     0,
	}
	for k, v := range nonZero {
		m[k] = v
	}
	return m
}

func TestListEvents(t *testing.T) {
	storetest.Run(t, func(t *testing.T, db *storetest.DB) {
		ctx := context.Background()
		c := &clock{t: time.Date(2026, 10, 29, 12, 0, 0, 987654321, time.UTC)}
		s := db.New(t, store.WithClock(c.now))
		acct := mustAccount(t, s)
		other := mustAccount(t, s)

		// No endpoint yet, so no deliveries.
		e0, _ := mustEvent(t, s, acct.ID, store.NewEvent{EventType: "order.paid", Payload: []byte(`{"n":0}`), IdempotencyKey: "k0"})
		c.advance(time.Second)

		mustEndpoint(t, s, acct.ID, store.NewEndpoint{URL: "https://example.com/all"})
		mustEndpoint(t, s, acct.ID, store.NewEndpoint{URL: "https://example.com/paid1", EventTypes: []string{"order.paid"}})
		mustEndpoint(t, s, acct.ID, store.NewEndpoint{URL: "https://example.com/paid2", EventTypes: []string{"order.paid"}})
		mustEndpoint(t, s, other.ID, store.NewEndpoint{URL: "https://example.com/other"})

		// Three deliveries, taken to three different states.
		e1, _ := mustEvent(t, s, acct.ID, store.NewEvent{EventType: "order.paid", Payload: []byte(`{"n":1}`)})
		c.advance(time.Second)
		for _, status := range []string{store.DeliverySucceeded, store.DeliveryFailed, ""} {
			d := mustClaim(t, s, time.Minute)
			if status == "" {
				continue // stays in progress
			}
			if err := s.RecordDeliveryResult(ctx, d.ID, store.DeliveryResult{Status: status, ResponseStatus: 200}); err != nil {
				t.Fatal(err)
			}
		}

		e2, _ := mustEvent(t, s, acct.ID, store.NewEvent{EventType: "user.created", Payload: []byte(`{"n":2}`)})
		c.advance(time.Second)
		otherEvent, _ := mustEvent(t, s, other.ID, store.NewEvent{EventType: "order.paid", Payload: []byte(`{}`)})

		got, err := s.ListEvents(ctx, acct.ID, store.EventFilter{Limit: 10})
		if err != nil {
			t.Fatal(err)
		}
		want := []store.EventSummary{
			{ID: e2.ID, AccountID: acct.ID, EventType: "user.created", CreatedAt: e2.CreatedAt,
				Deliveries: counts(map[string]int{store.DeliveryPending: 1})},
			{ID: e1.ID, AccountID: acct.ID, EventType: "order.paid", CreatedAt: e1.CreatedAt,
				Deliveries: counts(map[string]int{store.DeliverySucceeded: 1, store.DeliveryFailed: 1, store.DeliveryInProgress: 1})},
			{ID: e0.ID, AccountID: acct.ID, EventType: "order.paid", IdempotencyKey: "k0", CreatedAt: e0.CreatedAt,
				Deliveries: counts(nil)},
		}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("ListEvents = %+v, want %+v", got, want)
		}

		// The type filter is exact and keeps to the account.
		for _, tc := range []struct {
			filter store.EventFilter
			want   []string
		}{
			{store.EventFilter{EventType: "order.paid", Limit: 10}, []string{e1.ID, e0.ID}},
			{store.EventFilter{EventType: "user.created", Limit: 10}, []string{e2.ID}},
			{store.EventFilter{EventType: "order", Limit: 10}, []string{}},
			{store.EventFilter{EventType: "order.paid", Before: e1.ID, Limit: 10}, []string{e0.ID}},
			{store.EventFilter{EventType: "user.created", Before: e2.ID, Limit: 10}, []string{}},
		} {
			got, err := s.ListEvents(ctx, acct.ID, tc.filter)
			if err != nil {
				t.Fatal(err)
			}
			if ids := eventIDs(got); !slices.Equal(ids, tc.want) {
				t.Errorf("ListEvents(%+v) = %v, want %v", tc.filter, ids, tc.want)
			}
		}

		got, err = s.ListEvents(ctx, other.ID, store.EventFilter{Limit: 10})
		if err != nil {
			t.Fatal(err)
		}
		if ids := eventIDs(got); !slices.Equal(ids, []string{otherEvent.ID}) {
			t.Errorf("other account's events = %v, want only %s", ids, otherEvent.ID)
		}
		got, err = s.ListEvents(ctx, "acct_missing", store.EventFilter{Limit: 10})
		if err != nil || got == nil || len(got) != 0 {
			t.Fatalf("unknown account: ListEvents = %#v, %v; want empty slice", got, err)
		}
		if _, err := s.ListEvents(ctx, acct.ID, store.EventFilter{}); err == nil {
			t.Fatal("ListEvents with limit 0 succeeded")
		}
	})
}

func TestListEventsPagination(t *testing.T) {
	storetest.Run(t, func(t *testing.T, db *storetest.DB) {
		ctx := context.Background()
		s := db.New(t)
		acct := mustAccount(t, s)
		other := mustAccount(t, s)
		mustEndpoint(t, s, acct.ID, store.NewEndpoint{URL: "https://example.com/a"})
		mustEndpoint(t, s, other.ID, store.NewEndpoint{URL: "https://example.com/b"})

		var all, paid []string
		for i := range 7 {
			typ := "order.paid"
			if i%3 == 0 {
				typ = "user.created"
			}
			e, _ := mustEvent(t, s, acct.ID, store.NewEvent{EventType: typ, Payload: []byte(`{}`)})
			all = append(all, e.ID)
			if typ == "order.paid" {
				paid = append(paid, e.ID)
			}
			// Another account's events sit between this account's, and must
			// never show up in its pages.
			mustEvent(t, s, other.ID, store.NewEvent{EventType: typ, Payload: []byte(`{}`)})
		}
		slices.Reverse(all)
		slices.Reverse(paid)

		for _, tc := range []struct {
			eventType string
			want      []string
		}{
			{"", all},
			{"order.paid", paid},
		} {
			// Pages of 2, following the cursor, give every event once, newest
			// first.
			var got []string
			before := ""
			for pages := 0; ; pages++ {
				if pages > len(tc.want) {
					t.Fatal("pagination does not end")
				}
				page, err := s.ListEvents(ctx, acct.ID, store.EventFilter{EventType: tc.eventType, Before: before, Limit: 2})
				if err != nil {
					t.Fatal(err)
				}
				if len(page) > 2 {
					t.Fatalf("page has %d events, limit 2", len(page))
				}
				if len(page) == 0 {
					break
				}
				for _, e := range page {
					if e.AccountID != acct.ID {
						t.Fatalf("listed event %s of account %s", e.ID, e.AccountID)
					}
					if e.Deliveries[store.DeliveryPending] != 1 {
						t.Errorf("event %s deliveries = %v, want 1 pending", e.ID, e.Deliveries)
					}
				}
				got = append(got, eventIDs(page)...)
				before = page[len(page)-1].ID
			}
			if !slices.Equal(got, tc.want) {
				t.Errorf("type %q: paged IDs = %v, want %v", tc.eventType, got, tc.want)
			}
		}
	})
}
