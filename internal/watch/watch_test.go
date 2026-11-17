package watch

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/eneuberg/postwerk/internal/store"
	"github.com/eneuberg/postwerk/internal/testutil"
)

var ctx = context.Background()

// fakeStore records events and answers LastOf from them.
type fakeStore struct {
	mu       sync.Mutex
	events   []store.Event
	queues   map[string]store.QueueStat
	failNext bool
	writeErr error
	probes   int
	// appendErr, if set, fails every Append, as a full volume does.
	appendErr error
	appends   int
}

func (f *fakeStore) Append(_ context.Context, e store.Event) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.appends++
	if f.appendErr != nil {
		return f.appendErr
	}
	if f.failNext {
		f.failNext = false
		return errors.New("disk full")
	}
	f.events = append(f.events, e)
	return nil
}

func (f *fakeStore) LastOf(_ context.Context, target string, kinds ...string) (store.Event, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i := len(f.events) - 1; i >= 0; i-- {
		e := f.events[i]
		for _, k := range kinds {
			if e.Kind == k && e.Target == target {
				return e, true, nil
			}
		}
	}
	return store.Event{}, false, nil
}

func (f *fakeStore) Queues(context.Context) (map[string]store.QueueStat, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.queues, nil
}

func (f *fakeStore) Writable() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.writeErr
}

func (f *fakeStore) Probe(context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.probes++
	return f.writeErr
}

// kinds returns "kind level target" per event.
func (f *fakeStore) kinds() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for _, e := range f.events {
		out = append(out, strings.TrimSpace(e.Kind+" "+string(e.Level)+" "+e.Target))
	}
	return out
}

func (f *fakeStore) last() store.Event {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.events[len(f.events)-1]
}

const mb = 1 << 20

type fakeFS struct{ free atomic.Int64 }

func (f *fakeFS) statfs(string) (uint64, uint64, error) { return uint64(f.free.Load()), 1024 * mb, nil }

func newDisk(t *testing.T, st *fakeStore, fs *fakeFS) *Disk {
	t.Helper()
	d, err := NewDisk(ctx, DiskOptions{Store: st, Dir: "/data", MinFree: 64 * mb, Statfs: fs.statfs})
	if err != nil {
		t.Fatal(err)
	}
	return d
}

func TestDiskLowAndOK(t *testing.T) {
	st, fs := &fakeStore{}, &fakeFS{}
	fs.free.Store(1024 * mb)
	d := newDisk(t, st, fs)
	d.Check(ctx)
	if k := st.kinds(); len(k) != 0 {
		t.Fatalf("a start with room wrote %q", k)
	}
	if !d.Allow(15*mb) || d.Ready() != nil {
		t.Fatal("refused with room")
	}

	// A mail that would take the volume below the reserve is refused,
	// without changing the state.
	fs.free.Store(70 * mb)
	if d.Allow(15*mb) || !d.Allow(1*mb) || d.Ready() != nil {
		t.Fatal("size not counted against the reserve")
	}

	// Below the reserve: refused at once, not ready, one disk.low.
	fs.free.Store(50 * mb)
	if d.Allow(0) {
		t.Fatal("allowed below the reserve")
	}
	if d.Ready() == nil {
		t.Fatal("ready below the reserve")
	}
	d.Check(ctx)
	d.Check(ctx)
	if k := st.kinds(); len(k) != 1 || k[0] != "disk.low error" {
		t.Fatalf("events = %q", k)
	}
	if txt := st.last().Text; txt != "free=50MiB total=1024MiB reserve=64MiB" {
		t.Fatalf("text = %q", txt)
	}

	// Just above the reserve is not enough: a quarter more is needed.
	fs.free.Store(70 * mb)
	if d.Allow(0) {
		t.Fatal("allowed within the hysteresis")
	}
	fs.free.Store(81 * mb)
	if !d.Allow(0) || d.Ready() != nil {
		t.Fatal("still refused with room again")
	}
	d.Check(ctx)
	if k := st.kinds(); len(k) != 2 || k[1] != "disk.ok info" {
		t.Fatalf("events = %q", k)
	}
}

