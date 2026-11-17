package senders

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/eneuberg/postwerk/internal/store"
)

// UsageStore is what Usage writes to (internal/store.Store).
type UsageStore interface {
	TouchSenderUsage(ctx context.Context, uses []store.SenderUse) error
}

// DefaultUsageInterval is how often Usage writes what it collected.
const DefaultUsageInterval = time.Minute

// Usage remembers the last login of every credential in memory and writes
// them in one transaction per interval, so a busy sender costs one row
// update a minute, not one per mail. A crash loses at most one interval of
// "last used" times, never a mail or an event. It is safe for concurrent use.
type Usage struct {
	st       UsageStore
	logger   *slog.Logger
	interval time.Duration

	mu      sync.Mutex
	pending map[usageKey]time.Time
}

type usageKey struct {
	source Kind
	sender string
	keyID  string
}

var _ UsageRecorder = (*Usage)(nil)

// NewUsage returns a Usage writing to st; interval 0 means
// DefaultUsageInterval.
func NewUsage(st UsageStore, interval time.Duration, logger *slog.Logger) *Usage {
	if interval <= 0 {
		interval = DefaultUsageInterval
	}
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	return &Usage{st: st, logger: logger, interval: interval, pending: map[usageKey]time.Time{}}
}

// Record notes a login.
func (u *Usage) Record(p Principal, at time.Time) {
	k := usageKey{source: p.Source, sender: p.Sender, keyID: p.KeyID}
	u.mu.Lock()
	if at.After(u.pending[k]) {
		u.pending[k] = at
	}
	u.mu.Unlock()
}

// Run writes every interval until ctx ends; call Flush after it.
func (u *Usage) Run(ctx context.Context) {
	t := time.NewTicker(u.interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if err := u.Flush(ctx); err != nil && ctx.Err() == nil {
				u.logger.Warn("writing sender usage failed; tried again later", "err", err)
			}
		}
	}
}

// Flush writes what was collected. On failure it keeps it for the next try.
func (u *Usage) Flush(ctx context.Context) error {
	u.mu.Lock()
	batch := u.pending
	u.pending = map[usageKey]time.Time{}
	u.mu.Unlock()
	if len(batch) == 0 {
		return nil
	}
	uses := make([]store.SenderUse, 0, len(batch))
	for k, at := range batch {
		uses = append(uses, store.SenderUse{Source: string(k.source), Sender: k.sender, KeyID: k.keyID, At: at})
	}
	if err := u.st.TouchSenderUsage(ctx, uses); err != nil {
		u.mu.Lock()
		for k, at := range batch {
			if at.After(u.pending[k]) {
				u.pending[k] = at
			}
		}
		u.mu.Unlock()
		return err
	}
	return nil
}
