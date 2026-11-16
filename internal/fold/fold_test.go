package fold

import (
	"fmt"
	"slices"
	"sync"
	"testing"
	"time"
)

type sink struct {
	mu  sync.Mutex
	out []string
}

func (s *sink) write(k string, n int) {
	s.mu.Lock()
	s.out = append(s.out, fmt.Sprintf("%s=%d", k, n))
	s.mu.Unlock()
}

func (s *sink) get() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.out)
}

func TestWindowEnds(t *testing.T) {
	s := &sink{}
	c := New(50*time.Millisecond, s.write)
	for range 5 {
		c.Add("a")
	}
	c.Add("b")
	deadline := time.Now().Add(5 * time.Second)
	for len(s.get()) < 2 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	got := s.get()
	slices.Sort(got)
	if !slices.Equal(got, []string{"a=5", "b=1"}) {
		t.Fatalf("written = %q", got)
	}
	if c.Open() != 0 {
		t.Fatalf("open = %d", c.Open())
	}
}

func TestCloseInOpeningOrder(t *testing.T) {
	s := &sink{}
	c := New(time.Hour, s.write)
	for _, k := range []string{"z", "a", "z", "m"} {
		c.Add(k)
	}
	c.Close()
	if got := s.get(); !slices.Equal(got, []string{"z=2", "a=1", "m=1"}) {
		t.Fatalf("written at close = %q", got)
	}
	c.Add("a") // after Close: at once
	if got := s.get(); len(got) != 4 || got[3] != "a=1" {
		t.Fatalf("after close = %q", got)
	}
}
