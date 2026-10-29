package delivery

import (
	"bytes"
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	standardwebhooks "github.com/standard-webhooks/standard-webhooks/libraries/go"

	"hookd/internal/store"
)

// retryConfig polls every few milliseconds, so a retry is sent soon after it
// is due, and retries after a few milliseconds.
func retryConfig(schedule ...time.Duration) Config {
	cfg := testConfig
	cfg.PollInterval = 5 * time.Millisecond
	cfg.RetrySchedule = schedule
	return cfg
}

// scriptedReceiver answers each request with the next status of statuses,
// and with the last one once they run out. It records every request.
func scriptedReceiver(t *testing.T, statuses ...int) (*httptest.Server, chan received) {
	t.Helper()
	got := make(chan received, 100)
	var n atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body bytes.Buffer
		body.ReadFrom(r.Body)
		got <- received{header: r.Header.Clone(), body: body.Bytes()}
		i := min(int(n.Add(1))-1, len(statuses)-1)
		w.WriteHeader(statuses[i])
	}))
	t.Cleanup(srv.Close)
	return srv, got
}

// attempts returns the event's recorded attempts, oldest first.
func (f *fixture) attempts(t *testing.T, eventID string) []store.Attempt {
	t.Helper()
	as, err := f.st.ListEventAttempts(context.Background(), f.account, eventID, store.AttemptFilter{Limit: 100})
	if err != nil {
		t.Fatal(err)
	}
	return as
}

// waitStatus polls until the event's single delivery satisfies cond and
// returns it.
func (f *fixture) waitStatus(t *testing.T, eventID, what string, cond func(store.DeliveryStatus) bool) store.DeliveryStatus {
	t.Helper()
	var d store.DeliveryStatus
	waitFor(t, what, func() bool {
		e, err := f.st.GetEvent(context.Background(), f.account, eventID)
		if err != nil {
			t.Fatal(err)
		}
		if len(e.Deliveries) != 1 {
			t.Fatalf("deliveries = %d, want 1", len(e.Deliveries))
		}
		d = e.Deliveries[0]
		return cond(d)
	})
	return d
}

// syncBuffer is a bytes.Buffer safe for the pool's concurrent log writes.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func TestFailureIsRetriedUntilSuccess(t *testing.T) {
	f := newFixture(t)
	srv, got := scriptedReceiver(t, 500, 503, 200, 500)
	_, secret := f.endpoint(t, srv.URL)
	id := f.event(t, `{"amount":100}`)
	var logs syncBuffer
	cfg := retryConfig(5*time.Millisecond, 5*time.Millisecond, 5*time.Millisecond, 5*time.Millisecond)
	f.startLivePool(t, func() Config { return cfg }, func(p *Pool) {
		p.log = slog.New(slog.NewTextHandler(&logs, nil))
	})

	d := f.waitOne(t, id)
	if d.Status != store.DeliverySucceeded || d.Attempts != 3 || d.ResponseStatus != 200 || d.NextAttemptAt != nil {
		t.Errorf("delivery = %+v, want succeeded on attempt 3", d)
	}
	// Nothing is sent after the success.
	time.Sleep(50 * time.Millisecond)
	if n := len(got); n != 3 {
		t.Fatalf("receiver called %d times, want 3", n)
	}

	// Every attempt carries the same webhook-id and verifies.
	wh, err := standardwebhooks.NewWebhook(secret)
	if err != nil {
		t.Fatal(err)
	}
	for i := range 3 {
		r := <-got
		if v := r.header.Get("Webhook-Id"); v != id {
			t.Errorf("request %d: webhook-id = %q, want %q", i+1, v, id)
		}
		if err := wh.Verify(r.body, r.header); err != nil {
			t.Errorf("request %d: official library rejected it: %v", i+1, err)
		}
	}

	as := f.attempts(t, id)
	want := []struct {
		outcome string
		status  int
	}{{store.AttemptFailed, 500}, {store.AttemptFailed, 503}, {store.AttemptSucceeded, 200}}
	if len(as) != len(want) {
		t.Fatalf("attempts = %+v, want %d", as, len(want))
	}
	for i, a := range as {
		if a.Number != i+1 || a.Outcome != want[i].outcome || a.ResponseStatus != want[i].status {
			t.Errorf("attempt %d = %+v, want %s with %d", i+1, a, want[i].outcome, want[i].status)
		}
	}

	out := logs.String()
	for _, line := range []string{
		`msg="delivery attempt" delivery_id=`,
		`attempt=1 outcome=failed response_status=500`,
		`attempt=2 outcome=failed response_status=503`,
		`attempt=3 outcome=succeeded response_status=200`,
	} {
		if !strings.Contains(out, line) {
			t.Errorf("logs lack %q:\n%s", line, out)
		}
	}
	for _, l := range strings.Split(strings.TrimSpace(out), "\n") {
		failed := strings.Contains(l, "outcome=failed")
		if has := strings.Contains(l, "next_attempt_at="); has != failed {
			t.Errorf("log line has next_attempt_at %v, want %v: %s", has, failed, l)
		}
	}
}

