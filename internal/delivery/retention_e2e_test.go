package delivery

// End-to-end tests for encryption at rest, retention and pseudonymization.

import (
	"bytes"
	"context"
	"os"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/emersion/go-smtp"

	"github.com/eneuberg/postwerk/internal/addrhash"
	"github.com/eneuberg/postwerk/internal/config"
	"github.com/eneuberg/postwerk/internal/keys"
	"github.com/eneuberg/postwerk/internal/store"
	"github.com/eneuberg/postwerk/internal/testutil"
)

// bodyCanary appears only in the message body, never in an address, so a
// hit in the database file means the stored message is readable.
const bodyCanary = "body-canary-5d1e9b"

const canaryMsg = "From: Sender <noreply@example.test>\r\n" +
	"To: <" + canary + ">\r\n" +
	"Subject: Reset your password\r\n" +
	"\r\n" +
	"Your one-time code: " + bodyCanary + "\r\n"

var hashRe = regexp.MustCompile(`^[a-z0-9]{1,16}:[0-9a-f]{64}$`)

// fileBytes returns the database file and its WAL file.
func fileBytes(t *testing.T, dbPath string) map[string][]byte {
	t.Helper()
	out := map[string][]byte{}
	for _, p := range []string{dbPath, dbPath + "-wal"} {
		b, err := os.ReadFile(p)
		if err != nil && !os.IsNotExist(err) {
			t.Fatal(err)
		}
		out[p] = b
	}
	return out
}

func assertNotOnDisk(t *testing.T, dbPath string, needles ...string) {
	t.Helper()
	for p, b := range fileBytes(t, dbPath) {
		for _, n := range needles {
			if bytes.Contains(b, []byte(n)) {
				t.Errorf("%q found in %s", n, p)
			}
		}
	}
}

