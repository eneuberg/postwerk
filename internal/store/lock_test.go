//go:build linux || darwin || freebsd || netbsd || openbsd || dragonfly

package store

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func fastLockPoll(t *testing.T) {
	t.Helper()
	poll, period := lockPoll, lockLogPeriod
	lockPoll, lockLogPeriod = 10*time.Millisecond, 50*time.Millisecond
	t.Cleanup(func() { lockPoll, lockLogPeriod = poll, period })
}

// A second store on the same database waits for the lock, reports that
// it waits, and gives up with ErrLocked after LockWait.
func TestLockSecondOpenWaitsThenFails(t *testing.T) {
	fastLockPoll(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "pw.db")
	first := open(t, dir, Options{})
	_ = first

	fi, err := os.Stat(LockPath(path))
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Fatalf("lock file mode %04o", fi.Mode().Perm())
	}

	var calls atomic.Int32
	var holder atomic.Value
	start := time.Now()
	s, err := Open(ctx, path, Options{LockWait: 300 * time.Millisecond, OnLockWait: func(_ time.Duration, h string) {
		calls.Add(1)
		holder.Store(h)
	}})
	if !errors.Is(err, ErrLocked) {
		if s != nil {
			s.Close()
		}
		t.Fatalf("second open: %v", err)
	}
	if d := time.Since(start); d < 300*time.Millisecond {
		t.Fatalf("gave up after %v", d)
	}
	if calls.Load() < 2 {
		t.Fatalf("OnLockWait called %d times", calls.Load())
	}
	want := "pid=" + strconv.Itoa(os.Getpid())
	if h, _ := holder.Load().(string); !strings.Contains(h, want) || !strings.Contains(err.Error(), want) {
		t.Fatalf("holder %q, error %q, want %q", h, err, want)
	}

	// Without a wait, it fails at once.
	start = time.Now()
	if _, err := Open(ctx, path, Options{}); !errors.Is(err, ErrLocked) || time.Since(start) > time.Second {
		t.Fatalf("open without wait: %v after %v", err, time.Since(start))
	}
}

// Close releases the lock: a store that waits gets it, and nothing was
// written to the database by the waiting one before.
func TestLockReleasedOnClose(t *testing.T) {
	fastLockPoll(t)
	path := filepath.Join(t.TempDir(), "pw.db")
	first, err := Open(ctx, path, Options{})
	if err != nil {
		t.Fatal(err)
	}
	waiting := make(chan struct{})
	var once sync.Once
	type result struct {
		s   *Store
		err error
	}
	got := make(chan result, 1)
	go func() {
		s, err := Open(ctx, path, Options{LockWait: 10 * time.Second, OnLockWait: func(time.Duration, string) {
			once.Do(func() { close(waiting) })
		}})
		got <- result{s, err}
	}()
	select {
	case <-waiting:
	case r := <-got:
		t.Fatalf("second open did not wait: %v", r.err)
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	r := <-got
	if r.err != nil {
		t.Fatal(r.err)
	}
	if err := r.s.Ping(ctx); err != nil {
		t.Fatal(err)
	}
	r.s.Close()
	// Closed twice is harmless, and the lock is free again.
	first.Close()
	s, err := Open(ctx, path, Options{})
	if err != nil {
		t.Fatal(err)
	}
	s.Close()
}

// A cancelled context ends the wait.
func TestLockWaitCancelled(t *testing.T) {
	fastLockPoll(t)
	dir := t.TempDir()
	open(t, dir, Options{})
	c, cancel := context.WithTimeout(ctx, 100*time.Millisecond)
	defer cancel()
	_, err := Open(c, filepath.Join(dir, "pw.db"), Options{LockWait: time.Minute})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v", err)
	}
}

// A failed Open releases the lock it took.
func TestLockReleasedOnFailedOpen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "pw.db")
	if err := os.WriteFile(path, []byte("not a database, long enough to be read as a header....................."), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(ctx, path, Options{}); err == nil {
		t.Fatal("opened a broken database")
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	s, err := Open(ctx, path, Options{})
	if err != nil {
		t.Fatalf("lock left behind by the failed open: %v", err)
	}
	s.Close()
}

// Many stores race for one database: at no time do two hold it, and each
// gets it in turn.
func TestLockRace(t *testing.T) {
	fastLockPoll(t)
	path := filepath.Join(t.TempDir(), "pw.db")
	const n = 8
	var holders, maxHolders, done atomic.Int32
	var wg sync.WaitGroup
	errs := make(chan error, n)
	for range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			s, err := Open(ctx, path, Options{LockWait: 30 * time.Second})
			if err != nil {
				errs <- err
				return
			}
			h := holders.Add(1)
			for {
				m := maxHolders.Load()
				if h <= m || maxHolders.CompareAndSwap(m, h) {
					break
				}
			}
			if err := s.Append(ctx, Event{Time: time.Now(), Kind: "test.lock", Level: Info, Text: "held"}); err != nil {
				errs <- err
			}
			time.Sleep(5 * time.Millisecond)
			holders.Add(-1)
			if err := s.Close(); err != nil {
				errs <- err
			}
			done.Add(1)
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
	if maxHolders.Load() != 1 || done.Load() != n {
		t.Fatalf("max holders %d, done %d", maxHolders.Load(), done.Load())
	}
}
