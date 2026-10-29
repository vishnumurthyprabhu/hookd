package store_test

import (
	"context"
	"errors"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"hookd/internal/store"
	"hookd/internal/store/storetest"
)

// attemptFixture is an account with two endpoints and two events, each
// delivered to both. Every delivery to ep1 fails once and succeeds on its
// retry; every delivery to ep2 succeeds at once. Another account has one
// event with one attempt.
type attemptFixture struct {
	s          store.Store
	acct       store.Account
	ep1, ep2   store.Endpoint
	e1, e2     store.Event
	attempts   []store.Attempt // all of acct's, sorted by ID
	other      store.Account
	otherEP    store.Endpoint
	otherEvent store.Event
	otherAtt   store.Attempt
}

func newAttemptFixture(t *testing.T, db *storetest.DB) attemptFixture {
	t.Helper()
	c := &clock{t: time.Date(2026, 10, 29, 12, 0, 0, 0, time.UTC)}
	f := attemptFixture{s: db.New(t, store.WithClock(c.now))}
	s := f.s
	f.acct = mustAccount(t, s)
	f.ep1 = mustEndpoint(t, s, f.acct.ID, store.NewEndpoint{URL: "https://example.com/1"})
	f.ep2 = mustEndpoint(t, s, f.acct.ID, store.NewEndpoint{URL: "https://example.com/2"})
	f.e1, _ = mustEvent(t, s, f.acct.ID, store.NewEvent{EventType: "a", Payload: []byte(`{}`)})
	f.e2, _ = mustEvent(t, s, f.acct.ID, store.NewEvent{EventType: "a", Payload: []byte(`{}`)})

	retry := c.t.Add(time.Minute)
	for range 4 {
		d := mustClaim(t, s, time.Hour)
		r := store.AttemptResult{Number: 1, Outcome: store.AttemptSucceeded, ResponseStatus: 200, Latency: 5 * time.Millisecond}
		if d.EndpointID == f.ep1.ID {
			r = store.AttemptResult{Number: 1, Outcome: store.AttemptFailed, ResponseStatus: 500, Error: "non-2xx response: 500 Internal Server Error", NextAttemptAt: &retry}
		}
		f.attempts = append(f.attempts, mustRecordAttempt(t, s, d.ID, r))
	}
	mustClaimNone(t, s, time.Hour)
	c.advance(time.Minute)
	for range 2 {
		d := mustClaim(t, s, time.Hour)
		f.attempts = append(f.attempts, mustRecordAttempt(t, s, d.ID, store.AttemptResult{
			Number: 2, Outcome: store.AttemptSucceeded, ResponseStatus: 204, Latency: 7 * time.Millisecond,
		}))
	}
	mustClaimNone(t, s, time.Hour)
	slices.SortFunc(f.attempts, func(a, b store.Attempt) int { return strings.Compare(a.ID, b.ID) })

	f.other = mustAccount(t, s)
	f.otherEP = mustEndpoint(t, s, f.other.ID, store.NewEndpoint{URL: "https://example.com/other"})
	f.otherEvent, _ = mustEvent(t, s, f.other.ID, store.NewEvent{EventType: "a", Payload: []byte(`{}`)})
	d := mustClaim(t, s, time.Hour)
	f.otherAtt = mustRecordAttempt(t, s, d.ID, store.AttemptResult{Number: 1, Outcome: store.AttemptFailed, Error: "timeout"})
	return f
}

// where returns acct's attempts, oldest first, that keep returns true for.
func (f attemptFixture) where(keep func(store.Attempt) bool) []store.Attempt {
	out := []store.Attempt{}
	for _, a := range f.attempts {
		if keep(a) {
			out = append(out, a)
		}
	}
	return out
}

func reversed(as []store.Attempt) []store.Attempt {
	out := slices.Clone(as)
	slices.Reverse(out)
	return out
}

func attemptIDs(as []store.Attempt) []string {
	out := make([]string, len(as))
	for i, a := range as {
		out[i] = a.ID
	}
	return out
}

