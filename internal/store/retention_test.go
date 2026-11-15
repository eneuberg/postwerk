package store

import (
	"strings"
	"testing"
	"time"
)

// fakeHash stands in for addrhash: the store only needs some function.
func fakeHash(a string) string {
	return "k1:" + strings.Repeat("0", 60) + string(rune('a'+len(a)%26)) + "fff"
}

func finish(t *testing.T, s *Store, id, target string, at time.Time, status Status) {
	t.Helper()
	c, err := s.Claim(ctx, target, at)
	if err != nil || c == nil || c.MailID != id {
		t.Fatalf("claim %s/%s = %+v, %v", id, target, c, err)
	}
	r := Result{MailID: id, Target: target, At: at, Attempt: c.Attempt}
	for _, rc := range c.Recipients {
		r.Recipients = append(r.Recipients, RecipientResult{Position: rc.Position, Status: status,
			Kind: KindDeliveryHandedOff, Level: Info})
	}
	if status == Queued {
		r.NextAttempt = at
	}
	if err := s.Finish(ctx, r); err != nil {
		t.Fatal(err)
	}
}

// set puts every recipient of a delivery into a state directly, as a
// finished attempt would.
func set(t *testing.T, s *Store, id, target string, status Status, at time.Time) {
	t.Helper()
	var next any
	if status == Queued {
		next = ms(at)
	}
	res, err := s.db.Exec(`UPDATE delivery_recipients SET status = ?, updated_at = ? WHERE mail_id = ? AND target = ?`,
		string(status), ms(at), id, target)
	if err != nil {
		t.Fatal(err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		t.Fatalf("no delivery %s/%s", id, target)
	}
	if _, err := s.db.Exec(`UPDATE deliveries SET next_attempt_at = ?, updated_at = ? WHERE mail_id = ? AND target = ?`,
		next, ms(at), id, target); err != nil {
		t.Fatal(err)
	}
}

func mail(t *testing.T, s *Store, id string) MailRecord {
	t.Helper()
	m, err := s.Mail(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func basePolicy() RetainPolicy {
	return RetainPolicy{
		RawKeepAfterHandoff: time.Hour, RawKeepAfterFailure: 72 * time.Hour, RawMaxAge: 7 * 24 * time.Hour,
		PseudonymizeAfter: 30 * 24 * time.Hour, Pseudonymize: fakeHash,
		RecordRetention: 90 * 24 * time.Hour, EventRetention: 90 * 24 * time.Hour,
	}
}

func TestEventWatermark(t *testing.T) {
	s := open(t, t.TempDir(), Options{})
	t0 := time.UnixMilli(1_000_000)
	for i, id := range []string{"pw_1", "pw_2", "pw_3"} {
		if err := s.Accept(ctx, newMail(id, t0.Add(time.Duration(i)*time.Hour), "a")); err != nil {
			t.Fatal(err)
		}
	}
	mustFail := func(q string, args ...any) {
		t.Helper()
		if _, err := s.db.Exec(q, args...); err == nil {
			t.Errorf("%s %v: accepted", q, args)
		}
	}
	// Cutoff 0: nothing can be deleted or updated.
	mustFail("DELETE FROM events")
	mustFail("DELETE FROM events WHERE seq = 1")
	mustFail("UPDATE events SET kind = 'x' WHERE seq = 1")

	// Raise the cutoff past the first event only.
	if _, err := s.db.Exec("UPDATE event_retention SET cutoff = ?", ms(t0.Add(time.Minute))); err != nil {
		t.Fatal(err)
	}
	mustFail("DELETE FROM events")                         // includes newer events: all or nothing
	mustFail("DELETE FROM events WHERE seq = 2")           // newer than the cutoff
	mustFail("UPDATE events SET text = 'x' WHERE seq = 1") // older, but updates stay forbidden
	if _, err := s.db.Exec("DELETE FROM events WHERE seq = 1"); err != nil {
		t.Fatalf("delete below the cutoff: %v", err)
	}

	// The cutoff cannot go backwards, the row cannot be removed, replaced or
	// doubled, and its id cannot change.
	mustFail("UPDATE event_retention SET cutoff = 0")
	mustFail("UPDATE event_retention SET cutoff = NULL")
	mustFail("UPDATE event_retention SET id = 2")
	mustFail("DELETE FROM event_retention")
	mustFail("INSERT OR REPLACE INTO event_retention (id, cutoff) VALUES (1, 0)")
	mustFail("INSERT INTO event_retention (id, cutoff) VALUES (2, 0)")
	mustFail("REPLACE INTO event_retention (id, cutoff) VALUES (1, 0)")
	if c, _ := s.EventCutoff(ctx); !c.Equal(t0.Add(time.Minute)) {
		t.Fatalf("cutoff = %v", c)
	}
	evs, _ := s.EventsAfter(ctx, 0, 10)
	if len(evs) != 2 || evs[0].Seq != 2 {
		t.Fatalf("events = %+v", evs)
	}
}

func TestRetainRawHandedOff(t *testing.T) {
	s := open(t, t.TempDir(), Options{})
	t0 := time.UnixMilli(10_000_000)
	if err := s.Accept(ctx, newMail("pw_1", t0, "a", "b")); err != nil {
		t.Fatal(err)
	}
	finish(t, s, "pw_1", "a", t0.Add(time.Minute), HandedOff)
	p := basePolicy()
	// One delivery still queued: kept.
	if c, err := s.Retain(ctx, p, t0.Add(2*time.Hour)); err != nil || c.RawPurged != 0 {
		t.Fatalf("counts = %+v, %v", c, err)
	}
	finish(t, s, "pw_1", "b", t0.Add(2*time.Hour), HandedOff)
	// Not yet an hour after the last hand-off.
	if c, _ := s.Retain(ctx, p, t0.Add(2*time.Hour+59*time.Minute)); c.RawPurged != 0 {
		t.Fatalf("purged too early: %+v", c)
	}
	c, err := s.Retain(ctx, p, t0.Add(3*time.Hour))
	if err != nil || c.RawPurged != 1 || c.RawExpired != 0 {
		t.Fatalf("counts = %+v, %v", c, err)
	}
	if m := mail(t, s, "pw_1"); m.HasRaw || !m.RawPurgedAt.Equal(t0.Add(3*time.Hour)) {
		t.Fatalf("mail = %+v", m)
	}
	evs, _ := s.EventsAfter(ctx, 0, 100)
	last := evs[len(evs)-1]
	if last.Kind != KindRetentionPurged || last.MailID != "" || last.Level != Info ||
		last.Text != "raw_purged=1 raw_expired=0 pseudonymized=0 records_deleted=0 events_deleted=0" {
		t.Fatalf("event = %+v", last)
	}
	// Nothing to do: no event.
	if c, _ := s.Retain(ctx, p, t0.Add(4*time.Hour)); c != (RetainCounts{}) {
		t.Fatalf("second run = %+v", c)
	}
	if evs2, _ := s.EventsAfter(ctx, 0, 100); len(evs2) != len(evs) {
		t.Fatal("event written for an empty run")
	}
}

func TestPurgeRawOnHandoff(t *testing.T) {
	s := open(t, t.TempDir(), Options{PurgeRawOnHandoff: true})
	t0 := time.UnixMilli(10_000_000)
	if err := s.Accept(ctx, newMail("pw_1", t0, "a", "b")); err != nil {
		t.Fatal(err)
	}
	finish(t, s, "pw_1", "a", t0, HandedOff)
	if !mail(t, s, "pw_1").HasRaw {
		t.Fatal("purged while b is queued")
	}
	finish(t, s, "pw_1", "b", t0.Add(time.Second), HandedOff)
	if m := mail(t, s, "pw_1"); m.HasRaw || !m.RawPurgedAt.Equal(t0.Add(time.Second)) {
		t.Fatalf("not purged in the hand-off transaction: %+v", m)
	}

	// A failure keeps the message for a resend.
	if err := s.Accept(ctx, newMail("pw_2", t0, "a")); err != nil {
		t.Fatal(err)
	}
	finish(t, s, "pw_2", "a", t0, Failed)
	if !mail(t, s, "pw_2").HasRaw {
		t.Fatal("purged after a failure")
	}

	// Every delivery skipped: never stored.
	m := newMail("pw_3", t0)
	m.Deliveries = []NewDelivery{{Target: "a", Status: Skipped, Kind: KindDeliverySkipped, Level: Info}}
	if err := s.Accept(ctx, m); err != nil {
		t.Fatal(err)
	}
	if rec := mail(t, s, "pw_3"); rec.HasRaw || rec.RawPurgedAt.IsZero() {
		t.Fatalf("skipped mail = %+v", rec)
	}
}

func TestRetainAfterFailureAndMaxAge(t *testing.T) {
	s := open(t, t.TempDir(), Options{})
	t0 := time.UnixMilli(10_000_000)
	for _, id := range []string{"pw_f", "pw_u", "pw_q", "pw_s"} {
		if err := s.Accept(ctx, newMail(id, t0, "a", "opt")); err != nil {
			t.Fatal(err)
		}
	}
	// pw_f: failed + handed off; pw_u: unknown + handed off; pw_q: both
	// queued; pw_s: one in sending.
	set(t, s, "pw_f", "a", Failed, t0)
	set(t, s, "pw_f", "opt", HandedOff, t0)
	set(t, s, "pw_u", "a", Unknown, t0)
	set(t, s, "pw_u", "opt", HandedOff, t0)
	set(t, s, "pw_q", "a", Queued, t0.Add(time.Hour))
	set(t, s, "pw_s", "a", HandedOff, t0)
	set(t, s, "pw_s", "opt", Sending, t0)

	p := basePolicy()
	p.Optional = map[string]bool{"opt": true}
	// 72 h after the failures: pw_f and pw_u are purged; the two unknown
	// recipients of pw_u fail. pw_q is still queued, pw_s still sending.
	cnt, err := s.Retain(ctx, p, t0.Add(72*time.Hour))
	if err != nil || cnt.RawPurged != 2 || cnt.RawExpired != 2 {
		t.Fatalf("after failure: %+v %v", cnt, err)
	}
	ds, _ := s.Deliveries(ctx, "pw_u")
	if ds[0].Status != Failed || ds[0].NextAttempt != (time.Time{}) {
		t.Fatalf("pw_u = %+v", ds)
	}
	// 7 days: pw_q is purged and its four queued recipients (two per
	// target) fail; pw_s is in sending and waits for the next run.
	cnt, err = s.Retain(ctx, p, t0.Add(7*24*time.Hour))
	if err != nil || cnt.RawPurged != 1 || cnt.RawExpired != 4 {
		t.Fatalf("max age: %+v %v", cnt, err)
	}
	if !mail(t, s, "pw_s").HasRaw {
		t.Fatal("purged while sending")
	}
	ds, _ = s.Deliveries(ctx, "pw_q")
	if ds[0].Status != Failed || ds[1].Status != Failed {
		t.Fatalf("pw_q = %+v", ds)
	}
	levels := map[string]Level{}
	evs, _ := s.EventsAfter(ctx, 0, 1000)
	for _, e := range evs {
		if e.Kind == KindRawExpired && e.MailID == "pw_q" {
			levels[e.Target] = e.Level
			if !strings.Contains(e.Text, "POSTWERK_RAW_MAX_AGE") {
				t.Fatalf("text = %q", e.Text)
			}
		}
	}
	if levels["a"] != Error || levels["opt"] != Warn {
		t.Fatalf("levels = %v", levels)
	}
	// A claim of a queued delivery whose message is gone returns no raw;
	// the worker fails it.
	if err := s.Accept(ctx, newMail("pw_x", t0, "z")); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec("UPDATE mails SET raw = NULL WHERE id = 'pw_x'"); err != nil {
		t.Fatal(err)
	}
	if c, err := s.Claim(ctx, "z", t0); err != nil || c == nil || c.Raw != nil {
		t.Fatalf("claim without raw = %+v %v", c, err)
	}
}

func TestRetainPseudonymizeAndDelete(t *testing.T) {
	s := open(t, t.TempDir(), Options{})
	day := 24 * time.Hour
	t0 := time.UnixMilli(100_000_000_000)
	old := newMail("pw_old", t0, "a")
	old.EnvelopeFrom = "Person@Example.test"
	nullSender := newMail("pw_null", t0, "a")
	nullSender.EnvelopeFrom = ""
	held := newMail("pw_held", t0, "a")
	recent := newMail("pw_new", t0.Add(20*day), "a")
	for _, m := range []NewMail{old, nullSender, held, recent} {
		if err := s.Accept(ctx, m); err != nil {
			t.Fatal(err)
		}
	}
	set(t, s, "pw_held", "a", Unknown, t0)
	set(t, s, "pw_new", "a", HandedOff, t0.Add(20*day))
	set(t, s, "pw_null", "a", HandedOff, t0)
	set(t, s, "pw_old", "a", HandedOff, t0)

	p := basePolicy()
	p.RawMaxAge, p.RawKeepAfterFailure = 100*day, 100*day // keep pw_held unknown, with its raw, past 30 days
	cnt, err := s.Retain(ctx, p, t0.Add(30*day))
	if err != nil || cnt.Pseudonymized != 2 {
		t.Fatalf("counts = %+v %v", cnt, err)
	}
	m := mail(t, s, "pw_old")
	if m.EnvelopeFrom != fakeHash("Person@Example.test") || m.Recipients[0] != fakeHash("a@example.test") ||
		m.PseudonymizedAt.IsZero() {
		t.Fatalf("pw_old = %+v", m)
	}
	if m := mail(t, s, "pw_null"); m.EnvelopeFrom != "" || m.PseudonymizedAt.IsZero() {
		t.Fatalf("null sender = %+v", m)
	}
	for _, id := range []string{"pw_held", "pw_new"} {
		if m := mail(t, s, id); !m.PseudonymizedAt.IsZero() || m.Recipients[0] != "a@example.test" {
			t.Fatalf("%s pseudonymized: %+v", id, m)
		}
	}
	// Idempotent: hashes are not hashed again.
	if cnt, _ := s.Retain(ctx, p, t0.Add(31*day)); cnt.Pseudonymized != 0 {
		t.Fatalf("second run = %+v", cnt)
	}

	// Lookup finds plain and hashed rows.
	ids, err := s.MailIDsByAddress(ctx, "a@example.test", []string{fakeHash("a@example.test"), "k0:none"}, 10)
	if err != nil || strings.Join(ids, ",") != "pw_old,pw_null,pw_new,pw_held" {
		t.Fatalf("lookup = %v %v", ids, err)
	}
	ids, _ = s.MailIDsByAddress(ctx, "person@example.test", []string{fakeHash("Person@Example.test")}, 10)
	if strings.Join(ids, ",") != "pw_old" {
		t.Fatalf("sender lookup = %v", ids)
	}
	// Without hashes, only rows that are still plain are found.
	ids, _ = s.MailIDsByAddress(ctx, "b@example.test", nil, 10)
	if strings.Join(ids, ",") != "pw_new,pw_held" {
		t.Fatalf("plain lookup, second recipient = %v", ids)
	}
	if ids, _ := s.MailIDsByAddress(ctx, "", nil, 10); ids != nil {
		t.Fatal("empty query matched")
	}

	// 90 days: records go, except the unknown one; events older than 90
	// days go; the cutoff row records it.
	cnt, err = s.Retain(ctx, p, t0.Add(90*day+time.Millisecond))
	if err != nil || cnt.Records != 2 || cnt.Events == 0 {
		t.Fatalf("delete: %+v %v", cnt, err)
	}
	for _, id := range []string{"pw_old", "pw_null"} {
		if _, err := s.Mail(ctx, id); err != ErrNotFound {
			t.Fatalf("%s: %v", id, err)
		}
		if ds, _ := s.Deliveries(ctx, id); len(ds) != 0 {
			t.Fatal("deliveries survived their mail")
		}
	}
	var n int
	s.db.QueryRow("SELECT COUNT(*) FROM recipients WHERE mail_id IN ('pw_old', 'pw_null')").Scan(&n)
	if n != 0 {
		t.Fatal("recipients survived their mail")
	}
	if _, err := s.Mail(ctx, "pw_held"); err != nil {
		t.Fatalf("unknown mail deleted: %v", err)
	}
	if c, _ := s.EventCutoff(ctx); !c.Equal(t0.Add(time.Millisecond)) {
		t.Fatalf("cutoff = %v", c)
	}
	evs, _ := s.EventsAfter(ctx, 0, 1000)
	for _, e := range evs {
		if e.Time.Before(t0.Add(time.Millisecond)) {
			t.Fatalf("old event left: %+v", e)
		}
	}
	// A later run with a shorter retention never lowers the cutoff.
	p.EventRetention = 200 * day
	if _, err := s.Retain(ctx, p, t0.Add(91*day)); err != nil {
		t.Fatal(err)
	}
	if c, _ := s.EventCutoff(ctx); !c.Equal(t0.Add(time.Millisecond)) {
		t.Fatalf("cutoff moved back: %v", c)
	}
}

func TestRetainBatches(t *testing.T) {
	s := open(t, t.TempDir(), Options{})
	t0 := time.UnixMilli(10_000_000)
	const n = batch*2 + 5
	for i := range n {
		if err := s.Accept(ctx, newMail("pw_"+strings.Repeat("0", 5-len(itoa(i)))+itoa(i), t0, "a")); err != nil {
			t.Fatal(err)
		}
	}
	p := basePolicy()
	cnt, err := s.Retain(ctx, p, t0.Add(7*24*time.Hour))
	if err != nil || cnt.RawPurged != n || cnt.RawExpired != 2*n { // two recipients each
		t.Fatalf("counts = %+v %v", cnt, err)
	}
	cnt, err = s.Retain(ctx, p, t0.Add(91*24*time.Hour))
	if err != nil || cnt.Pseudonymized != n || cnt.Records != n || cnt.Events != n {
		t.Fatalf("counts = %+v %v", cnt, err)
	}
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var b []byte
	for ; i > 0; i /= 10 {
		b = append([]byte{byte('0' + i%10)}, b...)
	}
	return string(b)
}

func TestSecureDeleteOff(t *testing.T) {
	s := open(t, t.TempDir(), Options{InsecureDelete: true})
	var v string
	if err := s.db.QueryRow("PRAGMA secure_delete").Scan(&v); err != nil || v != "0" {
		t.Fatalf("secure_delete = %q %v", v, err)
	}
}
