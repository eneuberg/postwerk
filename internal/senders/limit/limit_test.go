package limit

import (
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *clock) add(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

func TestAllow(t *testing.T) {
	perMinute := Rate{N: 3, Per: time.Minute}
	for _, tc := range []struct {
		name  string
		steps []struct {
			wait time.Duration
			want bool
		}
	}{
		{"burst then refused", []struct {
			wait time.Duration
			want bool
		}{{0, true}, {0, true}, {0, true}, {0, false}, {0, false}}},
		{"one token back after a third of the period", []struct {
			wait time.Duration
			want bool
		}{{0, true}, {0, true}, {0, true}, {0, false}, {20 * time.Second, true}, {0, false}}},
		{"full again after a period", []struct {
			wait time.Duration
			want bool
		}{{0, true}, {0, true}, {0, true}, {time.Hour, true}, {0, true}, {0, true}, {0, false}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := &clock{t: time.Unix(1_800_000_000, 0)}
			l := New(c.now)
			for i, s := range tc.steps {
				c.add(s.wait)
				if got := l.Allow("app", perMinute); got != s.want {
					t.Fatalf("step %d: Allow = %v, want %v", i, got, s.want)
				}
			}
		})
	}
}

func TestUnlimitedAndKeys(t *testing.T) {
	l := New(nil)
	for range 1000 {
		if !l.Allow("app", Rate{}) || !l.Allow("app", Rate{N: 5}) {
			t.Fatal("an unlimited rate refused")
		}
	}
	if l.Len() != 0 {
		t.Fatal("bucket kept for an unlimited rate")
	}
	one := Rate{N: 1, Per: time.Hour}
	if !l.Allow("a", one) || !l.Allow("b", one) || l.Allow("a", one) {
		t.Fatal("keys are not separate")
	}
}

func TestAllowN(t *testing.T) {
	l := New(func() time.Time { return time.Unix(0, 0) })
	r := Rate{N: 10, Per: time.Hour}
	if !l.AllowN("k", r, 7) || l.AllowN("k", r, 4) || !l.AllowN("k", r, 3) || l.AllowN("k", r, 1) {
		t.Fatal("AllowN does not take all or nothing")
	}
}

func TestRateChangeClamps(t *testing.T) {
	c := &clock{t: time.Unix(0, 0)}
	l := New(c.now)
	big := Rate{N: 100, Per: time.Minute}
	if !l.Allow("k", big) {
		t.Fatal("first refused")
	}
	small := Rate{N: 2, Per: time.Minute}
	if !l.Allow("k", small) || !l.Allow("k", small) || l.Allow("k", small) {
		t.Fatal("a lowered rate did not take effect at once")
	}
}

func TestSweepDropsIdleBuckets(t *testing.T) {
	c := &clock{t: time.Unix(0, 0)}
	l := New(c.now)
	r := Rate{N: 1, Per: time.Minute}
	for i := range sweepEvery - 1 {
		l.Allow("k"+strconv.Itoa(i), r)
	}
	c.add(2 * time.Minute)
	l.Allow("last", r)
	if n := l.Len(); n != 1 {
		t.Fatalf("buckets after sweep = %d, want 1", n)
	}
}

// Run with -race: many goroutines on one key never take more than N.
func TestConcurrentAllow(t *testing.T) {
	l := New(func() time.Time { return time.Unix(0, 0) })
	r := Rate{N: 50, Per: time.Hour}
	var allowed atomic.Int32
	var wg sync.WaitGroup
	for range 16 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 20 {
				if l.Allow("k", r) {
					allowed.Add(1)
				}
				l.Allow("other", r)
			}
		}()
	}
	wg.Wait()
	if got := allowed.Load(); got != 50 {
		t.Fatalf("allowed %d, want 50", got)
	}
}
