package delivery

import (
	"context"
	"fmt"
	"log/slog"
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
}

// Pool is a set of workers that claim deliveries from the store, sign them,
// send each once and record the outcome (R13–R18). Nothing is retried.
type Pool struct {
	store  store.Store
	log    *slog.Logger
	cfg    func() Config
	sender *Sender
	wake   chan struct{}
	now    func() time.Time
	// resizeEvery is how often Run checks Config.Workers.
	resizeEvery time.Duration
	// running counts the worker goroutines, including stopped ones still
	// finishing a delivery.
	running atomic.Int64
}

// NewPool returns a pool that works on st. Start it with Run. cfg is called
// whenever the pool needs a setting, so a change applies without a restart:
// the worker count within about a second, the poll interval at a worker's
// next wait, and the timeout and lease at the next delivery.
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

// handle sends a claimed delivery and records the outcome. If the job cannot
// be loaded or the outcome cannot be stored, the delivery stays in progress
// and is claimed again once its lease expires.
func (p *Pool) handle(ctx context.Context, d store.ClaimedDelivery) {
	log := p.log.With("delivery_id", d.ID, "event_id", d.EventID, "endpoint_id", d.EndpointID)
	job, err := p.store.GetDeliveryJob(ctx, d.ID)
	if err != nil {
		log.Error("load delivery", "err", err)
		return
	}

	started := p.now()
	var r store.DeliveryResult
	if job.EndpointDeleted || !job.Endpoint.Enabled {
		r = store.DeliveryResult{Status: store.DeliveryFailed, Error: ErrEndpointDisabled}
	} else {
		r = p.send(ctx, job, started)
	}
	r.AttemptedAt = started
	r.Latency = p.now().Sub(started)

	rctx, cancel := context.WithTimeout(ctx, recordTimeout)
	defer cancel()
	if err := p.store.RecordDeliveryResult(rctx, d.ID, r); err != nil {
		log.Error("record delivery result", "err", err)
		return
	}
	log.Info("delivery "+r.Status,
		"response_status", r.ResponseStatus, "error", r.Error, "latency_ms", r.Latency.Milliseconds())
}

// send signs the job's event and POSTs it once.
func (p *Pool) send(ctx context.Context, job store.DeliveryJob, ts time.Time) store.DeliveryResult {
	header, err := signing.Sign(job.Secret, job.Event.ID, ts, job.Event.Payload)
	if err != nil {
		return store.DeliveryResult{Status: store.DeliveryFailed, Error: err.Error()}
	}
	status, err := p.sender.Send(ctx, p.cfg().Timeout, job.Endpoint.URL, header, job.Event.Payload)
	switch {
	case err != nil:
		return store.DeliveryResult{Status: store.DeliveryFailed, Error: err.Error()}
	case status >= 200 && status < 300:
		return store.DeliveryResult{Status: store.DeliverySucceeded, ResponseStatus: status}
	default:
		return store.DeliveryResult{Status: store.DeliveryFailed, ResponseStatus: status, Error: statusError(status)}
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
