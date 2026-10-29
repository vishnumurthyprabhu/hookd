package store_test

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"hookd/internal/store"
	"hookd/internal/store/storetest"
)

func mustClaim(t *testing.T, s store.Store, lease time.Duration) store.ClaimedDelivery {
	t.Helper()
	d, ok, err := s.ClaimDelivery(context.Background(), lease)
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatal("ClaimDelivery found nothing to claim")
	}
	return d
}

func mustClaimNone(t *testing.T, s store.Store, lease time.Duration) {
	t.Helper()
	d, ok, err := s.ClaimDelivery(context.Background(), lease)
	if err != nil {
		t.Fatal(err)
	}
	if ok {
		t.Fatalf("ClaimDelivery claimed %+v, want nothing", d)
	}
}

func TestClaimDeliveryLease(t *testing.T) {
	storetest.Run(t, func(t *testing.T, db *storetest.DB) {
		ctx := context.Background()
		c := &clock{t: time.Date(2026, 10, 29, 12, 0, 0, 0, time.UTC)}
		s := db.New(t, store.WithClock(c.now))
		acct := mustAccount(t, s)
		ep := mustEndpoint(t, s, acct.ID, store.NewEndpoint{URL: "https://example.com/hook"})

		mustClaimNone(t, s, time.Minute)

		e, _ := mustEvent(t, s, acct.ID, store.NewEvent{EventType: "a", Payload: []byte(`{}`)})
		d := mustClaim(t, s, time.Minute)
		if d.EventID != e.ID || d.EndpointID != ep.ID {
			t.Fatalf("claimed %+v, want event %s and endpoint %s", d, e.ID, ep.ID)
		}
		if want := c.t.Add(time.Minute); !d.LockedUntil.Equal(want) {
			t.Errorf("LockedUntil = %v, want %v", d.LockedUntil, want)
		}
		got, err := s.GetEvent(ctx, acct.ID, e.ID)
		if err != nil {
			t.Fatal(err)
		}
		if got.Deliveries[0].Status != store.DeliveryInProgress {
			t.Errorf("status after claim = %q, want %q", got.Deliveries[0].Status, store.DeliveryInProgress)
		}

		// While the lease is valid the row is not handed out again.
		mustClaimNone(t, s, time.Minute)
		c.advance(time.Minute)
		mustClaimNone(t, s, time.Minute)

		// Once it has expired, the row is claimed again with a new lease.
		c.advance(time.Microsecond)
		again := mustClaim(t, s, time.Minute)
		if again.ID != d.ID {
			t.Fatalf("reclaimed %s, want %s", again.ID, d.ID)
		}
		if want := c.t.Add(time.Minute); !again.LockedUntil.Equal(want) {
			t.Errorf("LockedUntil after reclaim = %v, want %v", again.LockedUntil, want)
		}
		mustClaimNone(t, s, time.Minute)
	})
}

func TestClaimDeliveryOldestFirst(t *testing.T) {
	storetest.Run(t, func(t *testing.T, db *storetest.DB) {
		c := &clock{t: time.Date(2026, 10, 29, 12, 0, 0, 0, time.UTC)}
		s := db.New(t, store.WithClock(c.now))
		acct := mustAccount(t, s)
		mustEndpoint(t, s, acct.ID, store.NewEndpoint{URL: "https://example.com/hook"})

		var want []string
		for range 3 {
			e, _ := mustEvent(t, s, acct.ID, store.NewEvent{EventType: "a", Payload: []byte(`{}`)})
			want = append(want, e.ID)
			c.advance(time.Second)
		}
		var got []string
		for range 3 {
			got = append(got, mustClaim(t, s, time.Minute).EventID)
		}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("claimed events %v, want %v", got, want)
		}
		mustClaimNone(t, s, time.Minute)
	})
}