func TestDeliveryFailsAfterLastWait(t *testing.T) {
	f := newFixture(t)
	srv, got := scriptedReceiver(t, 500)
	f.endpoint(t, srv.URL)
	id := f.event(t, `{}`)
	f.startPool(t, retryConfig(5*time.Millisecond, 10*time.Millisecond))

	d := f.waitOne(t, id)
	if d.Status != store.DeliveryFailed || d.Attempts != 3 || d.ResponseStatus != 500 || d.NextAttemptAt != nil {
		t.Errorf("delivery = %+v, want failed after 3 attempts", d)
	}
	time.Sleep(50 * time.Millisecond)
	if n := len(got); n != 3 {
		t.Errorf("receiver called %d times, want 3", n)
	}
	if as := f.attempts(t, id); len(as) != 3 || as[2].Outcome != store.AttemptFailed {
		t.Errorf("attempts = %+v, want 3 failed", as)
	}
}

func TestNoScheduleMeansOneAttempt(t *testing.T) {
	f := newFixture(t)
	srv, got := scriptedReceiver(t, 500, 200)
	f.endpoint(t, srv.URL)
	id := f.event(t, `{}`)
	f.startPool(t, retryConfig()) // HOOKD_RETRY_SCHEDULE=none

	d := f.waitOne(t, id)
	if d.Status != store.DeliveryFailed || d.Attempts != 1 || d.NextAttemptAt != nil {
		t.Errorf("delivery = %+v, want failed after 1 attempt", d)
	}
	time.Sleep(50 * time.Millisecond)
	if n := len(got); n != 1 {
		t.Errorf("receiver called %d times, want 1", n)
	}
}

func TestJitterStaysWithinTenPercent(t *testing.T) {
	now := time.Date(2026, 10, 29, 10, 0, 0, 0, time.UTC)
	schedule := []time.Duration{time.Second, time.Hour}
	p := NewPool(nil, slog.New(slog.DiscardHandler), func() Config { return Config{RetrySchedule: schedule} })

	for _, tc := range []struct {
		random float64
		number int
		want   time.Duration
	}{
		{0, 1, 900 * time.Millisecond},
		{0.5, 1, time.Second},
		{0, 2, 54 * time.Minute},
		{0.5, 2, time.Hour},
		{0.75, 2, 63 * time.Minute},
	} {
		p.random = func() float64 { return tc.random }
		next := p.nextAttempt(tc.number, now)
		if next == nil || (next.Sub(now)-tc.want).Abs() > time.Microsecond {
			t.Errorf("random %v, attempt %d: next = %v, want now + %v", tc.random, tc.number, next, tc.want)
		}
	}
	// Just under 1, the largest value random returns: just under 10% more.
	p.random = func() float64 { return 0.999999 }
	if next := p.nextAttempt(2, now); next == nil || next.Sub(now) >= 66*time.Minute || next.Sub(now) < 65*time.Minute {
		t.Errorf("random near 1: next = %v, want just under now + 66m", next)
	}
	// After the last wait, and before the first attempt, nothing is retried.
	for _, n := range []int{0, 3} {
		if next := p.nextAttempt(n, now); next != nil {
			t.Errorf("attempt %d: next = %v, want none", n, next)
		}
	}

	// The real random source stays within the bounds.
	p.random = NewPool(nil, nil, nil).random
	for range 1000 {
		d := p.nextAttempt(2, now).Sub(now)
		if d < 54*time.Minute || d > 66*time.Minute {
			t.Fatalf("wait = %v, want within 10%% of 1h", d)
		}
	}
}

