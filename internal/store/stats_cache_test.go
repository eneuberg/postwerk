package store

import (
	"fmt"
	"testing"
	"time"
)

// Hours without any count are cached too, and every distinct filter adds
// an entry per hour of the range (2160 for 90 days). The cache must bound
// the entries, not only the counts in them: else requests with ever new
// sender names, which need not exist, grow it without limit.
func TestStatsCacheBoundsEmptyEntries(t *testing.T) {
	var c statsCache
	for i := range StatsCacheCells + 1000 {
		c.clock++
		c.put(statsKey{chunk: int64(i), width: time.Hour.Milliseconds(), filter: fmt.Sprint(i % 500)}, nil)
	}
	if len(c.entries) > StatsCacheCells/statsEntryCells {
		t.Fatalf("the cache holds %d empty entries, more than its bound of %d", len(c.entries), StatsCacheCells/statsEntryCells)
	}
	sum := 0
	for _, e := range c.entries {
		sum += e.weight()
	}
	if sum != c.cells {
		t.Fatalf("weight %d, counted %d", sum, c.cells)
	}
}

// Through Stats: requests for senders that sent nothing stay bounded.
func TestStatsUnknownSendersStayBounded(t *testing.T) {
	s := open(t, t.TempDir(), Options{InsecureDelete: true})
	now := time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC)
	from := now.Add(-24 * time.Hour)
	for i := range 20 {
		f := StatsFilter{From: from, To: now, Width: time.Hour, Sender: fmt.Sprintf("nobody-%d", i), Now: now, Retained: from}
		if _, err := s.Stats(ctx, f); err != nil {
			t.Fatal(err)
		}
	}
	s.stats.mu.Lock()
	defer s.stats.mu.Unlock()
	if n := len(s.stats.entries); n != 20*24 || s.stats.cells != n*statsEntryCells {
		t.Fatalf("entries %d, weight %d; want %d entries", n, s.stats.cells, 20*24)
	}
}