func TestClaimDeliveryRejectsBadLease(t *testing.T) {
	storetest.Run(t, func(t *testing.T, db *storetest.DB) {
		s := db.New(t)
		if _, _, err := s.ClaimDelivery(context.Background(), 0); err == nil {
			t.Fatal("ClaimDelivery with a zero lease succeeded")
		}
	})
}

func TestConcurrentClaimsOnSQLite(t *testing.T) {
	ctx := context.Background()
	s := storetest.New(t)
	acct := mustAccount(t, s)
	for range 4 {
		mustEndpoint(t, s, acct.ID, store.NewEndpoint{URL: "https://example.com/hook"})
	}
	const events = 25
	for range events {
		mustEvent(t, s, acct.ID, store.NewEvent{EventType: "a", Payload: []byte(`{}`)})
	}
	const total = events * 4

	var (
		mu      sync.Mutex
		claimed = map[string]int{}
		wg      sync.WaitGroup
		errs    = make(chan error, 8)
	)
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				d, ok, err := s.ClaimDelivery(ctx, time.Hour)
				if err != nil {
					errs <- err
					return
				}
				if !ok {
					return
				}
				mu.Lock()
				claimed[d.ID]++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
	if len(claimed) != total {
		t.Errorf("claimed %d distinct deliveries, want %d", len(claimed), total)
	}
	for id, n := range claimed {
		if n != 1 {
			t.Errorf("delivery %s claimed %d times", id, n)
		}
	}
}

// TestConcurrentClaimsOnPostgres checks that FOR UPDATE SKIP LOCKED never
// hands a delivery to two workers, with several instances, each with its own
// connection pool, claiming at once.
func TestConcurrentClaimsOnPostgres(t *testing.T) {
	ctx := context.Background()
	db := storetest.Postgres(t)
	const (
		instances = 4
		workers   = 3 // per instance
	)
	db.Config.MaxConns = workers

	s := db.New(t)
	acct := mustAccount(t, s)
	for range 4 {
		mustEndpoint(t, s, acct.ID, store.NewEndpoint{URL: "https://example.com/hook"})
	}
	const events = 50
	for range events {
		mustEvent(t, s, acct.ID, store.NewEvent{EventType: "a", Payload: []byte(`{}`)})
	}
	const total = events * 4

	var (
		mu      sync.Mutex
		claimed = map[string]int{}
		wg      sync.WaitGroup
		start   = make(chan struct{})
		errs    = make(chan error, instances*workers)
	)
	for range instances {
		inst := db.Open(t)
		for range workers {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				for {
					d, ok, err := inst.ClaimDelivery(ctx, time.Hour)
					if err != nil {
						errs <- err
						return
					}
					if !ok {
						return
					}
					mu.Lock()
					claimed[d.ID]++
					mu.Unlock()
				}
			}()
		}
	}
	close(start)
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
	if len(claimed) != total {
		t.Errorf("claimed %d distinct deliveries, want %d", len(claimed), total)
	}
	for id, n := range claimed {
		if n != 1 {
			t.Errorf("delivery %s claimed %d times", id, n)
		}
	}
}