func TestPoolStoresJitteredRetryTime(t *testing.T) {
	f := newFixture(t)
	srv, _ := scriptedReceiver(t, 500)
	f.endpoint(t, srv.URL)
	id := f.event(t, `{}`)
	cfg := retryConfig(time.Hour)
	f.startLivePool(t, func() Config { return cfg }, func(p *Pool) {
		p.random = func() float64 { return 0 }
	})

	d := f.waitStatus(t, id, "a retry to wait", func(d store.DeliveryStatus) bool { return d.Attempts == 1 })
	if d.Status != store.DeliveryPending || d.NextAttemptAt == nil || d.LastAttemptAt == nil {
		t.Fatalf("delivery = %+v, want pending with a retry time", d)
	}
	if wait := d.NextAttemptAt.Sub(*d.LastAttemptAt); wait < 54*time.Minute || wait > 55*time.Minute {
		t.Errorf("next attempt %v after the last, want 54m (1h with -10%% jitter)", wait)
	}
}

func TestDisabledEndpointStopsRetries(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	srv, got := scriptedReceiver(t, 500)
	epID, _ := f.endpoint(t, srv.URL)
	id := f.event(t, `{}`)
	f.startPool(t, retryConfig(time.Hour, time.Hour))

	f.waitStatus(t, id, "a retry to wait", func(d store.DeliveryStatus) bool {
		return d.Status == store.DeliveryPending && d.Attempts == 1
	})
	off := false
	if _, err := f.st.UpdateEndpoint(ctx, f.account, epID, store.EndpointUpdate{Enabled: &off}); err != nil {
		t.Fatal(err)
	}

	d := f.waitOne(t, id)
	if d.Status != store.DeliveryFailed || d.Attempts != 2 || d.Error != ErrEndpointDisabled || d.NextAttemptAt != nil {
		t.Errorf("delivery = %+v, want failed with %s on attempt 2", d, ErrEndpointDisabled)
	}
	if n := len(got); n != 1 {
		t.Errorf("receiver called %d times, want 1", n)
	}
	as := f.attempts(t, id)
	if len(as) != 2 || as[1].Error != ErrEndpointDisabled || as[1].ResponseStatus != 0 {
		t.Errorf("attempts = %+v, want the second failed with %s", as, ErrEndpointDisabled)
	}
}

func TestChangedScheduleAppliesToNextWait(t *testing.T) {
	var offset atomic.Int64 // added to the store's and the pool's clock
	clock := func() time.Time { return time.Now().Add(time.Duration(offset.Load())) }
	f := newFixture(t, store.WithClock(clock))
	srv, got := scriptedReceiver(t, 500)
	f.endpoint(t, srv.URL)
	id := f.event(t, `{}`)
	live := &liveConfig{cfg: retryConfig(time.Hour, time.Hour)}
	p, _ := f.startLivePool(t, live.get, func(p *Pool) { p.now = clock })

	// Attempt 1 fails and waits about an hour, as the schedule said then.
	d := f.waitStatus(t, id, "a retry to wait", func(d store.DeliveryStatus) bool { return d.Attempts == 1 })
	if wait := d.NextAttemptAt.Sub(*d.LastAttemptAt); wait < 54*time.Minute {
		t.Fatalf("first wait = %v, want about 1h", wait)
	}

	// The schedule changes while it waits. The wait already set stays; the
	// next ones follow the new schedule, which also allows more retries.
	live.set(func(c *Config) {
		c.RetrySchedule = []time.Duration{time.Hour, 5 * time.Millisecond, 5 * time.Millisecond}
	})
	offset.Store(int64(2 * time.Hour))
	select {
	case p.Wake() <- struct{}{}:
	default:
	}

	d = f.waitOne(t, id)
	if d.Status != store.DeliveryFailed || d.Attempts != 4 {
		t.Errorf("delivery = %+v, want failed after 4 attempts", d)
	}
	if n := len(got); n != 4 {
		t.Errorf("receiver called %d times, want 4", n)
	}
	as := f.attempts(t, id)
	if len(as) != 4 {
		t.Fatalf("attempts = %+v, want 4", as)
	}
	for i := 2; i < 4; i++ {
		if gap := as[i].StartedAt.Sub(as[i-1].StartedAt); gap > time.Minute {
			t.Errorf("attempt %d started %v after attempt %d, want milliseconds", i+1, gap, i)
		}
	}
}
