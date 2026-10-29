// Package retention runs the retention runner, which deletes events older
// than HOOKD_RETENTION together with their deliveries and attempts (R17–R19).
package retention

import (
	"context"
	"log/slog"
	"time"

	"hookd/internal/store"
)

const (
	// FirstDelay is how long the runner waits after it starts before its
	// first run.
	FirstDelay = time.Minute
	// Interval is how long the runner waits between runs.
	Interval = time.Hour
	// BatchSize is how many events each call to DeleteExpiredEvents deletes
	// at most.
	BatchSize = 500
)

// Clock tells the time and waits. Tests replace it.
type Clock interface {
	Now() time.Time
	// After returns a channel that receives once d has passed.
	After(d time.Duration) <-chan time.Time
}

type realClock struct{}

func (realClock) Now() time.Time                         { return time.Now() }
func (realClock) After(d time.Duration) <-chan time.Time { return time.After(d) }

// Runner deletes expired events: it waits FirstDelay, then runs every
// Interval.
type Runner struct {
	store      store.Store
	log        *slog.Logger
	retention  func() time.Duration
	clock      Clock
	firstDelay time.Duration
	interval   time.Duration
	batchSize  int
}

// Option changes a Runner made by New.
type Option func(*Runner)

// WithClock makes the runner tell the time and wait with c instead of the
// system clock.
func WithClock(c Clock) Option { return func(r *Runner) { r.clock = c } }

// WithFirstDelay sets the wait before the first run instead of FirstDelay.
func WithFirstDelay(d time.Duration) Option { return func(r *Runner) { r.firstDelay = d } }

// WithInterval sets the wait between runs instead of Interval.
func WithInterval(d time.Duration) Option { return func(r *Runner) { r.interval = d } }

// WithBatchSize sets how many events each delete takes at most instead of
// BatchSize.
func WithBatchSize(n int) Option { return func(r *Runner) { r.batchSize = n } }

// New returns a runner that deletes expired events from st. Start it with
// Run. retention is called at each run, so it always uses the current
// HOOKD_RETENTION.
func New(st store.Store, log *slog.Logger, retention func() time.Duration, opts ...Option) *Runner {
	r := &Runner{
		store:      st,
		log:        log,
		retention:  retention,
		clock:      realClock{},
		firstDelay: FirstDelay,
		interval:   Interval,
		batchSize:  BatchSize,
	}
	for _, o := range opts {
		o(r)
	}
	return r
}

// Run waits the first delay, then deletes expired events once per interval.
// It returns when ctx is done; a run in progress stops after its current
// batch.
func (r *Runner) Run(ctx context.Context) {
	wait := r.firstDelay
	for {
		select {
		case <-ctx.Done():
			return
		case <-r.clock.After(wait):
		}
		r.RunOnce(ctx)
		wait = r.interval
	}
}

// RunOnce deletes the events created before now minus the retention period,
// one batch at a time until a batch comes back short, and returns how many
// it deleted. Errors are logged; the next run tries again.
func (r *Runner) RunOnce(ctx context.Context) int {
	cutoff := r.clock.Now().Add(-r.retention())
	total := 0
	for ctx.Err() == nil {
		n, err := r.store.DeleteExpiredEvents(ctx, cutoff, r.batchSize)
		total += n
		if err != nil {
			if ctx.Err() == nil {
				r.log.Error("delete expired events", "cutoff", cutoff, "deleted", total, "err", err)
			}
			return total
		}
		if n < r.batchSize {
			break
		}
	}
	r.log.Info("deleted expired events", "cutoff", cutoff, "deleted", total)
	return total
}