// A restart picks up the state from the event log: no second disk.low
// while it stays low, and disk.ok once it recovers.
func TestDiskRestart(t *testing.T) {
	st, fs := &fakeStore{}, &fakeFS{}
	fs.free.Store(10 * mb)
	newDisk(t, st, fs).Check(ctx)
	d := newDisk(t, st, fs)
	if d.Allow(0) {
		t.Fatal("allowed after restart while low")
	}
	d.Check(ctx)
	if k := st.kinds(); len(k) != 1 {
		t.Fatalf("events after restart = %q", k)
	}
	fs.free.Store(500 * mb)
	newDisk(t, st, fs).Check(ctx)
	if k := st.kinds(); len(k) != 2 || k[1] != "disk.ok info" {
		t.Fatalf("events after recovery = %q", k)
	}
}

// A failed event write is tried again on the next check.
func TestDiskEventRetried(t *testing.T) {
	st, fs := &fakeStore{failNext: true}, &fakeFS{}
	fs.free.Store(10 * mb)
	d := newDisk(t, st, fs)
	d.Check(ctx)
	if k := st.kinds(); len(k) != 0 {
		t.Fatalf("events = %q", k)
	}
	d.Check(ctx)
	if k := st.kinds(); len(k) != 1 || k[0] != "disk.low error" {
		t.Fatalf("events = %q", k)
	}
}

// A store that cannot be written makes /readyz fail, and the check probes
// it until a write goes through.
func TestDiskStoreNotWritable(t *testing.T) {
	st, fs := &fakeStore{writeErr: errors.New("database or disk is full")}, &fakeFS{}
	fs.free.Store(500 * mb)
	d := newDisk(t, st, fs)
	if d.Ready() == nil {
		t.Fatal("ready with a failing store")
	}
	d.Check(ctx)
	if st.probes != 1 {
		t.Fatalf("probes = %d", st.probes)
	}
	st.mu.Lock()
	st.writeErr = nil
	st.mu.Unlock()
	d.Check(ctx)
	if st.probes != 1 || d.Ready() != nil {
		t.Fatalf("probes = %d, ready = %v", st.probes, d.Ready())
	}
}

// A volume that is full before disk.low could be written: Run tries the
// write and the probe again at the next interval, not in a loop, and ingress
// asking all the time does not speed it up.
func TestDiskFullDoesNotSpin(t *testing.T) {
	full := errors.New("database or disk is full")
	st, fs := &fakeStore{writeErr: full, appendErr: full}, &fakeFS{}
	fs.free.Store(0)
	logs := &testutil.SafeBuffer{}
	d, err := NewDisk(ctx, DiskOptions{Store: st, Dir: "/data", MinFree: 64 * mb, Statfs: fs.statfs,
		Logger: testutil.Logger(logs)})
	if err != nil {
		t.Fatal(err)
	}
	rctx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() { defer close(done); d.Run(rctx) }()
	for end := time.Now().Add(300 * time.Millisecond); time.Now().Before(end); {
		d.Allow(0) // ingress refusing mail
		time.Sleep(time.Millisecond)
	}
	cancel()
	<-done
	st.mu.Lock()
	appends, probes := st.appends, st.probes
	st.mu.Unlock()
	// The first check, and at most one more for the wake-up NewDisk left.
	if appends < 1 || appends > 2 || probes > 2 {
		t.Fatalf("%d appends and %d probes in 300 ms; want the next ones only after %s", appends, probes, DiskInterval)
	}
	for _, msg := range []string{"writing the disk.low event failed", "the store cannot be written"} {
		if n := strings.Count(logs.String(), msg); n != 1 {
			t.Fatalf("%q logged %d times, want once", msg, n)
		}
	}
}

