package watch

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/eneuberg/postwerk/internal/store"
)

// Event kinds of the queue check.
const (
	KindQueueStale     = "queue.stale"
	KindQueueRecovered = "queue.recovered"
)

// QueueInterval is how often the queues are looked at.
const QueueInterval = time.Minute

// repeatSlack is how early a repeat of queue.stale may come. The ticks are
// QueueInterval apart, but each check reads the clock (and stores that as
// the time of its event) a little after its tick, by a varying amount, so two
// checks can be slightly less than QueueInterval apart. Without the slack a
// Repeat of one interval waited for the next check: a repeat every 2 minutes
// for POSTWERK_QUEUE_STALE_REPEAT=1m. With half an interval the repeat comes
// with the first check at or after Repeat, however the checks drift.
const repeatSlack = QueueInterval / 2

// QueueStore is what the queue check needs from the store.
type QueueStore interface {
	Store
	Queues(ctx context.Context) (map[string]store.QueueStat, error)
}

// Target is one configured target as the queue check sees it.
type Target struct {
	Name     string
	Optional bool
}

// QueueOptions configure a Queue.
type QueueOptions struct {
	Store   QueueStore
	Targets []Target
	// After: a target whose oldest queued mail is older is stale; 0 turns
	// the check off.
	After time.Duration
	// Repeat, if not 0, writes queue.stale again while a target stays
	// stale, with the first check at or after this interval (repeatSlack).
	// 0: once per change.
	Repeat time.Duration
	Logger *slog.Logger
	Now    func() time.Time
}

// Queue watches the age of the oldest queued mail per target. It is about
// age, not reachability: a mail can wait behind a target that is down, a
// rate limit, or a 4xx that keeps coming back, and all of them look the same
// to the person waiting for it.
type Queue struct {
	opts  QueueOptions
	state map[string]*queueState
}

type queueState struct {
	stale   bool
	written time.Time // when queue.stale was last written
}

// NewQueue returns a Queue that starts from the state the event log has
// for every target.
func NewQueue(ctx context.Context, opts QueueOptions) (*Queue, error) {
	if opts.Logger == nil {
		opts.Logger = slog.New(slog.DiscardHandler)
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	q := &Queue{opts: opts, state: make(map[string]*queueState)}
	if opts.After <= 0 {
		return q, nil
	}
	for _, t := range opts.Targets {
		st := &queueState{}
		last, ok, err := opts.Store.LastOf(ctx, t.Name, KindQueueStale, KindQueueRecovered)
		if err != nil {
			return nil, err
		}
		if ok && last.Kind == KindQueueStale {
			st.stale, st.written = true, last.Time
		}
		q.state[t.Name] = st
	}
	return q, nil
}

// Run checks every QueueInterval until ctx ends.
func (q *Queue) Run(ctx context.Context) {
	if q.opts.After <= 0 {
		return
	}
	t := time.NewTicker(QueueInterval)
	defer t.Stop()
	for {
		if err := q.Check(ctx); err != nil && ctx.Err() == nil {
			q.opts.Logger.Warn("queue check failed", "err", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// Check looks at every target's queue once and writes the events for the
// targets whose state changed (or whose reminder is due).
func (q *Queue) Check(ctx context.Context) error {
	if q.opts.After <= 0 {
		return nil
	}
	queues, err := q.opts.Store.Queues(ctx)
	if err != nil {
		return err
	}
	now := q.opts.Now()
	for _, t := range q.opts.Targets {
		st := q.state[t.Name]
		qs := queues[t.Name]
		age := time.Duration(0)
		if qs.Mails > 0 {
			age = now.Sub(qs.Oldest)
		}
		stale := qs.Mails > 0 && age > q.opts.After
		text := fmt.Sprintf("queued=%d oldest=%s after=%s", qs.Mails, Age(age), Age(q.opts.After))
		var e store.Event
		switch {
		case stale && (!st.stale || q.opts.Repeat > 0 && now.Sub(st.written) >= q.opts.Repeat-repeatSlack):
			level := store.Error
			if t.Optional {
				level = store.Warn
			}
			e = store.Event{Kind: KindQueueStale, Level: level}
		case !stale && st.stale:
			e = store.Event{Kind: KindQueueRecovered, Level: store.Info}
		default:
			continue
		}
		e.Time, e.Target, e.Text = now, t.Name, text
		if err := q.opts.Store.Append(ctx, e); err != nil {
			return err // the state stays, so the next check tries again
		}
		st.stale = stale
		if stale {
			st.written = now
		}
	}
	return nil
}

// Age writes a duration in whole minutes ("37m", "2h05m", "3d04h").
func Age(d time.Duration) string {
	m := int(d / time.Minute)
	switch {
	case m < 60:
		return fmt.Sprintf("%dm", m)
	case m < 24*60:
		return fmt.Sprintf("%dh%02dm", m/60, m%60)
	default:
		return fmt.Sprintf("%dd%02dh", m/(24*60), m%(24*60)/60)
	}
}