func TestListEventAttempts(t *testing.T) {
	storetest.Run(t, func(t *testing.T, db *storetest.DB) {
		ctx := context.Background()
		f := newAttemptFixture(t, db)

		got, err := f.s.ListEventAttempts(ctx, f.acct.ID, f.e1.ID, store.AttemptFilter{Limit: 100})
		if err != nil {
			t.Fatal(err)
		}
		want := f.where(func(a store.Attempt) bool { return a.EventID == f.e1.ID })
		if len(want) != 3 {
			t.Fatalf("fixture: event has %d attempts, want 3", len(want))
		}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("event attempts =\n%+v\nwant\n%+v", got, want)
		}

		got, err = f.s.ListEventAttempts(ctx, f.acct.ID, f.e1.ID, store.AttemptFilter{EndpointID: f.ep1.ID, Limit: 100})
		if err != nil {
			t.Fatal(err)
		}
		want = f.where(func(a store.Attempt) bool { return a.EventID == f.e1.ID && a.EndpointID == f.ep1.ID })
		if !reflect.DeepEqual(got, want) || len(want) != 2 || got[0].Number != 1 || got[1].Number != 2 {
			t.Fatalf("event attempts to ep1 = %+v, want %+v", got, want)
		}

		// The fields the event list does not use change nothing.
		got, err = f.s.ListEventAttempts(ctx, f.acct.ID, f.e1.ID, store.AttemptFilter{Outcome: store.AttemptFailed, Before: want[0].ID, Limit: 100})
		if err != nil || len(got) != 3 {
			t.Errorf("with Outcome and Before set: %d attempts, %v, want all 3", len(got), err)
		}

		// A filter to an endpoint with no attempts on the event, or an event
		// with no attempts yet, gives an empty list.
		got, err = f.s.ListEventAttempts(ctx, f.acct.ID, f.e1.ID, store.AttemptFilter{EndpointID: f.otherEP.ID, Limit: 100})
		if err != nil || got == nil || len(got) != 0 {
			t.Errorf("filtered to another account's endpoint = %v, %v, want an empty slice", got, err)
		}
		fresh, _ := mustEvent(t, f.s, f.acct.ID, store.NewEvent{EventType: "a", Payload: []byte(`{}`)})
		got, err = f.s.ListEventAttempts(ctx, f.acct.ID, fresh.ID, store.AttemptFilter{Limit: 100})
		if err != nil || got == nil || len(got) != 0 {
			t.Errorf("event without attempts = %v, %v, want an empty slice", got, err)
		}
	})
}

func TestListEventAttemptsPaging(t *testing.T) {
	storetest.Run(t, func(t *testing.T, db *storetest.DB) {
		ctx := context.Background()
		f := newAttemptFixture(t, db)
		want := attemptIDs(f.where(func(a store.Attempt) bool { return a.EventID == f.e2.ID }))

		for _, limit := range []int{1, 2, 3, 4} {
			var seen []string
			after := ""
			for range len(want) + 1 {
				page, err := f.s.ListEventAttempts(ctx, f.acct.ID, f.e2.ID, store.AttemptFilter{After: after, Limit: limit})
				if err != nil {
					t.Fatal(err)
				}
				if len(page) > limit {
					t.Fatalf("limit %d: page of %d", limit, len(page))
				}
				if len(page) == 0 {
					break
				}
				seen = append(seen, attemptIDs(page)...)
				after = page[len(page)-1].ID
			}
			if !slices.Equal(seen, want) {
				t.Errorf("limit %d: paged through %v, want %v", limit, seen, want)
			}
		}

		// The cursor combines with the endpoint filter.
		ep1 := attemptIDs(f.where(func(a store.Attempt) bool { return a.EventID == f.e2.ID && a.EndpointID == f.ep1.ID }))
		page, err := f.s.ListEventAttempts(ctx, f.acct.ID, f.e2.ID, store.AttemptFilter{EndpointID: f.ep1.ID, After: ep1[0], Limit: 10})
		if err != nil {
			t.Fatal(err)
		}
		if got := attemptIDs(page); !slices.Equal(got, ep1[1:]) {
			t.Errorf("ep1 attempts after %s = %v, want %v", ep1[0], got, ep1[1:])
		}
	})
}

