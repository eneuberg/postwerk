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
	// running counts the checks under way for the key. They count against
	// the free failures before they end, so a burst of requests at once
	// cannot run more checks than one at a time would.
	running int
}

func newThrottle(now func() time.Time, free int, forget time.Duration) *throttle {
	return &throttle{now: now, free: free, forget: forget, m: make(map[string]*failures)}
}

// begin reserves a check for key. It refuses while key must wait (wait >
// 0) and while other checks for key are running that could use up its
// last free failure (wait 0): past the free failures, checks for one key
// run one at a time, so each failure starts its wait before the next
// check. A reservation must be released with done.
func (t *throttle) begin(key string) (wait time.Duration, ok bool) {
	now := t.now()
	t.mu.Lock()
	defer t.mu.Unlock()
	f := t.entry(key, now)
	if f.until.After(now) {
		return f.until.Sub(now), false
	}
	if f.running > 0 && f.n+f.running >= t.free {
		return 0, false
	}
	f.running++
	return 0, true
}

// done releases a reservation of begin, after fail or reset.
func (t *throttle) done(key string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	f := t.m[key]
	if f == nil {
		return
	}
	if f.running > 0 {
		f.running--
	}
	if f.running == 0 && f.n == 0 {
		delete(t.m, key)
	}
}

// entry returns key's record, made new or started over after its forget
// period. t.mu is held.
func (t *throttle) entry(key string, now time.Time) *failures {
	f := t.m[key]
	if f == nil {
		if len(t.m) >= maxThrottleKeys {
			t.evict(now)
		}
		f = &failures{}
		t.m[key] = f
	} else if now.Sub(f.last) >= t.forget {
		f.n, f.until = 0, time.Time{}
	}
	return f
}

// fail counts a failed check for key and returns the wait it starts; 0
// while the key is within its free failures.
func (t *throttle) fail(key string) time.Duration {
	now := t.now()
	t.mu.Lock()
	defer t.mu.Unlock()
	f := t.entry(key, now)
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

// reset forgets key's failures; its running checks stay counted.
func (t *throttle) reset(key string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	f := t.m[key]
	switch {
	case f == nil:
	case f.running == 0:
		delete(t.m, key)
	default:
		f.n, f.until = 0, time.Time{}
	}
}

// evict makes room: forgotten keys first, else the oldest one. A key with
// a running check stays. t.mu is held.
func (t *throttle) evict(now time.Time) {
	var oldest string
	for k, f := range t.m {
		if f.running > 0 {
			continue
		}
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
