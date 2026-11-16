package store

import (
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestQueues(t *testing.T) {
	s := open(t, t.TempDir(), Options{})
	t0 := time.UnixMilli(1_763_000_000_000)
	for i, id := range []string{"pw_1", "pw_2"} {
		if err := s.Accept(ctx, newMail(id, t0.Add(time.Duration(i)*time.Minute), "a", "b")); err != nil {
			t.Fatal(err)
		}
	}
	skipped := newMail("pw_3", t0.Add(-time.Hour), "a")
	skipped.Deliveries[0] = NewDelivery{Target: "a", Status: Skipped, Kind: KindDeliverySkipped, Level: Info}
	if err := s.Accept(ctx, skipped); err != nil {
		t.Fatal(err)
	}
	q, err := s.Queues(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(q) != 2 || q["a"].Mails != 2 || !q["a"].Oldest.Equal(t0) || q["b"].Mails != 2 {
		t.Fatalf("queues = %+v", q)
	}
}

func TestLastOf(t *testing.T) {
	s := open(t, t.TempDir(), Options{})
	at := time.UnixMilli(1_763_000_000_000)
	if _, ok, err := s.LastOf(ctx, "", "disk.low", "disk.ok"); ok || err != nil {
		t.Fatalf("empty log: %v %v", ok, err)
	}
	for _, e := range []Event{
		{Time: at, Kind: "disk.low", Level: Error},
		{Time: at, Kind: "queue.stale", Level: Error, Target: "a"},
		{Time: at, Kind: "disk.ok", Level: Info},
		{Time: at, Kind: "queue.recovered", Level: Info, Target: "b"},
	} {
		if err := s.Append(ctx, e); err != nil {
			t.Fatal(err)
		}
	}
	if e, ok, _ := s.LastOf(ctx, "", "disk.low", "disk.ok"); !ok || e.Kind != "disk.ok" {
		t.Fatalf("disk = %+v", e)
	}
	if e, ok, _ := s.LastOf(ctx, "a", "queue.stale", "queue.recovered"); !ok || e.Kind != "queue.stale" || !e.Time.Equal(at) {
		t.Fatalf("queue a = %+v", e)
	}
}

// A write that fails because the database is full is remembered until a
// write goes through; Probe is such a write. max_page_count stands in for
// a full disk: SQLite answers SQLITE_FULL either way.
func TestWritable(t *testing.T) {
	s := open(t, t.TempDir(), Options{})
	if err := s.Writable(); err != nil {
		t.Fatal(err)
	}
	var pages int
	if err := s.db.QueryRow("PRAGMA page_count").Scan(&pages); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec("PRAGMA max_page_count = " + strconv.Itoa(pages)); err != nil {
		t.Fatal(err)
	}
	big := strings.Repeat("x", 300)
	var err error
	for i := 0; i < 200 && err == nil; i++ {
		err = s.Append(ctx, Event{Time: time.Now(), Kind: "test.fill", Level: Info, Text: big})
	}
	if err == nil {
		t.Fatal("database never got full")
	}
	if s.Writable() == nil {
		t.Fatalf("not marked unwritable after %v", err)
	}
	// A constraint error says nothing about storage and changes nothing.
	if err := s.Append(ctx, Event{Time: time.Now(), Kind: "", Level: Info}); err == nil {
		t.Fatal("event without kind accepted")
	}
	if s.Writable() == nil {
		t.Fatal("cleared by an error that is not about storage")
	}
	if _, err := s.db.Exec("PRAGMA max_page_count = 1000000"); err != nil {
		t.Fatal(err)
	}
	if err := s.Probe(ctx); err != nil {
		t.Fatal(err)
	}
	if err := s.Writable(); err != nil {
		t.Fatalf("still unwritable after a good write: %v", err)
	}
}