// A lasting failure is logged when it starts and then every
// failureReminder; the recovery once.
func TestDiskFailureLogLimited(t *testing.T) {
	full := errors.New("database or disk is full")
	st, fs := &fakeStore{writeErr: full, appendErr: full}, &fakeFS{}
	fs.free.Store(0)
	now := time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC)
	logs := &testutil.SafeBuffer{}
	d, err := NewDisk(ctx, DiskOptions{Store: st, Dir: "/data", MinFree: 64 * mb, Statfs: fs.statfs,
		Logger: testutil.Logger(logs), Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	count := func(msg string) int { return strings.Count(logs.String(), msg) }
	for range 30 { // 5 minutes of checks, less one
		d.Check(ctx)
		now = now.Add(DiskInterval)
	}
	if a, p := count("writing the disk.low event failed"), count("the store cannot be written"); a != 1 || p != 1 {
		t.Fatalf("logged %d and %d times in 5 minutes, want once each", a, p)
	}
	now = now.Add(DiskInterval)
	d.Check(ctx)
	if a, p := count("writing the disk.low event failed"), count("the store cannot be written"); a != 2 || p != 2 {
		t.Fatalf("no reminder after 5 minutes: %d and %d", a, p)
	}
	st.mu.Lock()
	st.writeErr, st.appendErr = nil, nil
	st.mu.Unlock()
	d.Check(ctx)
	if k := st.kinds(); len(k) != 1 || k[0] != "disk.low error" {
		t.Fatalf("events = %q", k)
	}
	if count("the store can be written again") != 1 || count("the disk.low event is written") != 1 {
		t.Fatalf("recovery not logged once: %s", logs.String())
	}
}

func TestDiskOff(t *testing.T) {
	st := &fakeStore{}
	for _, opts := range []DiskOptions{
		{Store: st, MinFree: 0, Statfs: func(string) (uint64, uint64, error) { return 0, 0, nil }},
		{Store: st, MinFree: 64 * mb, Statfs: func(string) (uint64, uint64, error) { return 0, 0, ErrNoStatfs }},
	} {
		d, err := NewDisk(ctx, opts)
		if err != nil {
			t.Fatal(err)
		}
		d.Check(ctx)
		if !d.Allow(1<<40) || d.Ready() != nil || len(st.kinds()) != 0 {
			t.Fatal("check not off")
		}
	}
}

// statfs works on the test's own directory (Linux, macOS, FreeBSD).
func TestStatfs(t *testing.T) {
	free, total, err := statfs(t.TempDir())
	if errors.Is(err, ErrNoStatfs) {
		t.Skip(err)
	}
	if err != nil || total == 0 || free > total {
		t.Fatalf("statfs = %d, %d, %v", free, total, err)
	}
}

func TestQueueStale(t *testing.T) {
	now := time.Date(2026, 11, 16, 12, 0, 0, 0, time.UTC)
	clock := func() time.Time { return now }
	st := &fakeStore{queues: map[string]store.QueueStat{}}
	newQueue := func(repeat time.Duration) *Queue {
		q, err := NewQueue(ctx, QueueOptions{Store: st, After: 15 * time.Minute, Repeat: repeat, Now: clock,
			Targets: []Target{{Name: "mailjet"}, {Name: "capture", Optional: true}}})
		if err != nil {
			t.Fatal(err)
		}
		return q
	}
	q := newQueue(0)
	check := func() {
		t.Helper()
		if err := q.Check(ctx); err != nil {
			t.Fatal(err)
		}
	}
	st.queues["mailjet"] = store.QueueStat{Mails: 3, Oldest: now.Add(-10 * time.Minute)}
	check()
	if k := st.kinds(); len(k) != 0 {
		t.Fatalf("young queue wrote %q", k)
	}
	st.queues["mailjet"] = store.QueueStat{Mails: 12, Oldest: now.Add(-37 * time.Minute)}
	st.queues["capture"] = store.QueueStat{Mails: 1, Oldest: now.Add(-2 * time.Hour)}
	check()
	check()
	if k := st.kinds(); len(k) != 2 || k[0] != "queue.stale error mailjet" || k[1] != "queue.stale warn capture" {
		t.Fatalf("events = %q", k)
	}
	if txt := st.events[0].Text; txt != "queued=12 oldest=37m after=15m" {
		t.Fatalf("text = %q", txt)
	}
	// A restart does not repeat it.
	q = newQueue(0)
	check()
	if k := st.kinds(); len(k) != 2 {
		t.Fatalf("events after restart = %q", k)
	}
	// Back under the line: one queue.recovered.
	delete(st.queues, "mailjet")
	check()
	check()
	if k := st.kinds(); len(k) != 3 || k[2] != "queue.recovered info mailjet" {
		t.Fatalf("events = %q", k)
	}

	// With a reminder interval, queue.stale repeats while it stays stale.
	q = newQueue(time.Hour)
	now = now.Add(30 * time.Minute)
	check()
	if k := st.kinds(); len(k) != 3 {
		t.Fatalf("reminder before its time: %q", k)
	}
	now = now.Add(31 * time.Minute)
	check()
	if k := st.kinds(); len(k) != 4 || k[3] != "queue.stale warn capture" {
		t.Fatalf("no reminder: %q", k)
	}
}

// A repeat comes with the first check at or after its interval, although the
// checks drift by the time each one takes: with a 1m repeat, every check.
func TestQueueStaleRepeatEveryCheck(t *testing.T) {
	now := time.Date(2026, 11, 16, 12, 0, 0, 0, time.UTC)
	st := &fakeStore{queues: map[string]store.QueueStat{
		"mailjet": {Mails: 1, Oldest: now.Add(-time.Hour)},
	}}
	q, err := NewQueue(ctx, QueueOptions{Store: st, After: 15 * time.Minute, Repeat: time.Minute,
		Now: func() time.Time { return now }, Targets: []Target{{Name: "mailjet"}}})
	if err != nil {
		t.Fatal(err)
	}
	check := func() {
		t.Helper()
		if err := q.Check(ctx); err != nil {
			t.Fatal(err)
		}
	}
	check() // the first queue.stale
	// The ticks are a minute apart; each check reads the clock a little after
	// its tick, by a varying amount.
	delays := []time.Duration{40 * time.Millisecond, 3 * time.Millisecond, 250 * time.Millisecond, 0, 2 * time.Second}
	tick := now
	for i, d := range delays {
		tick = tick.Add(QueueInterval)
		now = tick.Add(d)
		check()
		if n := len(st.kinds()); n != i+2 {
			t.Fatalf("after check %d: %d events, want %d (a repeat every check): %q", i+1, n, i+2, st.kinds())
		}
	}
	// Not earlier than half an interval before it is due.
	q, err = NewQueue(ctx, QueueOptions{Store: st, After: 15 * time.Minute, Repeat: 5 * time.Minute,
		Now: func() time.Time { return now }, Targets: []Target{{Name: "mailjet"}}})
	if err != nil {
		t.Fatal(err)
	}
	before := len(st.kinds())
	now = now.Add(4*time.Minute + 29*time.Second)
	check()
	if n := len(st.kinds()); n != before {
		t.Fatalf("repeat after 4m29s of 5m: %q", st.kinds())
	}
	now = now.Add(2 * time.Second)
	check()
	if n := len(st.kinds()); n != before+1 {
		t.Fatalf("no repeat after 4m31s of 5m: %q", st.kinds())
	}
}

func TestAge(t *testing.T) {
	for d, want := range map[time.Duration]string{
		59 * time.Second:               "0m",
		37 * time.Minute:               "37m",
		2*time.Hour + 5*time.Minute:    "2h05m",
		3*24*time.Hour + 4*time.Hour:   "3d04h",
		24*time.Hour - time.Nanosecond: "23h59m",
	} {
		if got := Age(d); got != want {
			t.Errorf("Age(%s) = %q, want %q", d, got, want)
		}
	}
}

func TestMiB(t *testing.T) {
	for b, want := range map[uint64]string{360448: "352KiB", 512 << 10: "512KiB", 10 << 20: "10MiB", 64 << 20: "64MiB"} {
		if got := mib(b); got != want {
			t.Errorf("mib(%d) = %q, want %q", b, got, want)
		}
	}
}
