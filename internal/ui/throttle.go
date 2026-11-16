package ui

import (
	"sync"
	"time"
)

// Login throttling (U22). Failures are counted per typed name and per
// source address, in memory. After the free failures, every further one
// makes the key wait: 1 minute, then twice as long each time, at most 15
// minutes. With the account limits that is at most about 100 failed checks
// a day for one name, the upper bound of NIST SP 800-63B-4 (3.2.2). A key
// without a failure for its forget period starts over; a good login clears
// its name (not its source).
const (
	accountFreeFailures = 5
	accountForget       = 24 * time.Hour
	// Through kubectl port-forward every person comes from 127.0.0.1, so
	// the source limit is looser than the one per name.
	sourceFreeFailures = 20
	sourceForget       = time.Hour

	throttleBase = time.Minute
	throttleMax  = 15 * time.Minute
	// maxThrottleKeys bounds the memory; typed names are attacker input.
	maxThrottleKeys = 10_000
)

type throttle struct {
	mu     sync.Mutex
	now    func() time.Time
	free   int
	forget time.Duration
	m      map[string]*failures
}

type failures struct {
	n     int
	last  time.Time
	until time.Time
}

func newThrottle(now func() time.Time, free int, forget time.Duration) *throttle {
	return &throttle{now: now, free: free, forget: forget, m: make(map[string]*failures)}
}

// wait returns how long key must still wait before its next check; 0 if it
// may try now.
func (t *throttle) wait(key string) time.Duration {
	now := t.now()
	t.mu.Lock()
	defer t.mu.Unlock()
	f := t.m[key]
	if f == nil || now.Sub(f.last) >= t.forget {
		return 0
	}
	return max(f.until.Sub(now), 0)
}

// fail counts a failed check for key and returns the wait it starts; 0
// while the key is within its free failures.
func (t *throttle) fail(key string) time.Duration {
	now := t.now()
	t.mu.Lock()
	defer t.mu.Unlock()
	f := t.m[key]
	if f == nil || now.Sub(f.last) >= t.forget {
		if f == nil && len(t.m) >= maxThrottleKeys {
			t.evict(now)
		}
		f = &failures{}
		t.m[key] = f
	}
	f.n++
	f.last = now
	if f.n <= t.free {
		return 0
	}
	d := throttleMax
	if steps := f.n - t.free - 1; steps < 5 {
		d = min(throttleBase<<steps, throttleMax)
	}
	f.until = now.Add(d)
	return d
}

// reset forgets key.
func (t *throttle) reset(key string) {
	t.mu.Lock()
	delete(t.m, key)
	t.mu.Unlock()
}

// evict makes room: forgotten keys first, else the oldest one. t.mu is
// held.
func (t *throttle) evict(now time.Time) {
	var oldest string
	for k, f := range t.m {
		if now.Sub(f.last) >= t.forget {
			delete(t.m, k)
			continue
		}
		if oldest == "" || f.last.Before(t.m[oldest].last) {
			oldest = k
		}
	}
	if len(t.m) >= maxThrottleKeys && oldest != "" {
		delete(t.m, oldest)
	}
}
