package filewatch

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

type recorder struct {
	mu      sync.Mutex
	current string
	loaded  int
	failed  []string
}

func (r *recorder) load(c [][]byte) error {
	s := string(c[0])
	if strings.HasPrefix(s, "bad") {
		return errors.New("refused " + s)
	}
	r.mu.Lock()
	r.current = s
	r.mu.Unlock()
	return nil
}

func (r *recorder) opts(path string) Options {
	return Options{
		Files: []string{path}, Load: r.load, Settle: time.Millisecond,
		Loaded: func() { r.mu.Lock(); r.loaded++; r.mu.Unlock() },
		Failed: func(err error) { r.mu.Lock(); r.failed = append(r.failed, err.Error()); r.mu.Unlock() },
	}
}

func write(t *testing.T, path, s string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(s), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestCheck(t *testing.T) {
	path := filepath.Join(t.TempDir(), "f")
	write(t, path, "one")
	r := &recorder{}
	w, err := New(r.opts(path))
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	steps := []struct {
		content     string // "" removes the file
		wantChanged bool
		wantCurrent string
		wantFailed  int
	}{
		{"one", false, "one", 0},
		{"two", true, "two", 0},
		{"bad one", false, "two", 1},
		{"bad one", false, "two", 1}, // reported once per content
		{"bad two", false, "two", 2},
		{"", false, "two", 3},
		{"", false, "two", 3},
		{"three", true, "three", 3},
		{"bad one", false, "three", 4}, // the same broken content after a good one is new again
	}
	for i, s := range steps {
		if s.content == "" {
			os.Remove(path)
		} else {
			write(t, path, s.content)
		}
		if got := w.Check(ctx); got != s.wantChanged {
			t.Fatalf("step %d: changed = %v", i, got)
		}
		r.mu.Lock()
		cur, failed := r.current, len(r.failed)
		r.mu.Unlock()
		if cur != s.wantCurrent || failed != s.wantFailed {
			t.Fatalf("step %d: current %q failed %d, want %q %d", i, cur, failed, s.wantCurrent, s.wantFailed)
		}
	}
	if r.loaded != 2 {
		t.Fatalf("loaded = %d", r.loaded)
	}
}

func TestNewFailsOnBadContent(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "f")
	r := &recorder{}
	if _, err := New(r.opts(path)); err == nil {
		t.Fatal("missing file accepted")
	}
	write(t, path, "bad")
	if _, err := New(r.opts(path)); err == nil {
		t.Fatal("refused content accepted")
	}
	write(t, path, strings.Repeat("x", 11))
	o := r.opts(path)
	o.MaxBytes = 10
	if _, err := New(o); err == nil || !strings.Contains(err.Error(), "larger than") {
		t.Fatalf("oversized file: %v", err)
	}
	if _, err := New(Options{}); err == nil {
		t.Fatal("empty options accepted")
	}
}

// A refused first read is read again after Settle: the second read may see
// the swap completed.
func TestSettleRereads(t *testing.T) {
	path := filepath.Join(t.TempDir(), "f")
	write(t, path, "one")
	r := &recorder{}
	o := r.opts(path)
	o.Settle = 50 * time.Millisecond
	w, err := New(o)
	if err != nil {
		t.Fatal(err)
	}
	write(t, path, "bad half")
	go func() {
		time.Sleep(10 * time.Millisecond)
		_ = os.WriteFile(path, []byte("complete"), 0o600)
	}()
	if !w.Check(context.Background()) || r.current != "complete" || len(r.failed) != 0 {
		t.Fatalf("current %q failed %v", r.current, r.failed)
	}
}

func TestRunStops(t *testing.T) {
	path := filepath.Join(t.TempDir(), "f")
	write(t, path, "one")
	r := &recorder{}
	o := r.opts(path)
	o.Interval = 5 * time.Millisecond
	w, err := New(o)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { w.Run(ctx); close(done) }()
	write(t, path, "two")
	deadline := time.Now().Add(5 * time.Second)
	for {
		r.mu.Lock()
		cur := r.current
		r.mu.Unlock()
		if cur == "two" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("Run did not load the change")
		}
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	<-done
}