func TestListEndpointAttempts(t *testing.T) {
	storetest.Run(t, func(t *testing.T, db *storetest.DB) {
		ctx := context.Background()
		f := newAttemptFixture(t, db)
		ofEP1 := func(outcome string) []store.Attempt {
			return reversed(f.where(func(a store.Attempt) bool {
				return a.EndpointID == f.ep1.ID && (outcome == "" || a.Outcome == outcome)
			}))
		}

		for _, outcome := range []string{"", store.AttemptSucceeded, store.AttemptFailed} {
			got, err := f.s.ListEndpointAttempts(ctx, f.acct.ID, f.ep1.ID, store.AttemptFilter{Outcome: outcome, Limit: 100})
			if err != nil {
				t.Fatal(err)
			}
			want := ofEP1(outcome)
			if len(want) == 0 {
				t.Fatalf("fixture: no ep1 attempts with outcome %q", outcome)
			}
			if !reflect.DeepEqual(got, want) {
				t.Errorf("ep1 attempts with outcome %q =\n%+v\nwant\n%+v", outcome, got, want)
			}
		}
		if got, err := f.s.ListEndpointAttempts(ctx, f.acct.ID, f.ep2.ID, store.AttemptFilter{Outcome: store.AttemptFailed, Limit: 100}); err != nil || got == nil || len(got) != 0 {
			t.Errorf("failed ep2 attempts = %v, %v, want an empty slice", got, err)
		}

		// The fields the endpoint list does not use change nothing.
		all := ofEP1("")
		got, err := f.s.ListEndpointAttempts(ctx, f.acct.ID, f.ep1.ID, store.AttemptFilter{EndpointID: f.ep2.ID, After: all[0].ID, Limit: 100})
		if err != nil || !reflect.DeepEqual(got, all) {
			t.Errorf("with EndpointID and After set = %v, %v, want all %d", got, err, len(all))
		}

		// A deleted endpoint's attempts are still listed.
		if err := f.s.DeleteEndpoint(ctx, f.acct.ID, f.ep2.ID); err != nil {
			t.Fatal(err)
		}
		got, err = f.s.ListEndpointAttempts(ctx, f.acct.ID, f.ep2.ID, store.AttemptFilter{Limit: 100})
		if err != nil {
			t.Fatal(err)
		}
		if want := reversed(f.where(func(a store.Attempt) bool { return a.EndpointID == f.ep2.ID })); len(want) != 2 || !reflect.DeepEqual(got, want) {
			t.Errorf("deleted endpoint's attempts = %+v, want %+v", got, want)
		}
	})
}

func TestListEndpointAttemptsPaging(t *testing.T) {
	storetest.Run(t, func(t *testing.T, db *storetest.DB) {
		ctx := context.Background()
		f := newAttemptFixture(t, db)

		for _, outcome := range []string{"", store.AttemptSucceeded} {
			want := attemptIDs(reversed(f.where(func(a store.Attempt) bool {
				return a.EndpointID == f.ep1.ID && (outcome == "" || a.Outcome == outcome)
			})))
			for _, limit := range []int{1, 3, 5} {
				var seen []string
				before := ""
				for range len(want) + 1 {
					page, err := f.s.ListEndpointAttempts(ctx, f.acct.ID, f.ep1.ID, store.AttemptFilter{Outcome: outcome, Before: before, Limit: limit})
					if err != nil {
						t.Fatal(err)
					}
					if len(page) > limit {
						t.Fatalf("limit %d: page of %d", limit, len(page))
					}
					if len(page) == 0 {
						break
					}
					seen = append(seen, attemptIDs(page)...)
					before = page[len(page)-1].ID
				}
				if !slices.Equal(seen, want) {
					t.Errorf("outcome %q, limit %d: paged through %v, want %v", outcome, limit, seen, want)
				}
			}
		}
	})
}

func TestGetAttempt(t *testing.T) {
	storetest.Run(t, func(t *testing.T, db *storetest.DB) {
		ctx := context.Background()
		f := newAttemptFixture(t, db)
		for _, want := range f.attempts {
			got, err := f.s.GetAttempt(ctx, f.acct.ID, want.ID)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got, want) {
				t.Errorf("GetAttempt(%s) = %+v, want %+v", want.ID, got, want)
			}
		}
		got, err := f.s.GetAttempt(ctx, f.other.ID, f.otherAtt.ID)
		if err != nil || !reflect.DeepEqual(got, f.otherAtt) {
			t.Errorf("other account's own attempt = %+v, %v, want %+v", got, err, f.otherAtt)
		}
	})
}

