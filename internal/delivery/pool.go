package delivery

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"sync"
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

// Config holds the pool's settings.
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
	cfg    Config
	sender *Sender
	wake   chan struct{}
	now    func() time.Time
}

// NewPool returns a pool that works on st. Start it with Run.
func NewPool(st store.Store, log *slog.Logger, cfg Config) *Pool {
	return &Pool{
		store:  st,
		log:    log,
		cfg:    cfg,
		sender: NewSender(cfg.Timeout),
		// One pending signal is enough: a worker that wakes up claims until
		// the queue is empty and wakes another for each delivery it takes.
		wake: make(chan struct{}, 1),
		now:  time.Now,
	}
}

// Wake returns the channel the API signals after storing an event (see
// api.WithWake). Sends must not block.
func (p *Pool) Wake() chan<- struct{} { return p.wake }

// Run starts the workers and blocks until ctx is done and every worker has
// stopped. A delivery being sent when ctx is done is finished and recorded
// first, so stopping takes at most about Timeout.
func (p *Pool) Run(ctx context.Context) {
	var wg sync.WaitGroup
	for range p.cfg.Workers {
		wg.Go(func() { p.worker(ctx) })
	}
	wg.Wait()
}

// worker claims and sends deliveries until there are none, then sleeps until
// it is woken or the poll interval passes.
func (p *Pool) worker(ctx context.Context) {
	ticker := time.NewTicker(p.cfg.PollInterval)
	defer ticker.Stop()
	for {
		for ctx.Err() == nil && p.deliverOne(ctx) {
		}
		select {
		case <-ctx.Done():
			return
		case <-p.wake:
		case <-ticker.C:
		}
	}
}

// deliverOne claims one delivery and handles it. It returns false when there
// was nothing to claim or the claim failed.
func (p *Pool) deliverOne(ctx context.Context) bool {
	d, ok, err := p.store.ClaimDelivery(ctx, p.cfg.Lease)
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
	status, err := p.sender.Send(ctx, job.Endpoint.URL, header, job.Event.Payload)
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
