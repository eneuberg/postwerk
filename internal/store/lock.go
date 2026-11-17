package store

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"time"
)

// ErrLocked is returned by Open when another process holds the data lock
// for longer than Options.LockWait (D61).
var ErrLocked = errors.New("store: the database is in use by another process")

// Lock polling: how often Open tries again, and how often it reports that
// it is still waiting. Variables for the tests.
var (
	lockPoll      = 250 * time.Millisecond
	lockLogPeriod = 5 * time.Second
)

// LockPath is the lock file of the database at path: next to it, with
// ".lock" appended.
func LockPath(path string) string { return path + ".lock" }

// acquireLock takes the exclusive lock on the lock file of the database at
// path, waiting up to wait while another process holds it (D61). onWait,
// if set, is called at the first busy attempt and every lockLogPeriod
// after, with the time waited so far and what the holder wrote into the
// lock file. Without lock support on the platform it returns the open
// file and never waits.
func acquireLock(ctx context.Context, path string, wait time.Duration, onWait func(time.Duration, string)) (*os.File, error) {
	name := LockPath(path)
	f, err := os.OpenFile(name, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, fmt.Errorf("store: open lock file: %w", err)
	}
	start := time.Now()
	var lastLog time.Time
	for {
		busy, err := tryLock(f)
		if err != nil {
			f.Close()
			return nil, fmt.Errorf("store: lock %s: %w", name, err)
		}
		if !busy {
			break
		}
		waited := time.Since(start)
		if waited >= wait {
			holder := lockHolder(f)
			f.Close()
			return nil, fmt.Errorf("%w: %s is held by %s; waited %s (POSTWERK_DATA_LOCK_WAIT)",
				ErrLocked, name, holder, wait)
		}
		if onWait != nil && (lastLog.IsZero() || time.Since(lastLog) >= lockLogPeriod) {
			lastLog = time.Now()
			onWait(waited, lockHolder(f))
		}
		t := time.NewTimer(min(lockPoll, wait-waited))
		select {
		case <-ctx.Done():
			t.Stop()
			f.Close()
			return nil, fmt.Errorf("store: waiting for %s: %w", name, ctx.Err())
		case <-t.C:
		}
	}
	// What is written here is for the operator of the process that waits;
	// nothing reads it to decide anything. The lock itself is the kernel's
	// and ends with the process, however it ends.
	host, _ := os.Hostname()
	line := fmt.Sprintf("host=%s pid=%d since=%s\n", host, os.Getpid(), time.Now().UTC().Format(time.RFC3339))
	if err := f.Truncate(0); err == nil {
		_, _ = f.WriteAt([]byte(line), 0)
	}
	return f, nil
}

// lockHolder reads what the holder wrote into the lock file, for the log:
// one line, printable ASCII only, at most 200 bytes.
func lockHolder(f *os.File) string {
	buf := make([]byte, 200)
	n, err := f.ReadAt(buf, 0)
	if err != nil && !errors.Is(err, io.EOF) {
		return "another process"
	}
	line, _, _ := strings.Cut(string(buf[:n]), "\n")
	line = strings.Map(func(r rune) rune {
		if r < 0x20 || r > 0x7e {
			return -1
		}
		return r
	}, line)
	if line == "" {
		return "another process"
	}
	return strconv.Quote(line)
}
