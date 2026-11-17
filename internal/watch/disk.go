// Package watch holds Postwerk's own health signals: free space on the data
// volume (D50) and mails waiting too long per target (D49). Each check
// writes one event when its state changes and nothing while it stays the
// same; who is woken for an event is the monitoring tool's decision.
package watch

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/eneuberg/postwerk/internal/store"
)

// Event kinds of the free-space check.
const (
	KindDiskLow = "disk.low"
	KindDiskOK  = "disk.ok"
)

// DiskInterval is how often the free space is checked besides the checks
// ingress asks for. A failed event write or store probe is tried again at
// the next interval, never sooner.
const DiskInterval = 10 * time.Second

// failureReminder is how often a failure that lasts is logged again.
const failureReminder = 5 * time.Minute

// ErrNoStatfs: the operating system gives no free-space figure here.
var ErrNoStatfs = errors.New("free space cannot be read on this operating system")

// Store is what the checks need from the store.
type Store interface {
	Append(ctx context.Context, e store.Event) error
	LastOf(ctx context.Context, target string, kinds ...string) (store.Event, bool, error)
}

// DiskStore adds what the free-space check needs on top of Store.
type DiskStore interface {
	Store
	Writable() error
	Probe(ctx context.Context) error
}

// DiskOptions configure a Disk.
type DiskOptions struct {
	Store DiskStore
	// Dir is the directory of the database file.
	Dir string
	// MinFree is the reserve in bytes; 0 turns the check off.
	MinFree int64
	Logger  *slog.Logger
	Now     func() time.Time
	// Statfs replaces the system call in tests.
	Statfs func(dir string) (free, total uint64, err error)
}

// Disk watches the free space of the data volume. Below the reserve it
// refuses new mail (Allow), makes /readyz fail (Ready) and writes one
// disk.low event, while the reserve still has room for it; disk.ok when the
// free space is back above the reserve plus a quarter of it. The quarter
// keeps a volume that hovers at the line from flapping.
type Disk struct {
	opts   DiskOptions
	wake   chan struct{}
	mu     sync.Mutex
	low    bool   // current state
	logged string // the state the event log has: KindDiskLow, KindDiskOK or ""
	free   uint64
	total  uint64
	off    bool
	// Only Check uses these: the lasting failures it logs, so a full volume
	// gives one line per failure and a reminder, not one per check.
	probeFail  failure
	appendFail failure
}

// failure rate-limits the log line of a failure that lasts.
type failure struct {
	since, logged time.Time // zero while it works
}

// log writes the line when the failure starts and every failureReminder
// while it lasts.
func (f *failure) log(l *slog.Logger, now time.Time, msg string, err error) {
	if !f.since.IsZero() && now.Sub(f.logged) < failureReminder {
		return
	}
	if f.since.IsZero() {
		f.since = now
	}
	f.logged = now
	l.Error(msg, "err", err, "failing_since", f.since)
}

// ok ends a failure and reports whether there was one.
func (f *failure) ok() bool {
	was := !f.since.IsZero()
	*f = failure{}
	return was
}

