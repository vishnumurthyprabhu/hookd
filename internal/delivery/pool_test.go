package delivery

import (
	"context"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	standardwebhooks "github.com/standard-webhooks/standard-webhooks/libraries/go"

	"hookd/internal/store"
	"hookd/internal/store/storetest"
)

// testConfig polls rarely, so tests that rely on the wake signal would hang
// without it, and times out quickly.
var testConfig = Config{
	Workers:      4,
	PollInterval: time.Hour,
	Timeout:      300 * time.Millisecond,
	Lease:        time.Minute,
}

type fixture struct {
	st      store.Store
	account string
}

func newFixture(t *testing.T, opts ...store.Option) *fixture {
	t.Helper()
	st := storetest.New(t, opts...)
	acct, err := st.CreateAccount(context.Background(), "acme")
	if err != nil {
		t.Fatal(err)
	}
	return &fixture{st: st, account: acct.ID}
}

// endpoint registers an endpoint for url and returns its ID and secret.
func (f *fixture) endpoint(t *testing.T, url string) (string, string) {
	t.Helper()
	ep, secret, err := f.st.CreateEndpoint(context.Background(), f.account, store.NewEndpoint{URL: url})
	if err != nil {
		t.Fatal(err)
	}
	return ep.ID, secret
}

// event stores an event, which fans out to every endpoint, and returns its ID.
func (f *fixture) event(t *testing.T, payload string) string {
	t.Helper()
	e, _, err := f.st.CreateEvent(context.Background(), f.account, store.NewEvent{
		EventType: "order.paid",
		Payload:   []byte(payload),
	})
	if err != nil {
		t.Fatal(err)
	}
	return e.ID
}

// liveConfig is a pool configuration a test changes while the pool runs.
type liveConfig struct {
	mu  sync.Mutex
	cfg Config
}

func (l *liveConfig) get() Config {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.cfg
}

func (l *liveConfig) set(change func(*Config)) {
	l.mu.Lock()
	defer l.mu.Unlock()
	change(&l.cfg)
}

// startPool runs a pool with a fixed configuration on the fixture's store,
// wakes it once, and stops it when the test ends. The returned function stops
// it early.
func (f *fixture) startPool(t *testing.T, cfg Config) func() {
	t.Helper()
	_, stop := f.startLivePool(t, func() Config { return cfg })
	return stop
}

// startLivePool is startPool with a configuration read from cfg. The pool
// checks the worker count every 10ms. Each setup function can change the pool
// before it starts.
func (f *fixture) startLivePool(t *testing.T, cfg func() Config, setup ...func(*Pool)) (*Pool, func()) {
	t.Helper()
	p := NewPool(f.st, slog.New(slog.DiscardHandler), cfg)
	p.resizeEvery = 10 * time.Millisecond
	for _, s := range setup {
		s(p)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		p.Run(ctx)
	}()
	p.Wake() <- struct{}{}
	var once sync.Once
	stop := func() {
		once.Do(func() {
			cancel()
			select {
			case <-done:
			case <-time.After(5 * time.Second):
				t.Error("pool did not stop")
			}
		})
	}
	t.Cleanup(stop)
	return p, stop
}

// wait polls until every delivery of the event is final and returns them.
func (f *fixture) wait(t *testing.T, eventID string) []store.DeliveryStatus {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		e, err := f.st.GetEvent(context.Background(), f.account, eventID)
		if err != nil {
			t.Fatal(err)
		}
		final := true
		for _, d := range e.Deliveries {
			if d.Status != store.DeliverySucceeded && d.Status != store.DeliveryFailed {
				final = false
			}
		}
		if final {
			return e.Deliveries
		}
		if time.Now().After(deadline) {
			t.Fatalf("deliveries not final: %+v", e.Deliveries)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// waitOne is wait for an event with a single delivery.
func (f *fixture) waitOne(t *testing.T, eventID string) store.DeliveryStatus {
	t.Helper()
	ds := f.wait(t, eventID)
	if len(ds) != 1 {
		t.Fatalf("deliveries = %d, want 1", len(ds))
	}
	return ds[0]
}

type received struct {
	header http.Header
	body   []byte
}

// receiver is an httptest server that answers status and records each
// request.
func receiver(t *testing.T, status int) (*httptest.Server, chan received) {
	t.Helper()
	got := make(chan received, 100)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		got <- received{header: r.Header.Clone(), body: body}
		if status >= 300 && status < 400 {
			w.Header().Set("Location", "/elsewhere")
		}
		w.WriteHeader(status)
		io.WriteString(w, strings.Repeat("x", 100<<10))
	}))
	t.Cleanup(srv.Close)
	return srv, got
}