// TestAttemptsScopedByAccount checks that another account's event, endpoint
// and attempt, and unknown IDs, answer ErrNotFound.
func TestAttemptsScopedByAccount(t *testing.T) {
	storetest.Run(t, func(t *testing.T, db *storetest.DB) {
		ctx := context.Background()
		f := newAttemptFixture(t, db)
		all := store.AttemptFilter{Limit: 100}
		check := func(name string, err error) {
			t.Helper()
			if !errors.Is(err, store.ErrNotFound) {
				t.Errorf("%s: err = %v, want ErrNotFound", name, err)
			}
		}

		_, err := f.s.ListEventAttempts(ctx, f.acct.ID, f.otherEvent.ID, all)
		check("another account's event", err)
		_, err = f.s.ListEventAttempts(ctx, f.other.ID, f.e1.ID, all)
		check("event seen from another account", err)
		_, err = f.s.ListEventAttempts(ctx, f.acct.ID, "msg_missing", all)
		check("unknown event", err)

		_, err = f.s.ListEndpointAttempts(ctx, f.acct.ID, f.otherEP.ID, all)
		check("another account's endpoint", err)
		_, err = f.s.ListEndpointAttempts(ctx, f.other.ID, f.ep1.ID, all)
		check("endpoint seen from another account", err)
		if err := f.s.DeleteEndpoint(ctx, f.acct.ID, f.ep1.ID); err != nil {
			t.Fatal(err)
		}
		_, err = f.s.ListEndpointAttempts(ctx, f.other.ID, f.ep1.ID, all)
		check("deleted endpoint seen from another account", err)
		_, err = f.s.ListEndpointAttempts(ctx, f.acct.ID, "ep_missing", all)
		check("unknown endpoint", err)

		_, err = f.s.GetAttempt(ctx, f.acct.ID, f.otherAtt.ID)
		check("another account's attempt", err)
		_, err = f.s.GetAttempt(ctx, f.other.ID, f.attempts[0].ID)
		check("attempt seen from another account", err)
		_, err = f.s.GetAttempt(ctx, f.acct.ID, "att_missing")
		check("unknown attempt", err)
	})
}

func TestAttemptListsRejectBadFilters(t *testing.T) {
	storetest.Run(t, func(t *testing.T, db *storetest.DB) {
		ctx := context.Background()
		f := newAttemptFixture(t, db)
		for _, limit := range []int{0, -1} {
			if _, err := f.s.ListEventAttempts(ctx, f.acct.ID, f.e1.ID, store.AttemptFilter{Limit: limit}); err == nil || errors.Is(err, store.ErrNotFound) {
				t.Errorf("event list, limit %d: err = %v, want a validation error", limit, err)
			}
			if _, err := f.s.ListEndpointAttempts(ctx, f.acct.ID, f.ep1.ID, store.AttemptFilter{Limit: limit}); err == nil || errors.Is(err, store.ErrNotFound) {
				t.Errorf("endpoint list, limit %d: err = %v, want a validation error", limit, err)
			}
		}
		if _, err := f.s.ListEndpointAttempts(ctx, f.acct.ID, f.ep1.ID, store.AttemptFilter{Outcome: store.DeliveryPending, Limit: 10}); err == nil || errors.Is(err, store.ErrNotFound) {
			t.Errorf("endpoint list, outcome pending: err = %v, want a validation error", err)
		}
	})
}