// NewDisk returns a Disk. It reads the state the event log has, so a
// restart neither repeats disk.low nor loses disk.ok.
func NewDisk(ctx context.Context, opts DiskOptions) (*Disk, error) {
	if opts.Logger == nil {
		opts.Logger = slog.New(slog.DiscardHandler)
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.Statfs == nil {
		opts.Statfs = statfs
	}
	d := &Disk{opts: opts, wake: make(chan struct{}, 1), off: opts.MinFree <= 0}
	if d.off {
		return d, nil
	}
	if _, _, err := opts.Statfs(opts.Dir); errors.Is(err, ErrNoStatfs) {
		opts.Logger.Warn("free-space check off: " + err.Error() + "; watch the data volume yourself")
		d.off = true
		return d, nil
	}
	last, ok, err := opts.Store.LastOf(ctx, "", KindDiskLow, KindDiskOK)
	if err != nil {
		return nil, err
	}
	if ok {
		d.logged = last.Kind
		d.low = last.Kind == KindDiskLow
	}
	d.measure()
	return d, nil
}

// Allow reports whether a mail of size bytes may be taken: the volume is
// not below its reserve, and size more bytes would not take it there. It
// reads the free space at once, so a burst cannot outrun the interval.
func (d *Disk) Allow(size int64) bool {
	if d.off {
		return true
	}
	free, low := d.measure()
	if low {
		return false
	}
	return free >= uint64(d.opts.MinFree)+uint64(max(size, 0))
}

// Ready returns an error while the volume is below its reserve or the
// store's last write failed for lack of space or storage.
func (d *Disk) Ready() error {
	if err := d.opts.Store.Writable(); err != nil {
		return err
	}
	if d.off {
		return nil
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.low {
		return fmt.Errorf("data volume below its reserve: %d bytes free, reserve %d", d.free, d.opts.MinFree)
	}
	return nil
}

// measure reads the free space and updates the state. It returns the free
// bytes and whether the volume is low. A failed read keeps the state.
func (d *Disk) measure() (uint64, bool) {
	free, total, err := d.opts.Statfs(d.opts.Dir)
	d.mu.Lock()
	defer d.mu.Unlock()
	if err != nil {
		d.opts.Logger.Warn("free space of the data volume cannot be read", "err", err)
		return d.free, d.low
	}
	d.free, d.total = free, total
	reserve := uint64(d.opts.MinFree)
	switch {
	case !d.low && free < reserve:
		d.low = true
	case d.low && free >= reserve+reserve/4:
		d.low = false
	default:
		return free, d.low
	}
	// The state changed: Run writes the event now, not at the next tick.
	// Only a change wakes it; a write that failed waits for the tick, so a
	// full volume cannot turn Run into a loop.
	select {
	case d.wake <- struct{}{}:
	default:
	}
	return free, d.low
}

// state is the kind the event log should have; d.mu must be held.
func (d *Disk) state() string {
	if d.low {
		return KindDiskLow
	}
	return KindDiskOK
}

// Run checks every DiskInterval, and at once when Allow saw a change,
// until ctx ends.
func (d *Disk) Run(ctx context.Context) {
	if d.off {
		return
	}
	t := time.NewTicker(DiskInterval)
	defer t.Stop()
	for {
		d.Check(ctx)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		case <-d.wake:
		}
	}
}

// Check reads the free space, writes the event for a changed state and,
// after a failed write, probes whether the store can be written again.
func (d *Disk) Check(ctx context.Context) {
	if d.off {
		return
	}
	if d.opts.Store.Writable() != nil {
		if err := d.opts.Store.Probe(ctx); err != nil {
			d.probeFail.log(d.opts.Logger, d.opts.Now(), "the store cannot be written", err)
		} else {
			d.probeFail.ok()
			d.opts.Logger.Info("the store can be written again")
		}
	} else if d.probeFail.ok() {
		// Another write went through before the next probe.
		d.opts.Logger.Info("the store can be written again")
	}
	d.measure()
	d.mu.Lock()
	want, free, total := d.state(), d.free, d.total
	logged := d.logged
	d.mu.Unlock()
	if want == logged || (logged == "" && want == KindDiskOK) {
		// Nothing changed; a first start with enough room says nothing.
		d.mu.Lock()
		if d.logged == "" {
			d.logged = want
		}
		d.mu.Unlock()
		return
	}
	level, msg := store.Error, "data volume below its free-space reserve; new mail is refused (452)"
	if want == KindDiskOK {
		level, msg = store.Info, "data volume has room again; new mail is accepted"
	}
	text := fmt.Sprintf("free=%s total=%s reserve=%s", mib(free), mib(total), mib(uint64(d.opts.MinFree)))
	ectx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := d.opts.Store.Append(ectx, store.Event{Time: d.opts.Now(), Kind: want, Level: level, Text: text}); err != nil {
		d.appendFail.log(d.opts.Logger, d.opts.Now(), "writing the "+want+" event failed; trying again every "+
			DiskInterval.String(), err)
		return
	}
	if d.appendFail.ok() {
		d.opts.Logger.Info("the " + want + " event is written")
	}
	if level == store.Error {
		d.opts.Logger.Error(msg, "free_bytes", free, "reserve_bytes", d.opts.MinFree)
	} else {
		d.opts.Logger.Info(msg, "free_bytes", free, "reserve_bytes", d.opts.MinFree)
	}
	d.mu.Lock()
	d.logged = want
	d.mu.Unlock()
}

// mib writes a byte count in MiB, or KiB below 10 MiB, rounded down.
func mib(b uint64) string {
	if b < 10<<20 {
		return fmt.Sprintf("%dKiB", b>>10)
	}
	return fmt.Sprintf("%dMiB", b>>20)
}