func TestDeliverySucceedsOn2xx(t *testing.T) {
	for _, status := range []int{200, 204, 299} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			f := newFixture(t)
			srv, got := receiver(t, status)
			f.endpoint(t, srv.URL)
			id := f.event(t, `{"amount":100}`)
			f.startPool(t, testConfig)

			d := f.waitOne(t, id)
			if d.Status != store.DeliverySucceeded || d.ResponseStatus != status || d.Error != "" {
				t.Errorf("delivery = %+v, want succeeded with %d", d, status)
			}
			if n := len(got); n != 1 {
				t.Errorf("receiver called %d times, want 1", n)
			}
		})
	}
}

func TestDeliveryFailsOnOtherStatus(t *testing.T) {
	for _, status := range []int{500, 302, 404} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			f := newFixture(t)
			srv, got := receiver(t, status)
			f.endpoint(t, srv.URL)
			id := f.event(t, `{"amount":100}`)
			f.startPool(t, testConfig)

			d := f.waitOne(t, id)
			if d.Status != store.DeliveryFailed || d.ResponseStatus != status {
				t.Errorf("delivery = %+v, want failed with %d", d, status)
			}
			if d.Error == "" {
				t.Error("failed delivery has no error text")
			}
			// With no retry schedule it is not retried, and a redirect is not
			// followed.
			time.Sleep(50 * time.Millisecond)
			if n := len(got); n != 1 {
				t.Errorf("receiver called %d times, want 1", n)
			}
		})
	}
}

func TestDeliveryFailsOnTimeout(t *testing.T) {
	f := newFixture(t)
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-release:
		}
	}))
	t.Cleanup(srv.Close)
	t.Cleanup(func() { close(release) })
	f.endpoint(t, srv.URL)
	id := f.event(t, `{}`)
	start := time.Now()
	f.startPool(t, testConfig)

	d := f.waitOne(t, id)
	if d.Status != store.DeliveryFailed || d.ResponseStatus != 0 || !strings.Contains(d.Error, "timeout") {
		t.Errorf("delivery = %+v, want failed with a timeout", d)
	}
	if took := time.Since(start); took > 3*time.Second {
		t.Errorf("took %v; the %v timeout was not applied", took, testConfig.Timeout)
	}
}

func TestDeliveryFailsOnRefusedConnection(t *testing.T) {
	f := newFixture(t)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	ln.Close() // nothing listens there now
	f.endpoint(t, "http://"+addr+"/hook")
	id := f.event(t, `{}`)
	f.startPool(t, testConfig)

	d := f.waitOne(t, id)
	if d.Status != store.DeliveryFailed || d.ResponseStatus != 0 || !strings.Contains(d.Error, "refused") {
		t.Errorf("delivery = %+v, want failed with connection refused", d)
	}
	if strings.Contains(d.Error, "Post ") {
		t.Errorf("error %q repeats the request", d.Error)
	}
}

func TestDisabledOrDeletedEndpointIsNotCalled(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	srv, got := receiver(t, 200)
	disabled, _ := f.endpoint(t, srv.URL+"/disabled")
	deleted, _ := f.endpoint(t, srv.URL+"/deleted")
	id := f.event(t, `{}`)

	off := false
	if _, err := f.st.UpdateEndpoint(ctx, f.account, disabled, store.EndpointUpdate{Enabled: &off}); err != nil {
		t.Fatal(err)
	}
	if err := f.st.DeleteEndpoint(ctx, f.account, deleted); err != nil {
		t.Fatal(err)
	}
	f.startPool(t, testConfig)

	ds := f.wait(t, id)
	if len(ds) != 2 {
		t.Fatalf("deliveries = %d, want 2", len(ds))
	}
	for _, d := range ds {
		if d.Status != store.DeliveryFailed || d.Error != ErrEndpointDisabled || d.ResponseStatus != 0 {
			t.Errorf("delivery = %+v, want failed with %s", d, ErrEndpointDisabled)
		}
	}
	if n := len(got); n != 0 {
		t.Errorf("receiver called %d times, want 0", n)
	}
}

