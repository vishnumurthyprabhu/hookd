package delivery

import (
	"context"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"hookd/internal/signing"
	"hookd/internal/store"
)

// ErrEndpointDisabled is the error stored with a delivery whose endpoint was
// disabled or deleted before it was sent (R7).
const ErrEndpointDisabled = "endpoint_disabled"

// recordTimeout bounds storing a delivery's outcome. It is separate from the
// pool's context so that a delivery finished during shutdown is still
// recorded.
const recordTimeout = 10 * time.Second

// resizeInterval is how often Run compares the number of workers with
// Config.Workers.
const resizeInterval = time.Second

// Config holds the pool's settings. The pool reads them again each time it
// uses one, so they can change while it runs.
type Config struct {
	// Workers is the number of delivery goroutines (HOOKD_WORKERS).
	Workers int
	// PollInterval is how often idle workers check for work
	// (HOOKD_POLL_INTERVAL).
	PollInterval time.Duration
	// Timeout bounds each delivery's HTTP request (HOOKD_DELIVERY_TIMEOUT).
	Timeout time.Duration
	// Lease is how long a claimed delivery stays locked
	// (HOOKD_DELIVERY_LEASE). It should be longer than Timeout, or a slow
	// delivery can be claimed and sent a second time.
	Lease time.Duration
	// RetrySchedule is the wait before each retry (HOOKD_RETRY_SCHEDULE):
	// a delivery whose attempt n fails is retried RetrySchedule[n-1] later,
	// with ±10% jitter. Empty means no retries.
	RetrySchedule []time.Duration
}

// Pool is a set of workers that claim due deliveries from the store, sign
// them, send them and record each attempt. A failed attempt is retried after
// the next wait in Config.RetrySchedule, until the schedule runs out (R1–R8).
type Pool struct {
	store  store.Store
	log    *slog.Logger
	cfg    func() Config
	sender *Sender
	wake   chan struct{}
	now    func() time.Time
	// random returns a number in [0, 1) that sets a retry's jitter.
	random func() float64
	// resizeEvery is how often Run checks Config.Workers.
	resizeEvery time.Duration
	// running counts the worker goroutines, including stopped ones still
	// finishing a delivery.
	running atomic.Int64
}

// NewPool returns a pool that works on st. Start it with Run. cfg is called
// whenever the pool needs a setting, so a change applies without a restart:
// the worker count within about a second, the poll interval at a worker's
// next wait, the timeout and lease at the next delivery, and the retry
// schedule at the next wait it sets.
func NewPool(st store.Store, log *slog.Logger, cfg func() Config) *Pool {
	return &Pool{
		store:  st,
		log:    log,
		cfg:    cfg,
		sender: NewSender(),
		// One pending signal is enough: a worker that wakes up claims until
		// the queue is empty and wakes another for each delivery it takes.
		wake:        make(chan struct{}, 1),
		now:         time.Now,
		random:      rand.Float64,
		resizeEvery: resizeInterval,
	}
}

// Wake returns the channel the API signals after storing an event (see
// api.WithWake). Sends must not block.
func (p *Pool) Wake() chan<- struct{} { return p.wake }

// Run starts the workers and blocks until ctx is done and every worker has
// stopped. While it runs, it starts or stops workers to match Workers. A
// delivery being sent when its worker is stopped, or when ctx is done, is
// finished and recorded first, so stopping takes at most about Timeout.
func (p *Pool) Run(ctx context.Context) {
	var wg sync.WaitGroup
	// stops holds one cancel function per worker that has not been told to
	// stop.
	var stops []context.CancelFunc
	resize := func() {
		want := max(p.cfg().Workers, 0)
		if want == len(stops) {
			return
		}
		if len(stops) > 0 {
			p.log.Info("changing the number of workers", "from", len(stops), "to", want)
		}
		for len(stops) < want {
			wctx, stop := context.WithCancel(ctx)
			stops = append(stops, stop)
			wg.Go(func() { p.worker(wctx) })
		}
		for len(stops) > want {
			last := len(stops) - 1
			stops[last]()
			stops = stops[:last]
		}
	}

	resize()
	ticker := time.NewTicker(p.resizeEvery)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			wg.Wait()
			for _, stop := range stops {
				stop()
			}
			return
		case <-ticker.C:
			resize()
		}
	}
}

// worker claims and sends deliveries until there are none, then sleeps until
// it is woken or the poll interval passes. It returns when ctx is done, but
// never in the middle of a delivery.
func (p *Pool) worker(ctx context.Context) {
	p.running.Add(1)
	defer p.running.Add(-1)
	for {
		for ctx.Err() == nil && p.deliverOne(ctx) {
		}
		timer := time.NewTimer(p.cfg().PollInterval)
		select {
		case <-ctx.Done():
		case <-p.wake:
			if ctx.Err() != nil {
				// This worker was stopped: pass the signal on to one that
				// was not.
				p.signal()
			}
		case <-timer.C:
		}
		timer.Stop()
		if ctx.Err() != nil {
			return
		}
	}
}

