package retention

import (
	"context"
	"errors"
	"log/slog"
	"slices"
	"sync"
	"testing"
	"time"

	"hookd/internal/store"
	"hookd/internal/store/storetest"
)

// fakeClock is a clock the test moves by hand. Each call to After is sent on
// waits, and the test fires it by sending on the channel it carries.
type fakeClock struct {
	mu    sync.Mutex
	t     time.Time
	waits chan fakeWait
}

type fakeWait struct {
	d    time.Duration
	fire chan time.Time
}

func newFakeClock() *fakeClock {
	return &fakeClock{t: time.Date(2026, 10, 29, 12, 0, 0, 0, time.UTC), waits: make(chan fakeWait, 1)}
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

func (c *fakeClock) After(d time.Duration) <-chan time.Time {
	w := fakeWait{d: d, fire: make(chan time.Time, 1)}
	c.waits <- w
	return w.fire
}

// nextWait returns the runner's next wait, which it asks for once the
// previous run has finished.
func (c *fakeClock) nextWait(t *testing.T) fakeWait {
	t.Helper()
	select {
	case w := <-c.waits:
		return w
	case <-time.After(10 * time.Second):
		t.Fatal("the runner did not wait")
		return fakeWait{}
	}
}

// countingStore records how many events each DeleteExpiredEvents call
// deleted.
type countingStore struct {
	store.Store
	mu      sync.Mutex
	deleted []int
}

func (s *countingStore) DeleteExpiredEvents(ctx context.Context, cutoff time.Time, limit int) (int, error) {
	n, err := s.Store.DeleteExpiredEvents(ctx, cutoff, limit)
	s.mu.Lock()
	s.deleted = append(s.deleted, n)
	s.mu.Unlock()
	return n, err
}

func (s *countingStore) calls() []int {
	s.mu.Lock()
	defer s.mu.Unlock()
	calls := s.deleted
	s.deleted = nil
	return calls
}

func TestRunnerDeletesInBatches(t *testing.T) {
	ctx := context.Background()
	clock := newFakeClock()
	st := &countingStore{Store: storetest.New(t, store.WithClock(clock.Now))}
	acct, err := st.CreateAccount(ctx, "acme")
	if err != nil {
		t.Fatal(err)
	}
	newEvents := func(n int) []string {
		t.Helper()
		var ids []string
		for range n {
			clock.advance(time.Second)
			e, _, err := st.CreateEvent(ctx, acct.ID, store.NewEvent{EventType: "order.paid", Payload: []byte(`{}`)})
			if err != nil {
				t.Fatal(err)
			}
			ids = append(ids, e.ID)
		}
		return ids
	}
	present := func(id string) bool {
		t.Helper()
		_, err := st.GetEvent(ctx, acct.ID, id)
		if err != nil && !errors.Is(err, store.ErrNotFound) {
			t.Fatal(err)
		}
		return err == nil
	}

	old := newEvents(7)
	clock.advance(48 * time.Hour)
	recent := newEvents(2)

	r := New(st, slog.New(slog.DiscardHandler), func() time.Duration { return 24 * time.Hour },
		WithClock(clock), WithBatchSize(3))
	runCtx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		r.Run(runCtx)
	}()

	w := clock.nextWait(t)
	if w.d != FirstDelay {
		t.Fatalf("first wait = %v, want %v", w.d, FirstDelay)
	}
	if calls := st.calls(); len(calls) != 0 {
		t.Fatalf("deleted before the first delay: %v", calls)
	}
	w.fire <- clock.Now()

	w = clock.nextWait(t)
	if w.d != Interval {
		t.Fatalf("wait after a run = %v, want %v", w.d, Interval)
	}
	if calls, want := st.calls(), []int{3, 3, 1}; !slices.Equal(calls, want) {
		t.Fatalf("first run deleted %v per batch, want %v", calls, want)
	}
	for _, id := range old {
		if present(id) {
			t.Errorf("old event %s was kept", id)
		}
	}
	for _, id := range recent {
		if !present(id) {
			t.Errorf("recent event %s was deleted", id)
		}
	}

	// A day later the recent events have expired too.
	clock.advance(25 * time.Hour)
	w.fire <- clock.Now()
	w = clock.nextWait(t)
	if calls, want := st.calls(), []int{2}; !slices.Equal(calls, want) {
		t.Fatalf("second run deleted %v per batch, want %v", calls, want)
	}
	for _, id := range recent {
		if present(id) {
			t.Errorf("event %s was kept after it expired", id)
		}
	}

	cancel()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("Run did not return after the context was cancelled")
	}
	if calls := st.calls(); len(calls) != 0 {
		t.Fatalf("deleted after the context was cancelled: %v", calls)
	}
}

func TestRunnerReadsRetentionEachRun(t *testing.T) {
	ctx := context.Background()
	clock := newFakeClock()
	st := &countingStore{Store: storetest.New(t, store.WithClock(clock.Now))}
	acct, err := st.CreateAccount(ctx, "acme")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := st.CreateEvent(ctx, acct.ID, store.NewEvent{EventType: "a", Payload: []byte(`{}`)}); err != nil {
		t.Fatal(err)
	}
	clock.advance(2 * time.Hour)

	var mu sync.Mutex
	retention := 3 * time.Hour
	r := New(st, slog.New(slog.DiscardHandler), func() time.Duration {
		mu.Lock()
		defer mu.Unlock()
		return retention
	}, WithClock(clock))
	if n := r.RunOnce(ctx); n != 0 {
		t.Fatalf("deleted %d events younger than the retention period", n)
	}
	mu.Lock()
	retention = time.Hour
	mu.Unlock()
	if n := r.RunOnce(ctx); n != 1 {
		t.Fatalf("deleted %d events, want 1 once the retention period is shorter", n)
	}
}

func TestRunnerStopsBeforeFirstRun(t *testing.T) {
	clock := newFakeClock()
	st := &countingStore{}
	r := New(st, slog.New(slog.DiscardHandler), func() time.Duration { return time.Hour }, WithClock(clock))
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		r.Run(ctx)
	}()
	clock.nextWait(t)
	cancel()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("Run did not return after the context was cancelled")
	}
	if calls := st.calls(); len(calls) != 0 {
		t.Fatalf("deleted before the first delay: %v", calls)
	}
}
