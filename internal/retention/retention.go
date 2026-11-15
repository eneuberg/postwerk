// Package retention runs the store's retention steps at start and then on a
// fixed interval (S6–S10).
//
// Each run removes stored messages that are no longer needed, replaces old
// addresses by keyed hashes, deletes old records and events, writes one
// retention.purged event with counts, and checkpoints the WAL. The work is
// done by the store; this package only schedules it and logs the outcome.
package retention

import (
	"context"
	"log/slog"
	"time"

	"github.com/eneuberg/postwerk/internal/store"
)

// Interval is the fixed time between two runs. It is not configurable: the
// retention periods are, and ten minutes is precise enough for all of them.
const Interval = 10 * time.Minute

// Options configure a Runner.
type Options struct {
	Store  *store.Store
	Policy store.RetainPolicy
	Logger *slog.Logger
	// Interval between runs; zero means Interval. Only tests change it.
	Interval time.Duration
	// Now returns the current time; nil means time.Now.
	Now func() time.Time
}

// Runner runs retention.
type Runner struct {
	opts Options
}

// New creates a Runner.
func New(opts Options) *Runner {
	if opts.Interval <= 0 {
		opts.Interval = Interval
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.Logger == nil {
		opts.Logger = slog.New(slog.DiscardHandler)
	}
	return &Runner{opts: opts}
}

// Once runs every retention step one time.
func (r *Runner) Once(ctx context.Context) (store.RetainCounts, error) {
	c, err := r.opts.Store.Retain(ctx, r.opts.Policy, r.opts.Now())
	if err != nil {
		if ctx.Err() == nil {
			r.opts.Logger.Error("retention run incomplete, will retry", "err", err, "counts", c.String())
		}
		return c, err
	}
	if c != (store.RetainCounts{}) {
		r.opts.Logger.Info("retention run", "raw_purged", c.RawPurged, "raw_expired", c.RawExpired,
			"pseudonymized", c.Pseudonymized, "records_deleted", c.Records, "events_deleted", c.Events)
	}
	return c, nil
}

// Run runs retention every interval until ctx ends. The caller runs Once at
// start, before the delivery workers.
func (r *Runner) Run(ctx context.Context) {
	t := time.NewTicker(r.opts.Interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			_, _ = r.Once(ctx)
		}
	}
}
