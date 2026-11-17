// Package limit counts what a sender may do in a period, in memory (D64).
//
// A Limiter keeps one token bucket per key. A Rate of N per period lets a key
// take N at once and then N spread over each period: the bucket holds at most
// N tokens and refills at N per period. A key that is not used for a full
// period is back at N and its bucket is dropped, so memory stays bounded by
// the keys that were busy within the last period.
//
// The buckets live in memory only. A restart starts every key full, which
// lets a sender send at most one more burst; the limits are there to stop a
// leaked key from sending at full speed for hours, not to count exactly.
package limit

import (
	"sync"
	"time"
)

// Rate is N per Per. A zero N means no limit.
type Rate struct {
	N   int
	Per time.Duration
}

// Unlimited reports whether r allows everything.
func (r Rate) Unlimited() bool { return r.N <= 0 || r.Per <= 0 }

// Limiter holds the buckets. It is safe for concurrent use.
type Limiter struct {
	now func() time.Time

	mu      sync.Mutex
	buckets map[string]*bucket
	calls   int // since the last sweep
}

type bucket struct {
	tokens float64
	last   time.Time
	rate   Rate
}

// sweepEvery is how many calls pass between two sweeps of idle buckets.
const sweepEvery = 1024

// New returns a Limiter; now nil means time.Now.
func New(now func() time.Time) *Limiter {
	if now == nil {
		now = time.Now
	}
	return &Limiter{now: now, buckets: make(map[string]*bucket)}
}

// Allow takes one token from key's bucket for rate r and reports whether
// there was one. An unlimited rate always allows and keeps no bucket.
func (l *Limiter) Allow(key string, r Rate) bool { return l.AllowN(key, r, 1) }

// AllowN takes n tokens at once, or none.
func (l *Limiter) AllowN(key string, r Rate, n int) bool {
	if r.Unlimited() {
		return true
	}
	if n <= 0 {
		return true
	}
	now := l.now()
	l.mu.Lock()
	defer l.mu.Unlock()
	l.calls++
	if l.calls >= sweepEvery {
		l.sweep(now)
	}
	b := l.buckets[key]
	if b == nil {
		b = &bucket{tokens: float64(r.N), last: now, rate: r}
		l.buckets[key] = b
	}
	b.refill(now, r)
	if b.tokens < float64(n) {
		return false
	}
	b.tokens -= float64(n)
	return true
}

// refill adds what the time since the last call earned, up to the rate's N.
// A changed rate (a new policy) takes effect at once: the bucket is clamped
// to the new size.
func (b *bucket) refill(now time.Time, r Rate) {
	b.rate = r
	if elapsed := now.Sub(b.last); elapsed > 0 {
		b.tokens += float64(r.N) * float64(elapsed) / float64(r.Per)
		b.last = now
	}
	if b.tokens > float64(r.N) {
		b.tokens = float64(r.N)
	}
}

// sweep drops the buckets that are full again. l.mu must be held.
func (l *Limiter) sweep(now time.Time) {
	l.calls = 0
	for k, b := range l.buckets {
		if now.Sub(b.last) >= b.rate.Per {
			delete(l.buckets, k)
		}
	}
}

// Len returns the number of buckets held, for tests.
func (l *Limiter) Len() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.buckets)
}
