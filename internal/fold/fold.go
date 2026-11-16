// Package fold counts repeated occurrences of the same thing and writes one
// record per key and window, with the count (D42, D51).
//
// The first occurrence of a key opens a window; later ones only count; when
// the window ends, write is called once with the key and the count. Close
// ends every open window at once (at shutdown, before the final export);
// after it, every occurrence is written at once. Nothing is dropped: every
// occurrence is counted. Callers keep the set of keys bounded, for example
// by mapping names that are not configured to one "unknown" key.
package fold

import (
	"cmp"
	"slices"
	"sync"
	"time"
)

// Counter folds occurrences per key. It is safe for concurrent use.
type Counter[K comparable] struct {
	window time.Duration
	write  func(key K, n int)

	mu     sync.Mutex
	open   map[K]*win
	next   uint64 // opening order, so Close writes in a stable order
	closed bool
}

type win struct {
	n     int
	order uint64
	timer *time.Timer
}

// New returns a Counter that calls write once per key and window. write
// runs outside the Counter's lock, on a timer's goroutine or in Close.
func New[K comparable](window time.Duration, write func(key K, n int)) *Counter[K] {
	return &Counter[K]{window: window, write: write, open: make(map[K]*win)}
}

// Add counts one occurrence of key.
func (c *Counter[K]) Add(key K) {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		c.write(key, 1)
		return
	}
	if w := c.open[key]; w != nil {
		w.n++
		c.mu.Unlock()
		return
	}
	c.next++
	w := &win{n: 1, order: c.next}
	w.timer = time.AfterFunc(c.window, func() { c.flush(key, w) })
	c.open[key] = w
	c.mu.Unlock()
}

// Open returns the number of open windows.
func (c *Counter[K]) Open() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.open)
}

// flush ends the window w of key, unless Close took it already.
func (c *Counter[K]) flush(key K, w *win) {
	c.mu.Lock()
	if c.open[key] != w {
		c.mu.Unlock()
		return
	}
	delete(c.open, key)
	n := w.n
	c.mu.Unlock()
	c.write(key, n)
}

// Close writes every open window at once, in the order they were opened.
// Later occurrences are written one by one.
func (c *Counter[K]) Close() {
	c.mu.Lock()
	c.closed = true
	type entry struct {
		key K
		w   *win
	}
	entries := make([]entry, 0, len(c.open))
	for k, w := range c.open {
		entries = append(entries, entry{k, w})
	}
	c.open = make(map[K]*win)
	c.mu.Unlock()
	slices.SortFunc(entries, func(a, b entry) int { return cmp.Compare(a.w.order, b.w.order) })
	for _, e := range entries {
		e.w.timer.Stop()
		c.write(e.key, e.w.n)
	}
}
