package store

import (
	"context"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

var ctx = context.Background()

func open(t *testing.T, dir string, opts Options) *Store {
	t.Helper()
	s, err := Open(ctx, filepath.Join(dir, "pw.db"), opts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func newMail(id string, at time.Time, targets ...string) NewMail {
	m := NewMail{
		ID: id, ReceivedAt: at, Sender: "app", EnvelopeFrom: "from@example.test",
		Recipients: []string{"a@example.test", "b@example.test"}, MessageID: "<m@x>", Ref: "r1",
		Raw: []byte("Subject: x\r\n\r\nbody\r\n"),
	}
	for _, t := range targets {
		m.Deliveries = append(m.Deliveries, NewDelivery{Target: t, Status: Queued})
	}
	return m
}

func TestOpenPragmasAndPermissions(t *testing.T) {
	dir := t.TempDir()
	s := open(t, dir, Options{})
	files, _ := fs.Glob(migrationFS, "migrations/*.sql")
	for name, want := range map[string]string{
		"journal_mode": "wal", "synchronous": "2", "foreign_keys": "1",
		"secure_delete": "1", "temp_store": "2", "user_version": strconv.Itoa(len(files)),
	} {
		var got string
		if err := s.db.QueryRow("PRAGMA " + name).Scan(&got); err != nil {
			t.Fatal(err)
		}
		if strings.ToLower(got) != want {
			t.Errorf("%s = %q, want %q", name, got, want)
		}
	}
	fi, err := os.Stat(filepath.Join(dir, "pw.db"))
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Errorf("mode = %v, want 0600", fi.Mode().Perm())
	}
	if s.db.Stats().MaxOpenConnections != 1 {
		t.Errorf("max open conns = %d", s.db.Stats().MaxOpenConnections)
	}
}

func TestMigrateIdempotent(t *testing.T) {
	dir := t.TempDir()
	s := open(t, dir, Options{})
	if err := s.Accept(ctx, newMail("pw_1", time.Now(), "a")); err != nil {
		t.Fatal(err)
	}
	s.Close()
	s2 := open(t, dir, Options{})
	if err := s2.migrate(ctx); err != nil {
		t.Fatal(err)
	}
	ds, err := s2.Deliveries(ctx, "pw_1")
	if err != nil || len(ds) != 1 {
		t.Fatalf("data lost after reopen: %v %v", ds, err)
	}
}

func TestNewerSchemaRefused(t *testing.T) {
	dir := t.TempDir()
	s := open(t, dir, Options{})
	if _, err := s.db.Exec("PRAGMA user_version = 99"); err != nil {
		t.Fatal(err)
	}
	s.Close()
	if _, err := Open(ctx, filepath.Join(dir, "pw.db"), Options{}); err == nil || !strings.Contains(err.Error(), "newer") {
		t.Fatalf("err = %v", err)
	}
}

func TestInvalidPath(t *testing.T) {
	for _, p := range []string{"", "a?b", "file:x.db", "a#b"} {
		if _, err := Open(ctx, p, Options{}); err == nil {
			t.Errorf("Open(%q) accepted", p)
		}
	}
}

func TestEventsAppendOnly(t *testing.T) {
	s := open(t, t.TempDir(), Options{})
	if err := s.Accept(ctx, newMail("pw_1", time.Now(), "a")); err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{
		"UPDATE events SET kind = 'x'",
		"UPDATE events SET text = 'x' WHERE seq = 1",
		"DELETE FROM events",
		"DELETE FROM events WHERE seq = 1",
	} {
		_, err := s.db.Exec(q)
		if err == nil || !strings.Contains(err.Error(), "append-only") {
			t.Errorf("%s: err = %v", q, err)
		}
	}
	evs, _ := s.EventsAfter(ctx, 0, 10)
	if len(evs) != 1 || evs[0].Kind != KindMailAccepted {
		t.Fatalf("events changed: %+v", evs)
	}
}

func TestAcceptAtomic(t *testing.T) {
	s := open(t, t.TempDir(), Options{})
	m := newMail("pw_1", time.Now(), "a")
	m.Deliveries = append(m.Deliveries, NewDelivery{Target: "b", Status: Sending}) // invalid at accept
	if err := s.Accept(ctx, m); err == nil {
		t.Fatal("invalid delivery accepted")
	}
	for _, table := range []string{"mails", "recipients", "deliveries", "events"} {
		var n int
		if err := s.db.QueryRow("SELECT COUNT(*) FROM " + table).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n != 0 {
			t.Errorf("%s has %d rows after a failed accept", table, n)
		}
	}

	// A duplicate id fails as a whole as well.
	if err := s.Accept(ctx, newMail("pw_1", time.Now(), "a")); err != nil {
		t.Fatal(err)
	}
	if err := s.Accept(ctx, newMail("pw_1", time.Now(), "a")); err == nil {
		t.Fatal("duplicate accepted")
	}
	evs, _ := s.EventsAfter(ctx, 0, 10)
	if len(evs) != 1 {
		t.Fatalf("events = %d, want 1", len(evs))
	}
}

func TestAcceptSkippedAndEvents(t *testing.T) {
	var hook atomic.Int32
	s := open(t, t.TempDir(), Options{OnEvent: func() { hook.Add(1) }})
	m := newMail("pw_1", time.UnixMilli(1000), "a")
	m.Deliveries = append(m.Deliveries,
		NewDelivery{Target: "b", Status: Skipped, Kind: KindDeliveryNotAllowed, Level: Warn, Text: "rcpt x@y.test not allowed"})
	if err := s.Accept(ctx, m); err != nil {
		t.Fatal(err)
	}
	if hook.Load() != 1 {
		t.Errorf("OnEvent called %d times", hook.Load())
	}
	ds, _ := s.Deliveries(ctx, "pw_1")
	if len(ds) != 2 || ds[0].Status != Queued || ds[1].Status != Skipped || !ds[1].NextAttempt.IsZero() {
		t.Fatalf("deliveries = %+v", ds)
	}
	evs, _ := s.EventsAfter(ctx, 0, 10)
	if len(evs) != 2 || evs[1].Kind != KindDeliveryNotAllowed || evs[1].Level != Warn || evs[1].Target != "b" ||
		evs[1].Sender != "app" || evs[1].Text != "rcpt <redacted> not allowed" {
		t.Fatalf("events = %+v", evs)
	}
}

func TestClaimFinishCycle(t *testing.T) {
	s := open(t, t.TempDir(), Options{})
	t0 := time.UnixMilli(1_000_000)
	if err := s.Accept(ctx, newMail("pw_2", t0.Add(time.Millisecond), "a", "b")); err != nil {
		t.Fatal(err)
	}
	if err := s.Accept(ctx, newMail("pw_1", t0, "a")); err != nil {
		t.Fatal(err)
	}

	if c, err := s.Claim(ctx, "a", t0.Add(-time.Second)); err != nil || c != nil {
		t.Fatalf("claimed before due: %+v %v", c, err)
	}
	c, err := s.Claim(ctx, "a", t0.Add(time.Second))
	if err != nil || c == nil {
		t.Fatal(c, err)
	}
	if c.MailID != "pw_1" || c.Attempt != 1 || c.EnvelopeFrom != "from@example.test" ||
		len(c.Recipients) != 2 || c.Recipients[0] != "a@example.test" || string(c.Raw) != "Subject: x\r\n\r\nbody\r\n" ||
		!c.ReceivedAt.Equal(t0) {
		t.Fatalf("claimed = %+v", c)
	}

	if err := s.MarkDataSent(ctx, "pw_1", "a", t0.Add(2*time.Second)); err != nil {
		t.Fatal(err)
	}
	st, _ := s.Sending(ctx, "a")
	if len(st) != 1 || !st[0].DataSent || st[0].Attempt != 1 {
		t.Fatalf("sending = %+v", st)
	}

	next := t0.Add(time.Minute)
	if err := s.Finish(ctx, Result{
		MailID: "pw_1", Target: "a", At: t0.Add(3 * time.Second), Attempt: 1, Status: Queued, NextAttempt: next,
		Code: 451, Text: "4.3.0 try later <a@example.test>", Kind: KindDeliveryRetry, Level: Warn,
	}); err != nil {
		t.Fatal(err)
	}
	if due, ok, _ := s.NextDue(ctx, "a"); !ok || !due.Equal(t0.Add(time.Millisecond)) {
		t.Fatalf("next due = %v %v", due, ok)
	}
	ds, _ := s.Deliveries(ctx, "pw_1")
	if ds[0].Status != Queued || !ds[0].NextAttempt.Equal(next) || ds[0].LastCode != 451 ||
		ds[0].LastText != "4.3.0 try later <redacted>" || ds[0].Attempts != 1 {
		t.Fatalf("delivery = %+v", ds[0])
	}

	// Finish of a delivery that is not sending is refused.
	if err := s.Finish(ctx, Result{MailID: "pw_1", Target: "a", At: t0, Status: Failed, Kind: KindDeliveryFailed, Level: Error}); err == nil {
		t.Fatal("finish of a queued delivery accepted")
	}
	if err := s.MarkDataSent(ctx, "pw_1", "a", t0); err == nil {
		t.Fatal("data sent on a queued delivery accepted")
	}

	// Second attempt resets data_sent_at.
	c, _ = s.Claim(ctx, "a", t0.Add(time.Second))
	if c == nil || c.MailID != "pw_2" {
		t.Fatalf("claimed = %+v", c)
	}
	c, _ = s.Claim(ctx, "a", next)
	if c == nil || c.MailID != "pw_1" || c.Attempt != 2 {
		t.Fatalf("claimed = %+v", c)
	}
	st, _ = s.Sending(ctx, "a")
	for _, x := range st {
		if x.DataSent {
			t.Fatalf("data_sent_at survived a new claim: %+v", st)
		}
	}

	pending, _ := s.PendingByTarget(ctx)
	if pending["a"] != 2 || pending["b"] != 1 {
		t.Fatalf("pending = %v", pending)
	}

	evs, _ := s.EventsAfter(ctx, 0, 100)
	var kinds []string
	for _, e := range evs {
		kinds = append(kinds, e.Kind+"/"+e.Target)
	}
	want := "mail.accepted/ mail.accepted/ delivery.attempt/a delivery.retry/a delivery.attempt/a delivery.attempt/a"
	if strings.Join(kinds, " ") != want {
		t.Fatalf("events:\n%s\nwant\n%s", strings.Join(kinds, " "), want)
	}
	if evs[3].Code != 451 || evs[3].Attempt != 1 || strings.Contains(evs[3].Text, "@") {
		t.Fatalf("retry event = %+v", evs[3])
	}
	more, _ := s.EventsAfter(ctx, evs[3].Seq, 100)
	if len(more) != 2 {
		t.Fatalf("events after %d = %d", evs[3].Seq, len(more))
	}
}

func TestCursor(t *testing.T) {
	s := open(t, t.TempDir(), Options{})
	if c, err := s.Cursor(ctx, "stdout"); err != nil || c != 0 {
		t.Fatal(c, err)
	}
	for _, v := range []int64{5, 9} {
		if err := s.SetCursor(ctx, "stdout", v); err != nil {
			t.Fatal(err)
		}
		if c, _ := s.Cursor(ctx, "stdout"); c != v {
			t.Fatalf("cursor = %d, want %d", c, v)
		}
	}
}

func TestClosedStoreFails(t *testing.T) {
	s := open(t, t.TempDir(), Options{})
	s.Close()
	if err := s.Accept(ctx, newMail("pw_1", time.Now(), "a")); err == nil {
		t.Fatal("accept on closed store succeeded")
	}
	if err := s.Ping(ctx); err == nil {
		t.Fatal("ping on closed store succeeded")
	}
}

func TestParamsStored(t *testing.T) {
	s := open(t, t.TempDir(), Options{})
	now := time.Now()
	m := newMail("pw_params", now, "t")
	m.Params = Params{Body: Body8BitMIME, SMTPUTF8: true, DeclaredSize: 1234}
	if err := s.Accept(ctx, m); err != nil {
		t.Fatal(err)
	}
	rec, err := s.Mail(ctx, "pw_params")
	if err != nil {
		t.Fatal(err)
	}
	if rec.Params != m.Params {
		t.Fatalf("stored params = %+v", rec.Params)
	}
	c, err := s.Claim(ctx, "t", now)
	if err != nil || c == nil {
		t.Fatalf("claim: %v", err)
	}
	if c.Params != m.Params {
		t.Fatalf("claimed params = %+v", c.Params)
	}

	bad := newMail("pw_badbody", now, "t")
	bad.Params.Body = "BINARYMIME"
	if err := s.Accept(ctx, bad); err == nil {
		t.Fatal("BODY=BINARYMIME was stored")
	}
}