// deliverOne claims one delivery and handles it. It returns false when there
// was nothing to claim or the claim failed.
func (p *Pool) deliverOne(ctx context.Context) bool {
	d, ok, err := p.store.ClaimDelivery(ctx, p.cfg().Lease)
	if err != nil {
		if ctx.Err() == nil {
			p.log.Error("claim delivery", "err", err)
		}
		return false
	}
	if !ok {
		return false
	}
	// There may be more work: let another worker look for it while this
	// one sends.
	p.signal()
	p.handle(context.WithoutCancel(ctx), d)
	return true
}

func (p *Pool) signal() {
	select {
	case p.wake <- struct{}{}:
	default:
	}
}

// handle makes one attempt at a claimed delivery and records it, with the time
// of the next attempt if it failed and is to be retried. If the job cannot be
// loaded or the attempt cannot be stored, the delivery stays in progress and
// is claimed again once its lease expires, for an attempt with the same
// number.
func (p *Pool) handle(ctx context.Context, d store.ClaimedDelivery) {
	log := p.log.With("delivery_id", d.ID, "event_id", d.EventID, "endpoint_id", d.EndpointID)
	job, err := p.store.GetDeliveryJob(ctx, d.ID)
	if err != nil {
		log.Error("load delivery", "err", err)
		return
	}

	started := p.now()
	var r store.AttemptResult
	if job.EndpointDeleted || !job.Endpoint.Enabled {
		r = store.AttemptResult{Outcome: store.AttemptFailed, Error: ErrEndpointDisabled}
	} else {
		// Each attempt is signed with its own timestamp.
		r = p.send(ctx, job, started)
	}
	finished := p.now()
	r.Number = d.Attempts + 1
	r.StartedAt = started
	r.Latency = finished.Sub(started)
	if r.Outcome == store.AttemptFailed && r.Error != ErrEndpointDisabled {
		r.NextAttemptAt = p.nextAttempt(r.Number, finished)
	}

	rctx, cancel := context.WithTimeout(ctx, recordTimeout)
	defer cancel()
	if _, err := p.store.RecordAttempt(rctx, d.ID, r); err != nil {
		log.Error("record attempt", "attempt", r.Number, "err", err)
		return
	}
	attrs := []any{"attempt", r.Number, "outcome", r.Outcome,
		"response_status", r.ResponseStatus, "error", r.Error, "latency_ms", r.Latency.Milliseconds()}
	if r.NextAttemptAt != nil {
		attrs = append(attrs, "next_attempt_at", *r.NextAttemptAt)
	}
	log.Info("delivery attempt", attrs...)
}

// nextAttempt returns when a delivery whose attempt number failed at now is
// tried again, or nil if the retry schedule has no wait left for it. The
// schedule is read at this moment, so a changed schedule applies to the next
// wait set. Each wait is multiplied by a random factor in [0.9, 1.1].
func (p *Pool) nextAttempt(number int, now time.Time) *time.Time {
	schedule := p.cfg().RetrySchedule
	if number < 1 || number > len(schedule) {
		return nil
	}
	r := 0.9 + 0.2*p.random()
	next := now.Add(time.Duration(float64(schedule[number-1]) * r))
	return &next
}

// send signs the job's event with timestamp ts and POSTs it once.
func (p *Pool) send(ctx context.Context, job store.DeliveryJob, ts time.Time) store.AttemptResult {
	header, err := signing.Sign(job.Secret, job.Event.ID, ts, job.Event.Payload)
	if err != nil {
		return store.AttemptResult{Outcome: store.AttemptFailed, Error: err.Error()}
	}
	status, err := p.sender.Send(ctx, p.cfg().Timeout, job.Endpoint.URL, header, job.Event.Payload)
	switch {
	case err != nil:
		return store.AttemptResult{Outcome: store.AttemptFailed, Error: err.Error()}
	case status >= 200 && status < 300:
		return store.AttemptResult{Outcome: store.AttemptSucceeded, ResponseStatus: status}
	default:
		return store.AttemptResult{Outcome: store.AttemptFailed, ResponseStatus: status, Error: statusError(status)}
	}
}

// statusError explains why a response status counts as a failure.
func statusError(status int) string {
	text := strings.TrimSpace(fmt.Sprintf("%d %s", status, http.StatusText(status)))
	if status >= 300 && status < 400 {
		return "redirect not followed: " + text
	}
	return "non-2xx response: " + text
}