func TestRetentionLeavesOnlyHashes(t *testing.T) {
	ok := newTarget(t, plain, nil, "", "")
	dropping := newTarget(t, plain, nil, "", "")
	dropping.set(func(b *behavior) { b.drop = 100 })
	h := startPostwerk(t, []config.Target{
		target("ok", ok, plain),
		target("dropping", dropping, plain), // unknown under hold: the message stays stored
	}, harnessOpts{store: store.Options{PurgeRawOnHandoff: true}})

	mailID := h.send([]string{canary, "Second@Example.test"}, canaryMsg)
	h.waitStatus(mailID, "ok", store.HandedOff)
	h.waitStatus(mailID, "dropping", store.Unknown)
	h.stop()
	ctx := context.Background()

	// The message is still stored, but only encrypted: neither the database
	// file nor the WAL holds its body.
	rec, err := h.store.Mail(ctx, mailID)
	if err != nil || !rec.HasRaw {
		t.Fatalf("record = %+v, %v", rec, err)
	}
	if got := ok.got(); len(got) != 1 || !strings.Contains(string(got[0].data), bodyCanary) {
		t.Fatal("target did not get the plain message")
	}
	assertNotOnDisk(t, h.dbPath, bodyCanary, "Reset your password")

	// One retention run with tiny periods, as if a month had passed.
	ring, err := keys.Parse([]byte(keys.Generate(time.Now()) + "\n"))
	if err != nil {
		t.Fatal(err)
	}
	hasher := addrhash.New(ring)
	counts, err := h.store.Retain(ctx, store.RetainPolicy{
		RawKeepAfterFailure: time.Millisecond,
		RawMaxAge:           2 * time.Millisecond,
		Pseudonymize:        hasher.Hash,
		PseudonymizeAfter:   3 * time.Millisecond,
		RecordRetention:     time.Hour,
		EventRetention:      time.Hour,
	}, time.Now().Add(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	// Both recipients of the unknown delivery fail.
	if counts.RawPurged != 1 || counts.RawExpired != 2 || counts.Pseudonymized != 1 || counts.Records != 0 {
		t.Fatalf("counts = %+v", counts)
	}
	if d := h.delivery(mailID, "dropping"); d.Status != store.Failed {
		t.Fatalf("unknown delivery after purge = %+v", d)
	}

	// Only hashes remain in recipients and mails.
	rec, err = h.store.Mail(ctx, mailID)
	if err != nil {
		t.Fatal(err)
	}
	if rec.HasRaw || rec.RawPurgedAt.IsZero() || rec.PseudonymizedAt.IsZero() || !hashRe.MatchString(rec.EnvelopeFrom) {
		t.Fatalf("record = %+v", rec)
	}
	for _, r := range append(rec.Recipients, rec.EnvelopeFrom) {
		if !hashRe.MatchString(r) || strings.Contains(r, "@") {
			t.Fatalf("not a hash: %q", r)
		}
	}
	if rec.Recipients[1] != hasher.Hash("second@example.test") {
		t.Fatal("recipient hash does not follow the normalization")
	}

	// The mail is still found by address, in any case.
	n, hashes := hasher.Lookup(strings.ToUpper(canary))
	ids, err := h.store.MailIDsByAddress(ctx, n, hashes, 10)
	if err != nil || len(ids) != 1 || ids[0] != mailID {
		t.Fatalf("lookup = %v, %v", ids, err)
	}

	// Nothing personal is left on disk: not in the database file, not in
	// the WAL (checkpointed and truncated by the run).
	assertNotOnDisk(t, h.dbPath, canary, "canary-leak", "noreply@example.test", "Second@Example.test", bodyCanary)
	if b := fileBytes(t, h.dbPath)[h.dbPath+"-wal"]; len(b) != 0 {
		t.Logf("WAL holds %d bytes after the run (writes after the checkpoint)", len(b))
	}

	var purged bool
	evs, _ := h.store.EventsAfter(ctx, 0, 1000)
	for _, e := range evs {
		if e.Kind == store.KindRetentionPurged {
			purged = e.Text == "raw_purged=1 raw_expired=2 pseudonymized=1 records_deleted=0 events_deleted=0"
		}
	}
	if !purged {
		t.Fatalf("no retention.purged event with counts: %+v", evs)
	}
}

func TestRawPurgedOnHandoff(t *testing.T) {
	a := newTarget(t, plain, nil, "", "")
	b := newTarget(t, plain, nil, "", "")
	h := startPostwerk(t, []config.Target{target("a", a, plain), target("b", b, plain)},
		harnessOpts{store: store.Options{PurgeRawOnHandoff: true}})
	mailID := h.send([]string{canary}, testMsg)
	h.waitStatus(mailID, "a", store.HandedOff)
	h.waitStatus(mailID, "b", store.HandedOff)
	rec, err := h.store.Mail(context.Background(), mailID)
	if err != nil || rec.HasRaw || rec.RawPurgedAt.IsZero() || rec.Size != len(deliveredBody) {
		t.Fatalf("record = %+v, %v", rec, err)
	}

	// All targets skipped: the message is never stored.
	skipped := h.send([]string{canary}, "X-Postwerk-Skip: a, b\r\n"+testMsg)
	rec, err = h.store.Mail(context.Background(), skipped)
	if err != nil || rec.HasRaw || rec.RawPurgedAt.IsZero() {
		t.Fatalf("skipped record = %+v, %v", rec, err)
	}
}

func TestRawExpiredWhileQueued(t *testing.T) {
	busy := &smtp.SMTPError{Code: 451, EnhancedCode: smtp.EnhancedCode{4, 3, 0}, Message: "busy"}
	f := newTarget(t, plain, nil, "", "")
	f.set(func(b *behavior) { b.rcptErr = busy }) // every attempt is a retry
	h := startPostwerk(t, []config.Target{
		target("main", f, plain),
		target("extra", f, plain, func(t *config.Target) { t.Optional = true }),
	}, harnessOpts{})
	mailID := h.send([]string{canary}, testMsg)
	policy := store.RetainPolicy{
		RawKeepAfterFailure: time.Hour, RawMaxAge: time.Millisecond,
		RecordRetention: time.Hour, EventRetention: time.Hour,
		Optional: map[string]bool{"extra": true},
	}
	// Retention skips a mail while one of its deliveries is being sent, so
	// try until it hits a moment when both are queued.
	testutil.Eventually(t, timeout, "raw expired", func() bool {
		c, err := h.store.Retain(context.Background(), policy, time.Now().Add(time.Second))
		return err == nil && c.RawExpired == 2
	})
	levels := map[string]store.Level{}
	for _, e := range h.eventsOf(mailID) {
		if e.Kind == store.KindRawExpired {
			levels[e.Target] = e.Level
		}
	}
	if levels["main"] != store.Error || levels["extra"] != store.Warn {
		t.Fatalf("raw_expired levels = %v", levels)
	}
	for _, tg := range []string{"main", "extra"} {
		if d := h.delivery(mailID, tg); d.Status != store.Failed {
			t.Fatalf("%s = %+v", tg, d)
		}
	}
}

func TestDecryptFailedNeverSends(t *testing.T) {
	f := newTarget(t, plain, nil, "", "")
	// The workers hold a different key than the one the message was sealed
	// with, as after removing a key from the store key file too early.
	h := startPostwerk(t, []config.Target{target("t", f, plain)},
		harnessOpts{sealer: newSealer(t), workerSealer: newSealer(t)})
	mailID := h.send([]string{canary}, testMsg)
	d := h.waitStatus(mailID, "t", store.Failed)
	if d.Attempts != 1 {
		t.Fatalf("delivery = %+v", d)
	}
	time.Sleep(100 * time.Millisecond)
	if n := len(f.got()); n != 0 {
		t.Fatalf("target got %d messages", n)
	}
	var ev *store.Event
	for _, e := range h.eventsOf(mailID) {
		if e.Kind == store.KindDecryptFailed {
			ev = &e
		}
	}
	if ev == nil || ev.Level != store.Error || !strings.Contains(ev.Text, "is not in the store key file") {
		t.Fatalf("decrypt event = %+v", ev)
	}
}

func TestMailjetCustomIDReplaced(t *testing.T) {
	mj := newTarget(t, plain, nil, "", "")
	other := newTarget(t, plain, nil, "", "")
	h := startPostwerk(t, []config.Target{
		target("mailjet", mj, plain, func(t *config.Target) { t.Status = config.StatusMailjet }),
		target("other", other, plain),
	}, harnessOpts{})
	msg := "X-MJ-CustomID: chosen-by-sender\r\nx-mj-customid: second\r\n  folded\r\n" + testMsg
	mailID := h.send([]string{canary}, msg)
	h.waitStatus(mailID, "mailjet", store.HandedOff)
	h.waitStatus(mailID, "other", store.HandedOff)

	want := "X-Postwerk-Id: " + mailID + "\r\nX-MJ-CustomID: " + mailID + "\r\n" + deliveredBody
	if got := string(mj.got()[0].data); got != want {
		t.Fatalf("mailjet received:\n%q\nwant\n%q", got, want)
	}
	// Other targets get the sender's header unchanged (P5).
	wantOther := "X-Postwerk-Id: " + mailID + "\r\n" + strings.Replace(msg, "X-Postwerk-Ref: order-42\r\n", "", 1)
	if got := string(other.got()[0].data); got != wantOther {
		t.Fatalf("other received:\n%q\nwant\n%q", got, wantOther)
	}
}