// mustRecordAttempt records an attempt and fails the test if it cannot.
func mustRecordAttempt(t *testing.T, s store.Store, deliveryID string, r store.AttemptResult) store.Attempt {
	t.Helper()
	a, err := s.RecordAttempt(context.Background(), deliveryID, r)
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func TestRecordAttempt(t *testing.T) {
	storetest.Run(t, func(t *testing.T, db *storetest.DB) {
		ctx := context.Background()
		c := &clock{t: time.Date(2026, 10, 29, 12, 0, 0, 0, time.UTC)}
		s := db.New(t, store.WithClock(c.now))
		acct := mustAccount(t, s)
		ep1 := mustEndpoint(t, s, acct.ID, store.NewEndpoint{URL: "https://example.com/1"})
		ep2 := mustEndpoint(t, s, acct.ID, store.NewEndpoint{URL: "https://example.com/2"})
		e, _ := mustEvent(t, s, acct.ID, store.NewEvent{EventType: "a", Payload: []byte(`{}`)})

		started := c.t.Add(-time.Second)
		results := map[string]store.AttemptResult{
			ep1.ID: {Number: 1, Outcome: store.AttemptSucceeded, ResponseStatus: 204, Latency: 42*time.Millisecond + 300*time.Microsecond, StartedAt: started},
			ep2.ID: {Number: 1, Outcome: store.AttemptFailed, Error: "connection refused", Latency: 3 * time.Millisecond},
		}
		got := map[string]store.Attempt{}
		for range 2 {
			d := mustClaim(t, s, time.Minute)
			if d.Attempts != 0 {
				t.Errorf("first claim: Attempts = %d, want 0", d.Attempts)
			}
			a := mustRecordAttempt(t, s, d.ID, results[d.EndpointID])
			if a.DeliveryID != d.ID || a.EventID != e.ID || a.EndpointID != d.EndpointID || a.AccountID != acct.ID {
				t.Errorf("attempt %+v does not match delivery %+v of account %s", a, d, acct.ID)
			}
			if !strings.HasPrefix(a.ID, "att_") {
				t.Errorf("attempt ID = %q, want an att_ ID", a.ID)
			}
			got[d.EndpointID] = a
		}
		a1 := got[ep1.ID]
		if a1.Number != 1 || a1.Outcome != store.AttemptSucceeded || a1.ResponseStatus != 204 ||
			a1.Latency != 42*time.Millisecond || !a1.StartedAt.Equal(started) || a1.Error != "" {
			t.Errorf("success attempt = %+v", a1)
		}
		a2 := got[ep2.ID]
		if a2.Outcome != store.AttemptFailed || a2.Error != "connection refused" || a2.ResponseStatus != 0 || !a2.StartedAt.Equal(c.t) {
			t.Errorf("failed attempt = %+v, want started at the store's time %v", a2, c.t)
		}

		// Final deliveries are not claimed again, even after the lease.
		c.advance(time.Hour)
		mustClaimNone(t, s, time.Minute)

		ev, err := s.GetEvent(ctx, acct.ID, e.ID)
		if err != nil {
			t.Fatal(err)
		}
		now := c.t.Add(-time.Hour)
		want := []store.DeliveryStatus{
			{EndpointID: ep1.ID, Status: store.DeliverySucceeded, ResponseStatus: 204, Attempts: 1, LastAttemptAt: &started},
			{EndpointID: ep2.ID, Status: store.DeliveryFailed, Error: "connection refused", Attempts: 1, LastAttemptAt: &now},
		}
		if !reflect.DeepEqual(ev.Deliveries, want) {
			t.Fatalf("deliveries = %+v, want %+v", ev.Deliveries, want)
		}
	})
}

// TestRecordAttemptRetry follows one delivery through a failure with a next
// attempt, the wait, and a second, final attempt.
func TestRecordAttemptRetry(t *testing.T) {
	storetest.Run(t, func(t *testing.T, db *storetest.DB) {
		ctx := context.Background()
		c := &clock{t: time.Date(2026, 10, 29, 12, 0, 0, 0, time.UTC)}
		s := db.New(t, store.WithClock(c.now))
		acct := mustAccount(t, s)
		ep := mustEndpoint(t, s, acct.ID, store.NewEndpoint{URL: "https://example.com/hook"})
		e, _ := mustEvent(t, s, acct.ID, store.NewEvent{EventType: "a", Payload: []byte(`{}`)})
		delivery := func() store.DeliveryStatus {
			t.Helper()
			ev, err := s.GetEvent(ctx, acct.ID, e.ID)
			if err != nil {
				t.Fatal(err)
			}
			return ev.Deliveries[0]
		}

		// A new delivery has no attempts and no next attempt to report.
		if got, want := delivery(), (store.DeliveryStatus{EndpointID: ep.ID, Status: store.DeliveryPending}); !reflect.DeepEqual(got, want) {
			t.Fatalf("new delivery = %+v, want %+v", got, want)
		}

		d := mustClaim(t, s, time.Minute)
		first := c.t
		next := c.t.Add(time.Hour)
		mustRecordAttempt(t, s, d.ID, store.AttemptResult{
			Number: 1, Outcome: store.AttemptFailed, ResponseStatus: 500, Error: "non-2xx response: 500 Internal Server Error",
			StartedAt: first, NextAttemptAt: &next,
		})
		want := store.DeliveryStatus{
			EndpointID: ep.ID, Status: store.DeliveryPending, ResponseStatus: 500, Error: "non-2xx response: 500 Internal Server Error",
			Attempts: 1, LastAttemptAt: &first, NextAttemptAt: &next,
		}
		if got := delivery(); !reflect.DeepEqual(got, want) {
			t.Fatalf("waiting delivery = %+v, want %+v", got, want)
		}
		if n, err := s.CountWaitingRetries(ctx); err != nil || n != 1 {
			t.Errorf("CountWaitingRetries = %d, %v, want 1", n, err)
		}

		// The same attempt cannot be recorded twice.
		if _, err := s.RecordAttempt(ctx, d.ID, store.AttemptResult{Number: 1, Outcome: store.AttemptSucceeded}); !errors.Is(err, store.ErrNotFound) {
			t.Errorf("second record of attempt 1: err = %v, want ErrNotFound", err)
		}

		// Not due yet, then due.
		mustClaimNone(t, s, time.Minute)
		c.advance(time.Hour - time.Microsecond)
		mustClaimNone(t, s, time.Minute)
		c.advance(time.Microsecond)
		d = mustClaim(t, s, time.Minute)
		if d.Attempts != 1 {
			t.Fatalf("claimed retry with Attempts = %d, want 1", d.Attempts)
		}
		// A stale record of attempt 1 does not count as attempt 2.
		if _, err := s.RecordAttempt(ctx, d.ID, store.AttemptResult{Number: 1, Outcome: store.AttemptSucceeded}); !errors.Is(err, store.ErrNotFound) {
			t.Errorf("record of attempt 1 after the retry was claimed: err = %v, want ErrNotFound", err)
		}
		second := c.t
		a := mustRecordAttempt(t, s, d.ID, store.AttemptResult{Number: 2, Outcome: store.AttemptSucceeded, ResponseStatus: 200, StartedAt: second})
		if a.Number != 2 {
			t.Errorf("attempt number = %d, want 2", a.Number)
		}
		want = store.DeliveryStatus{EndpointID: ep.ID, Status: store.DeliverySucceeded, ResponseStatus: 200, Attempts: 2, LastAttemptAt: &second}
		if got := delivery(); !reflect.DeepEqual(got, want) {
			t.Fatalf("final delivery = %+v, want %+v", got, want)
		}
		if n, err := s.CountWaitingRetries(ctx); err != nil || n != 0 {
			t.Errorf("CountWaitingRetries after success = %d, %v, want 0", n, err)
		}
		c.advance(time.Hour)
		mustClaimNone(t, s, time.Minute)
	})
}

// TestRecordAttemptAfterReclaim has two workers holding the same attempt
// after a lease expired: only the first to record it is stored.
func TestRecordAttemptAfterReclaim(t *testing.T) {
	storetest.Run(t, func(t *testing.T, db *storetest.DB) {
		ctx := context.Background()
		c := &clock{t: time.Date(2026, 10, 29, 12, 0, 0, 0, time.UTC)}
		s := db.New(t, store.WithClock(c.now))
		acct := mustAccount(t, s)
		mustEndpoint(t, s, acct.ID, store.NewEndpoint{URL: "https://example.com/hook"})
		mustEvent(t, s, acct.ID, store.NewEvent{EventType: "a", Payload: []byte(`{}`)})

		slow := mustClaim(t, s, time.Minute)
		c.advance(time.Minute + time.Microsecond)
		fast := mustClaim(t, s, time.Minute)
		if fast.ID != slow.ID || fast.Attempts != 0 {
			t.Fatalf("reclaimed %+v, want %s with no attempts", fast, slow.ID)
		}
		mustRecordAttempt(t, s, fast.ID, store.AttemptResult{Number: fast.Attempts + 1, Outcome: store.AttemptSucceeded, ResponseStatus: 200})
		if _, err := s.RecordAttempt(ctx, slow.ID, store.AttemptResult{Number: slow.Attempts + 1, Outcome: store.AttemptFailed, Error: "timeout"}); !errors.Is(err, store.ErrNotFound) {
			t.Errorf("late record: err = %v, want ErrNotFound", err)
		}
	})
}

func TestRecordAttemptErrors(t *testing.T) {
	storetest.Run(t, func(t *testing.T, db *storetest.DB) {
		ctx := context.Background()
		s := db.New(t)
		acct := mustAccount(t, s)
		mustEndpoint(t, s, acct.ID, store.NewEndpoint{URL: "https://example.com/hook"})
		mustEvent(t, s, acct.ID, store.NewEvent{EventType: "a", Payload: []byte(`{}`)})

		ok := store.AttemptResult{Number: 1, Outcome: store.AttemptSucceeded, ResponseStatus: 200}
		if _, err := s.RecordAttempt(ctx, "dlv_missing", ok); !errors.Is(err, store.ErrNotFound) {
			t.Errorf("unknown delivery: err = %v, want ErrNotFound", err)
		}
		d := mustClaim(t, s, time.Minute)
		later := time.Now().Add(time.Hour)
		for name, r := range map[string]store.AttemptResult{
			"pending outcome":         {Number: 1, Outcome: store.DeliveryPending},
			"number 0":                {Number: 0, Outcome: store.AttemptSucceeded},
			"success with next retry": {Number: 1, Outcome: store.AttemptSucceeded, NextAttemptAt: &later},
		} {
			if _, err := s.RecordAttempt(ctx, d.ID, r); err == nil || errors.Is(err, store.ErrNotFound) {
				t.Errorf("%s: err = %v, want a validation error", name, err)
			}
		}
		if _, err := s.RecordAttempt(ctx, d.ID, store.AttemptResult{Number: 2, Outcome: store.AttemptSucceeded}); !errors.Is(err, store.ErrNotFound) {
			t.Errorf("number 2 for a first attempt: err = %v, want ErrNotFound", err)
		}
		mustRecordAttempt(t, s, d.ID, ok)
		// A finished delivery is not in progress any more.
		if _, err := s.RecordAttempt(ctx, d.ID, ok); !errors.Is(err, store.ErrNotFound) {
			t.Errorf("second record: err = %v, want ErrNotFound", err)
		}
	})
}

// TestClaimDeliveryDueFirst checks that a retry and a new delivery are taken
// in the order they came due, not the order they were created.
func TestClaimDeliveryDueFirst(t *testing.T) {
	storetest.Run(t, func(t *testing.T, db *storetest.DB) {
		c := &clock{t: time.Date(2026, 10, 29, 12, 0, 0, 0, time.UTC)}
		s := db.New(t, store.WithClock(c.now))
		acct := mustAccount(t, s)
		mustEndpoint(t, s, acct.ID, store.NewEndpoint{URL: "https://example.com/hook"})

		old, _ := mustEvent(t, s, acct.ID, store.NewEvent{EventType: "a", Payload: []byte(`{}`)})
		d := mustClaim(t, s, time.Minute)
		next := c.t.Add(10 * time.Second)
		mustRecordAttempt(t, s, d.ID, store.AttemptResult{Number: 1, Outcome: store.AttemptFailed, Error: "boom", NextAttemptAt: &next})
		c.advance(5 * time.Second)
		newer, _ := mustEvent(t, s, acct.ID, store.NewEvent{EventType: "a", Payload: []byte(`{}`)})

		c.advance(time.Minute)
		if got := mustClaim(t, s, time.Minute); got.EventID != newer.ID || got.Attempts != 0 {
			t.Errorf("first claim = %+v, want the new event %s", got, newer.ID)
		}
		if got := mustClaim(t, s, time.Minute); got.EventID != old.ID || got.Attempts != 1 {
			t.Errorf("second claim = %+v, want the retry of %s", got, old.ID)
		}
	})
}

// TestEndpointGoneMakesRetriesDue checks that disabling or deleting an
// endpoint makes its deliveries waiting for a retry due at once, and leaves
// other endpoints' deliveries waiting.
func TestEndpointGoneMakesRetriesDue(t *testing.T) {
	for name, gone := range map[string]func(ctx context.Context, s store.Store, accountID, id string) error{
		"disabled": func(ctx context.Context, s store.Store, accountID, id string) error {
			_, err := s.UpdateEndpoint(ctx, accountID, id, store.EndpointUpdate{Enabled: ptr(false)})
			return err
		},
		"deleted": func(ctx context.Context, s store.Store, accountID, id string) error {
			return s.DeleteEndpoint(ctx, accountID, id)
		},
	} {
		t.Run(name, func(t *testing.T) {
			storetest.Run(t, func(t *testing.T, db *storetest.DB) {
				ctx := context.Background()
				c := &clock{t: time.Date(2026, 10, 29, 12, 0, 0, 0, time.UTC)}
				s := db.New(t, store.WithClock(c.now))
				acct := mustAccount(t, s)
				ep1 := mustEndpoint(t, s, acct.ID, store.NewEndpoint{URL: "https://example.com/1"})
				ep2 := mustEndpoint(t, s, acct.ID, store.NewEndpoint{URL: "https://example.com/2"})
				mustEvent(t, s, acct.ID, store.NewEvent{EventType: "a", Payload: []byte(`{}`)})
				next := c.t.Add(5 * time.Hour)
				for range 2 {
					d := mustClaim(t, s, time.Minute)
					mustRecordAttempt(t, s, d.ID, store.AttemptResult{Number: 1, Outcome: store.AttemptFailed, Error: "boom", NextAttemptAt: &next})
				}

				c.advance(time.Minute)
				mustClaimNone(t, s, time.Minute)
				if err := gone(ctx, s, acct.ID, ep1.ID); err != nil {
					t.Fatal(err)
				}
				got := mustClaim(t, s, time.Minute)
				if got.EndpointID != ep1.ID || got.Attempts != 1 {
					t.Fatalf("claimed %+v, want the retry for %s", got, ep1.ID)
				}
				mustRecordAttempt(t, s, got.ID, store.AttemptResult{Number: 2, Outcome: store.AttemptFailed, Error: "endpoint_disabled"})
				// The other endpoint's delivery still waits.
				mustClaimNone(t, s, time.Minute)
				c.advance(5*time.Hour - time.Minute)
				if got := mustClaim(t, s, time.Minute); got.EndpointID != ep2.ID {
					t.Fatalf("claimed %+v, want the retry for %s", got, ep2.ID)
				}
			})
		})
	}
}

// TestEnableEndpointKeepsRetriesWaiting checks that enabling an endpoint,
// whether it was disabled or not, does not move its deliveries' next attempt.
func TestEnableEndpointKeepsRetriesWaiting(t *testing.T) {
	storetest.Run(t, func(t *testing.T, db *storetest.DB) {
		ctx := context.Background()
		c := &clock{t: time.Date(2026, 10, 29, 12, 0, 0, 0, time.UTC)}
		s := db.New(t, store.WithClock(c.now))
		acct := mustAccount(t, s)
		ep := mustEndpoint(t, s, acct.ID, store.NewEndpoint{URL: "https://example.com/hook"})
		enable := func(on bool) {
			t.Helper()
			if _, err := s.UpdateEndpoint(ctx, acct.ID, ep.ID, store.EndpointUpdate{Enabled: ptr(on)}); err != nil {
				t.Fatal(err)
			}
		}
		mustEvent(t, s, acct.ID, store.NewEvent{EventType: "a", Payload: []byte(`{}`)})
		d := mustClaim(t, s, time.Minute)
		next := c.t.Add(5 * time.Hour)
		mustRecordAttempt(t, s, d.ID, store.AttemptResult{Number: 1, Outcome: store.AttemptFailed, Error: "boom", NextAttemptAt: &next})

		// Enabling an enabled endpoint, or changing anything else, is not a
		// disable.
		enable(true)
		if _, err := s.UpdateEndpoint(ctx, acct.ID, ep.ID, store.EndpointUpdate{URL: ptr("https://example.com/other")}); err != nil {
			t.Fatal(err)
		}
		mustClaimNone(t, s, time.Minute)

		// Disabled then enabled again: the delivery stays due.
		enable(false)
		enable(true)
		if got := mustClaim(t, s, time.Minute); got.ID != d.ID {
			t.Fatalf("claimed %+v, want %s", got, d.ID)
		}
	})
}

func TestCountWaitingRetries(t *testing.T) {
	storetest.Run(t, func(t *testing.T, db *storetest.DB) {
		ctx := context.Background()
		c := &clock{t: time.Date(2026, 10, 29, 12, 0, 0, 0, time.UTC)}
		s := db.New(t, store.WithClock(c.now))
		count := func(want int) {
			t.Helper()
			n, err := s.CountWaitingRetries(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if n != want {
				t.Fatalf("CountWaitingRetries = %d, want %d", n, want)
			}
		}
		count(0)

		// Two accounts, five deliveries: two wait for a retry, one is new,
		// one is final and one is in progress again on its retry.
		a, _ := s.CreateAccount(ctx, "a")
		b, _ := s.CreateAccount(ctx, "b")
		mustEndpoint(t, s, a.ID, store.NewEndpoint{URL: "https://example.com/1"})
		mustEndpoint(t, s, b.ID, store.NewEndpoint{URL: "https://example.com/2"})
		for range 2 {
			mustEvent(t, s, a.ID, store.NewEvent{EventType: "x", Payload: []byte(`{}`)})
			mustEvent(t, s, b.ID, store.NewEvent{EventType: "x", Payload: []byte(`{}`)})
		}
		soon, later := c.t.Add(time.Second), c.t.Add(time.Hour)
		for i, next := range []*time.Time{&soon, &later, &later, nil} {
			d := mustClaim(t, s, time.Minute)
			mustRecordAttempt(t, s, d.ID, store.AttemptResult{Number: 1, Outcome: store.AttemptFailed, Error: fmt.Sprint("boom ", i), NextAttemptAt: next})
		}
		count(3)
		c.advance(time.Second)
		mustClaim(t, s, time.Minute) // the retry due soon
		count(2)
		mustEvent(t, s, a.ID, store.NewEvent{EventType: "x", Payload: []byte(`{}`)})
		count(2)
	})
}

func TestGetDeliveryJob(t *testing.T) {
	storetest.Run(t, func(t *testing.T, db *storetest.DB) {
		ctx := context.Background()
		c := &clock{t: time.Date(2026, 10, 29, 12, 0, 0, 0, time.UTC)}
		s := db.New(t, store.WithClock(c.now))
		acct := mustAccount(t, s)
		ep, secret, err := s.CreateEndpoint(ctx, acct.ID, store.NewEndpoint{URL: "https://example.com/hook", EventTypes: []string{"a"}})
		if err != nil {
			t.Fatal(err)
		}
		payload := []byte(`{"x":  1}`)
		e, _ := mustEvent(t, s, acct.ID, store.NewEvent{EventType: "a", Payload: payload, IdempotencyKey: "k"})
		d := mustClaim(t, s, time.Minute)

		j, err := s.GetDeliveryJob(ctx, d.ID)
		if err != nil {
			t.Fatal(err)
		}
		wantEvent := e
		wantEvent.Deliveries = []store.DeliveryStatus{}
		want := store.DeliveryJob{DeliveryID: d.ID, Event: wantEvent, Endpoint: ep, Secret: secret}
		if !reflect.DeepEqual(j, want) {
			t.Fatalf("GetDeliveryJob = %+v, want %+v", j, want)
		}

		// A disabled endpoint is still returned, marked as not enabled.
		if _, err := s.UpdateEndpoint(ctx, acct.ID, ep.ID, store.EndpointUpdate{Enabled: ptr(false)}); err != nil {
			t.Fatal(err)
		}
		if j, err = s.GetDeliveryJob(ctx, d.ID); err != nil {
			t.Fatal(err)
		}
		if j.Endpoint.Enabled || j.EndpointDeleted {
			t.Errorf("disabled endpoint: Enabled = %v, EndpointDeleted = %v, want false, false", j.Endpoint.Enabled, j.EndpointDeleted)
		}

		// So is a deleted one, marked as deleted.
		if err := s.DeleteEndpoint(ctx, acct.ID, ep.ID); err != nil {
			t.Fatal(err)
		}
		if j, err = s.GetDeliveryJob(ctx, d.ID); err != nil {
			t.Fatal(err)
		}
		if !j.EndpointDeleted || j.Endpoint.ID != ep.ID {
			t.Errorf("deleted endpoint: job = %+v, want EndpointDeleted for %s", j, ep.ID)
		}

		if _, err := s.GetDeliveryJob(ctx, "dlv_missing"); !errors.Is(err, store.ErrNotFound) {
			t.Errorf("unknown delivery: err = %v, want ErrNotFound", err)
		}
	})
}

func TestCountDeliveriesByStatus(t *testing.T) {
	storetest.Run(t, func(t *testing.T, db *storetest.DB) {
		ctx := context.Background()
		s := db.New(t)
		count := func(want map[string]int) {
			t.Helper()
			got, err := s.CountDeliveriesByStatus(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("CountDeliveriesByStatus = %v, want %v", got, want)
			}
		}
		count(map[string]int{
			store.DeliveryPending: 0, store.DeliveryInProgress: 0, store.DeliverySucceeded: 0, store.DeliveryFailed: 0,
		})

		// Two accounts with three endpoints between them: five deliveries.
		a, _ := s.CreateAccount(ctx, "a")
		b, _ := s.CreateAccount(ctx, "b")
		mustEndpoint(t, s, a.ID, store.NewEndpoint{URL: "https://example.com/1"})
		mustEndpoint(t, s, a.ID, store.NewEndpoint{URL: "https://example.com/2"})
		mustEndpoint(t, s, b.ID, store.NewEndpoint{URL: "https://example.com/3"})
		mustEvent(t, s, a.ID, store.NewEvent{EventType: "x", Payload: []byte(`{}`)})
		mustEvent(t, s, a.ID, store.NewEvent{EventType: "x", Payload: []byte(`{}`)})
		mustEvent(t, s, b.ID, store.NewEvent{EventType: "x", Payload: []byte(`{}`)})

		for _, r := range []store.AttemptResult{
			{Number: 1, Outcome: store.AttemptSucceeded, ResponseStatus: 200},
			{Number: 1, Outcome: store.AttemptFailed, Error: "boom"},
		} {
			mustRecordAttempt(t, s, mustClaim(t, s, time.Minute).ID, r)
		}
		mustClaim(t, s, time.Minute)

		count(map[string]int{
			store.DeliveryPending: 2, store.DeliveryInProgress: 1, store.DeliverySucceeded: 1, store.DeliveryFailed: 1,
		})
	})
}