func TestDeleteExpiredEvents(t *testing.T) {
	storetest.Run(t, func(t *testing.T, db *storetest.DB) {
		ctx := context.Background()
		c := &clock{t: time.Date(2026, 10, 29, 12, 0, 0, 0, time.UTC)}
		s := db.New(t, store.WithClock(c.now))
		acct := mustAccount(t, s)
		mustEndpoint(t, s, acct.ID, store.NewEndpoint{URL: "https://example.com/hook", EventTypes: []string{"a"}})
		other := mustAccount(t, s)
		mustEndpoint(t, s, other.ID, store.NewEndpoint{URL: "https://example.com/other"})

		// newEvent creates an event a second after the last one and, if
		// result is not nil, claims its delivery and records result.
		newEvent := func(accountID, eventType string, result *store.AttemptResult) (store.Event, []store.Attempt) {
			t.Helper()
			c.advance(time.Second)
			e, _ := mustEvent(t, s, accountID, store.NewEvent{EventType: eventType, Payload: []byte(`{}`)})
			if result == nil {
				return e, nil
			}
			d := mustClaim(t, s, 1000*time.Hour)
			if d.EventID != e.ID {
				t.Fatalf("claimed a delivery of %s, want %s", d.EventID, e.ID)
			}
			return e, []store.Attempt{mustRecordAttempt(t, s, d.ID, *result)}
		}
		succeeded := &store.AttemptResult{Number: 1, Outcome: store.AttemptSucceeded, ResponseStatus: 200}
		failed := &store.AttemptResult{Number: 1, Outcome: store.AttemptFailed, Error: "timeout"}
		later := c.t.Add(100 * 24 * time.Hour)
		retried := &store.AttemptResult{Number: 1, Outcome: store.AttemptFailed, Error: "timeout", NextAttemptAt: &later}

		eOK, okAtt := newEvent(acct.ID, "a", succeeded)
		eFailed, failedAtt := newEvent(other.ID, "a", failed)
		eNone, _ := newEvent(acct.ID, "b", nil) // no endpoint takes it
		eWaiting, waitingAtt := newEvent(acct.ID, "a", retried)
		eBusy, _ := newEvent(acct.ID, "a", nil)
		mustClaim(t, s, 1000*time.Hour) // eBusy's delivery stays in progress
		c.advance(30 * 24 * time.Hour)
		eNew, newAtt := newEvent(acct.ID, "a", succeeded)
		cutoff := c.t.Add(-24 * time.Hour)

		present := func(e store.Event) bool {
			t.Helper()
			_, err := s.GetEvent(ctx, e.AccountID, e.ID)
			if err != nil && !errors.Is(err, store.ErrNotFound) {
				t.Fatal(err)
			}
			return err == nil
		}

		// Oldest first, limit respected across calls.
		for i, want := range []struct {
			n    int
			gone []store.Event
			kept []store.Event
		}{
			{2, []store.Event{eOK, eFailed}, []store.Event{eNone}},
			{1, []store.Event{eNone}, nil},
			{0, nil, nil},
		} {
			n, err := s.DeleteExpiredEvents(ctx, cutoff, 2)
			if err != nil {
				t.Fatal(err)
			}
			if n != want.n {
				t.Fatalf("call %d: DeleteExpiredEvents = %d, want %d", i+1, n, want.n)
			}
			for _, e := range want.gone {
				if present(e) {
					t.Errorf("call %d: event %s still there", i+1, e.ID)
				}
			}
			for _, e := range want.kept {
				if !present(e) {
					t.Errorf("call %d: event %s deleted too early", i+1, e.ID)
				}
			}
		}

		// Deleted events take their attempts and deliveries with them.
		for _, a := range append(okAtt, failedAtt...) {
			if _, err := s.GetAttempt(ctx, a.AccountID, a.ID); !errors.Is(err, store.ErrNotFound) {
				t.Errorf("GetAttempt(%s) after delete: err %v, want ErrNotFound", a.ID, err)
			}
		}
		counts, err := s.CountDeliveriesByStatus(ctx)
		if err != nil {
			t.Fatal(err)
		}
		wantCounts := map[string]int{
			store.DeliveryPending:    1, // eWaiting
			store.DeliveryInProgress: 1, // eBusy
			store.DeliverySucceeded:  1, // eNew
			store.DeliveryFailed:     0,
		}
		if !reflect.DeepEqual(counts, wantCounts) {
			t.Errorf("deliveries left = %v, want %v", counts, wantCounts)
		}

		// Old events with unfinished deliveries and newer events stay whole.
		for _, e := range []store.Event{eWaiting, eBusy, eNew} {
			got, err := s.GetEvent(ctx, e.AccountID, e.ID)
			if err != nil {
				t.Fatalf("GetEvent(%s): %v", e.ID, err)
			}
			if len(got.Deliveries) != 1 {
				t.Errorf("event %s has %d deliveries, want 1", e.ID, len(got.Deliveries))
			}
		}
		for _, a := range append(waitingAtt, newAtt...) {
			if _, err := s.GetAttempt(ctx, a.AccountID, a.ID); err != nil {
				t.Errorf("GetAttempt(%s): %v", a.ID, err)
			}
		}

		// Once everything is older than the cutoff and finished, all goes.
		if n, err := s.DeleteExpiredEvents(ctx, c.t.Add(time.Second), 10); err != nil || n != 1 {
			t.Errorf("DeleteExpiredEvents after the newest = %d, %v, want 1 (eNew)", n, err)
		}
		if present(eNew) {
			t.Errorf("event %s still there", eNew.ID)
		}

		if _, err := s.DeleteExpiredEvents(ctx, cutoff, 0); err == nil {
			t.Error("DeleteExpiredEvents with limit 0 succeeded, want an error")
		}
	})
}
