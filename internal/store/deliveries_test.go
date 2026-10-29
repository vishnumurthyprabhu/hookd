package store_test

import (
	"context"
	"errors"
	"reflect"
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

func TestRecordDeliveryResult(t *testing.T) {
	storetest.Run(t, func(t *testing.T, db *storetest.DB) {
		ctx := context.Background()
		c := &clock{t: time.Date(2026, 10, 29, 12, 0, 0, 0, time.UTC)}
		s := db.New(t, store.WithClock(c.now))
		acct := mustAccount(t, s)
		ep1 := mustEndpoint(t, s, acct.ID, store.NewEndpoint{URL: "https://example.com/1"})
		ep2 := mustEndpoint(t, s, acct.ID, store.NewEndpoint{URL: "https://example.com/2"})
		e, _ := mustEvent(t, s, acct.ID, store.NewEvent{EventType: "a", Payload: []byte(`{}`)})

		results := map[string]store.DeliveryResult{
			ep1.ID: {Status: store.DeliverySucceeded, ResponseStatus: 204, Latency: 42 * time.Millisecond, AttemptedAt: c.t},
			ep2.ID: {Status: store.DeliveryFailed, Error: "connection refused", Latency: 3 * time.Millisecond},
		}
		for range 2 {
			d := mustClaim(t, s, time.Minute)
			if err := s.RecordDeliveryResult(ctx, d.ID, results[d.EndpointID]); err != nil {
				t.Fatal(err)
			}
		}
		// Finished deliveries are not claimed again, even after the lease.
		c.advance(time.Hour)
		mustClaimNone(t, s, time.Minute)

		got, err := s.GetEvent(ctx, acct.ID, e.ID)
		if err != nil {
			t.Fatal(err)
		}
		want := []store.DeliveryStatus{
			{EndpointID: ep1.ID, Status: store.DeliverySucceeded, ResponseStatus: 204},
			{EndpointID: ep2.ID, Status: store.DeliveryFailed, Error: "connection refused"},
		}
		if !reflect.DeepEqual(got.Deliveries, want) {
			t.Fatalf("deliveries = %+v, want %+v", got.Deliveries, want)
		}
	})
}

func TestRecordDeliveryResultErrors(t *testing.T) {
	storetest.Run(t, func(t *testing.T, db *storetest.DB) {
		ctx := context.Background()
		s := db.New(t)
		acct := mustAccount(t, s)
		mustEndpoint(t, s, acct.ID, store.NewEndpoint{URL: "https://example.com/hook"})
		mustEvent(t, s, acct.ID, store.NewEvent{EventType: "a", Payload: []byte(`{}`)})

		ok := store.DeliveryResult{Status: store.DeliverySucceeded, ResponseStatus: 200}
		if err := s.RecordDeliveryResult(ctx, "dlv_missing", ok); !errors.Is(err, store.ErrNotFound) {
			t.Errorf("unknown delivery: err = %v, want ErrNotFound", err)
		}
		d := mustClaim(t, s, time.Minute)
		if err := s.RecordDeliveryResult(ctx, d.ID, store.DeliveryResult{Status: store.DeliveryPending}); err == nil {
			t.Error("recording a pending status succeeded")
		}
		if err := s.RecordDeliveryResult(ctx, d.ID, ok); err != nil {
			t.Fatal(err)
		}
		// A finished delivery is not in progress any more.
		if err := s.RecordDeliveryResult(ctx, d.ID, ok); !errors.Is(err, store.ErrNotFound) {
			t.Errorf("second result: err = %v, want ErrNotFound", err)
		}
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