func TestDeliveryVerifiesWithOfficialLibrary(t *testing.T) {
	f := newFixture(t)
	srv, got := receiver(t, 200)
	_, secret := f.endpoint(t, srv.URL)
	payload := `{"order":{"id":42,"name":"Zoë"}}`
	id := f.event(t, payload)
	f.startPool(t, testConfig)
	f.waitOne(t, id)

	r := <-got
	wh, err := standardwebhooks.NewWebhook(secret)
	if err != nil {
		t.Fatal(err)
	}
	if err := wh.Verify(r.body, r.header); err != nil {
		t.Errorf("official library rejected delivery: %v", err)
	}
	if string(r.body) != payload {
		t.Errorf("body = %q, want %q", r.body, payload)
	}
	for k, want := range map[string]string{
		"Content-Type": "application/json",
		"User-Agent":   "Hookd/1",
		"Webhook-Id":   id,
	} {
		if v := r.header.Get(k); v != want {
			t.Errorf("%s = %q, want %q", k, v, want)
		}
	}
}

func TestExpiredLeaseIsSentAfterRestart(t *testing.T) {
	ctx := context.Background()
	var offset atomic.Int64 // added to the store's clock
	clock := func() time.Time { return time.Now().Add(time.Duration(offset.Load())) }
	db := storetest.SQLite(t)
	st := db.New(t, store.WithClock(clock))
	acct, err := st.CreateAccount(ctx, "acme")
	if err != nil {
		t.Fatal(err)
	}
	f := &fixture{st: st, account: acct.ID}
	srv, got := receiver(t, 200)
	f.endpoint(t, srv.URL)
	id := f.event(t, `{}`)

	// A worker claims the delivery and the process dies before sending it.
	if _, ok, err := st.ClaimDelivery(ctx, time.Minute); err != nil || !ok {
		t.Fatalf("claim: ok %v, err %v", ok, err)
	}
	st.Close()

	// The process restarts while the lease still holds: nothing is sent.
	f.st = db.Open(t, store.WithClock(clock))
	stop := f.startPool(t, testConfig)
	time.Sleep(50 * time.Millisecond)
	stop()
	if n := len(got); n != 0 {
		t.Fatalf("receiver called %d times while the lease held, want 0", n)
	}

	// Once the lease has expired, the next pool sends it.
	offset.Store(int64(2 * time.Minute))
	f.startPool(t, testConfig)
	if d := f.waitOne(t, id); d.Status != store.DeliverySucceeded {
		t.Errorf("delivery = %+v, want succeeded", d)
	}
	if n := len(got); n != 1 {
		t.Errorf("receiver called %d times, want 1", n)
	}
}

func TestPollFindsWorkWithoutWake(t *testing.T) {
	f := newFixture(t)
	srv, _ := receiver(t, 200)
	f.endpoint(t, srv.URL)
	cfg := testConfig
	cfg.PollInterval = 20 * time.Millisecond
	f.startPool(t, cfg) // the one wake finds nothing
	time.Sleep(30 * time.Millisecond)

	id := f.event(t, `{}`)
	if d := f.waitOne(t, id); d.Status != store.DeliverySucceeded {
		t.Errorf("delivery = %+v, want succeeded", d)
	}
}

func TestOneWakeDeliversEveryDelivery(t *testing.T) {
	f := newFixture(t)
	srv, got := receiver(t, 200)
	for range 5 {
		f.endpoint(t, srv.URL)
	}
	ids := []string{f.event(t, `{"n":1}`), f.event(t, `{"n":2}`)}
	f.startPool(t, testConfig) // polls hourly, so only the wake starts it

	for _, id := range ids {
		for _, d := range f.wait(t, id) {
			if d.Status != store.DeliverySucceeded {
				t.Errorf("delivery = %+v, want succeeded", d)
			}
		}
	}
	if n := len(got); n != 10 {
		t.Errorf("receiver called %d times, want 10", n)
	}
}

func TestShutdownFinishesInFlightDelivery(t *testing.T) {
	f := newFixture(t)
	arrived := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(arrived)
		time.Sleep(100 * time.Millisecond)
	}))
	t.Cleanup(srv.Close)
	f.endpoint(t, srv.URL)
	id := f.event(t, `{}`)
	cfg := testConfig
	cfg.Timeout = 5 * time.Second
	stop := f.startPool(t, cfg)

	<-arrived
	stop()
	if d := f.waitOne(t, id); d.Status != store.DeliverySucceeded {
		t.Errorf("delivery = %+v, want succeeded", d)
	}
}

// waitFor polls cond until it holds, and fails the test after 5 seconds.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestWorkerCountFollowsSetting(t *testing.T) {
	f := newFixture(t)
	var inFlight, peak atomic.Int64
	arrived := make(chan struct{}, 100)
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := inFlight.Add(1)
		defer inFlight.Add(-1)
		for p := peak.Load(); n > p && !peak.CompareAndSwap(p, n); p = peak.Load() {
		}
		arrived <- struct{}{}
		select {
		case <-release:
		case <-r.Context().Done():
		}
		time.Sleep(20 * time.Millisecond)
	}))
	t.Cleanup(srv.Close)
	for range 3 {
		f.endpoint(t, srv.URL)
	}
	live := &liveConfig{cfg: testConfig}
	live.set(func(c *Config) { c.Workers, c.Timeout = 1, 5*time.Second })
	first := f.event(t, `{"n":1}`) // three deliveries
	p, _ := f.startLivePool(t, live.get)

	// One worker: one delivery at a time.
	<-arrived
	time.Sleep(50 * time.Millisecond)
	if n := inFlight.Load(); n != 1 {
		t.Fatalf("deliveries in flight with 1 worker = %d, want 1", n)
	}

	// Grown to three, the new workers take the other two.
	live.set(func(c *Config) { c.Workers = 3 })
	for range 2 {
		select {
		case <-arrived:
		case <-time.After(5 * time.Second):
			t.Fatal("new workers did not start delivering")
		}
	}
	if n := p.running.Load(); n != 3 {
		t.Fatalf("workers running = %d, want 3", n)
	}

	// Shrunk to one while all three are sending: the stopped workers finish
	// their deliveries first.
	live.set(func(c *Config) { c.Workers = 1 })
	time.Sleep(50 * time.Millisecond)
	if n := p.running.Load(); n != 3 {
		t.Fatalf("workers running while sending = %d, want 3 until their deliveries finish", n)
	}
	close(release)
	for _, d := range f.wait(t, first) {
		if d.Status != store.DeliverySucceeded {
			t.Errorf("delivery = %+v, want succeeded", d)
		}
	}
	waitFor(t, "workers to stop", func() bool { return p.running.Load() == 1 })

	// The one worker left still delivers, one at a time.
	peak.Store(0)
	second := f.event(t, `{"n":2}`)
	select {
	case p.Wake() <- struct{}{}:
	default:
	}
	for _, d := range f.wait(t, second) {
		if d.Status != store.DeliverySucceeded {
			t.Errorf("delivery = %+v, want succeeded", d)
		}
	}
	if n := peak.Load(); n != 1 {
		t.Errorf("deliveries in flight with 1 worker = %d, want 1", n)
	}
}

func TestChangedTimeoutAppliesToNextSend(t *testing.T) {
	f := newFixture(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-time.After(200 * time.Millisecond):
		case <-r.Context().Done():
		}
	}))
	t.Cleanup(srv.Close)
	f.endpoint(t, srv.URL)
	live := &liveConfig{cfg: testConfig}
	live.set(func(c *Config) { c.Timeout = 5 * time.Second })
	p, _ := f.startLivePool(t, live.get)

	if d := f.waitOne(t, f.event(t, `{"n":1}`)); d.Status != store.DeliverySucceeded {
		t.Fatalf("delivery with a 5s timeout = %+v, want succeeded", d)
	}

	live.set(func(c *Config) { c.Timeout = 50 * time.Millisecond })
	id := f.event(t, `{"n":2}`)
	select {
	case p.Wake() <- struct{}{}:
	default:
	}
	d := f.waitOne(t, id)
	if d.Status != store.DeliveryFailed || !strings.Contains(d.Error, "timeout: no response within 50ms") {
		t.Errorf("delivery with a 50ms timeout = %+v, want failed with a timeout of 50ms", d)
	}
}
