package store

import (
	"errors"
	"slices"
	"strings"
	"testing"
	"time"
)

// statuses returns the stored status and resolution of every recipient of
// one delivery, as "status" or "status/resolution".
func statuses(t *testing.T, s *Store, id, target string) []string {
	t.Helper()
	ds, err := s.Deliveries(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range ds {
		if d.Target != target {
			continue
		}
		var out []string
		for _, r := range d.Recipients {
			v := string(r.Status)
			if r.Resolution != "" {
				v += "/" + string(r.Resolution)
			}
			out = append(out, v)
		}
		return out
	}
	t.Fatalf("no delivery %s/%s", id, target)
	return nil
}

func kindsOf(t *testing.T, s *Store, kinds ...string) []Event {
	t.Helper()
	evs, err := s.EventsAfter(ctx, 0, 1000)
	if err != nil {
		t.Fatal(err)
	}
	var out []Event
	for _, e := range evs {
		if slices.Contains(kinds, e.Kind) {
			out = append(out, e)
		}
	}
	return out
}

func TestCloseTransitions(t *testing.T) {
	at := time.Date(2026, 11, 15, 12, 0, 0, 0, time.UTC)
	for _, c := range []struct {
		name  string
		from  Status
		res   Resolution
		want  string
		err   error
		after string // derived summary of the delivery
	}{
		{"unknown confirmed", Unknown, Confirmed, "handed_off/confirmed", nil, "handed_off"},
		{"unknown given up", Unknown, GivenUp, "failed/given_up", nil, "failed"},
		{"failed confirmed", Failed, Confirmed, "handed_off/confirmed", nil, "handed_off"},
		{"failed given up", Failed, GivenUp, "failed/given_up", nil, "failed"},
		{"queued given up", Queued, GivenUp, "failed/given_up", nil, "failed"},
		{"queued confirmed", Queued, Confirmed, "", ErrConfirmQueued, ""},
		{"sending", Sending, GivenUp, "", ErrInProgress, ""},
		{"handed off", HandedOff, GivenUp, "", ErrNothingToClose, ""},
		{"skipped", Skipped, GivenUp, "", ErrNothingToClose, ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			s := open(t, t.TempDir(), Options{})
			if err := s.Accept(ctx, newMail("pw_1", at, "mj")); err != nil {
				t.Fatal(err)
			}
			set(t, s, "pw_1", "mj", c.from, at)
			p, err := s.CloseDelivery(ctx, Close{MailID: "pw_1", Target: "mj", Resolution: c.res, Note: "checked",
				Actor: "alice", Via: "ui", At: at.Add(time.Minute)})
			if !errors.Is(err, c.err) {
				t.Fatalf("close: %v, want %v", err, c.err)
			}
			if c.err != nil {
				if got := statuses(t, s, "pw_1", "mj"); got[0] != string(c.from) {
					t.Fatalf("refused close changed the status: %v", got)
				}
				if evs := kindsOf(t, s, KindDeliveryClosed); len(evs) != 0 {
					t.Fatalf("refused close wrote events: %+v", evs)
				}
				return
			}
			if !slices.Equal(p, []int{0, 1}) {
				t.Fatalf("closed %v", p)
			}
			if got := statuses(t, s, "pw_1", "mj"); !slices.Equal(got, []string{c.want, c.want}) {
				t.Fatalf("after close: %v", got)
			}
			ds, _ := s.Deliveries(ctx, "pw_1")
			if ds[0].Status != Status(c.after) || !ds[0].NextAttempt.IsZero() {
				t.Fatalf("delivery after close: %s, next %v", ds[0].Status, ds[0].NextAttempt)
			}
			evs := kindsOf(t, s, KindDeliveryClosed)
			if len(evs) != 2 {
				t.Fatalf("events: %+v", evs)
			}
			for i, e := range evs {
				want := "resolution=" + string(c.res) + " was=" + string(c.from) + " via=ui: checked"
				if e.Actor != "alice" || e.Target != "mj" || e.MailID != "pw_1" || *e.Recipient != i || e.Level != Info ||
					e.Text != want {
					t.Errorf("event %d: %+v", i, e)
				}
			}
			// Closed is final: a second close and a resend find nothing.
			if _, err := s.CloseDelivery(ctx, Close{MailID: "pw_1", Target: "mj", Resolution: GivenUp, At: at}); !errors.Is(err, ErrNothingToClose) {
				t.Errorf("second close: %v", err)
			}
			if _, err := s.ResendDelivery(ctx, Resend{MailID: "pw_1", Target: "mj", AcceptDuplicate: true, At: at}); err == nil {
				t.Error("resend of a closed delivery worked")
			}
		})
	}
}

func TestCloseOnlyOpenRecipients(t *testing.T) {
	at := time.Date(2026, 11, 15, 12, 0, 0, 0, time.UTC)
	s := open(t, t.TempDir(), Options{})
	m := newMail("pw_1", at, "mj")
	m.Recipients = append(m.Recipients, "c@example.test")
	if err := s.Accept(ctx, m); err != nil {
		t.Fatal(err)
	}
	set(t, s, "pw_1", "mj", HandedOff, at)
	if _, err := s.db.Exec(`UPDATE delivery_recipients SET status = 'unknown' WHERE position = 1`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`UPDATE delivery_recipients SET status = 'failed' WHERE position = 2`); err != nil {
		t.Fatal(err)
	}
	p, err := s.CloseDelivery(ctx, Close{MailID: "pw_1", Target: "mj", Resolution: Confirmed, Actor: "a", Via: "api", At: at})
	if err != nil || !slices.Equal(p, []int{1, 2}) {
		t.Fatalf("close: %v %v", p, err)
	}
	if got := statuses(t, s, "pw_1", "mj"); !slices.Equal(got, []string{"handed_off", "handed_off/confirmed", "handed_off/confirmed"}) {
		t.Fatalf("after close: %v", got)
	}
	if evs := kindsOf(t, s, KindDeliveryClosed); len(evs) != 2 || *evs[0].Recipient != 1 || *evs[1].Recipient != 2 {
		t.Fatalf("events: %+v", evs)
	}
}

func TestCloseErrors(t *testing.T) {
	at := time.Date(2026, 11, 15, 12, 0, 0, 0, time.UTC)
	s := open(t, t.TempDir(), Options{})
	if err := s.Accept(ctx, newMail("pw_1", at, "mj")); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CloseDelivery(ctx, Close{MailID: "pw_1", Target: "other", Resolution: GivenUp, At: at}); !errors.Is(err, ErrNoDelivery) {
		t.Errorf("unknown target: %v", err)
	}
	if _, err := s.CloseDelivery(ctx, Close{MailID: "pw_nope", Target: "mj", Resolution: GivenUp, At: at}); !errors.Is(err, ErrNoDelivery) {
		t.Errorf("unknown mail: %v", err)
	}
	if _, err := s.CloseDelivery(ctx, Close{MailID: "pw_1", Target: "mj", Resolution: "delivered", At: at}); err == nil {
		t.Error("invalid resolution accepted")
	}
	if _, err := s.ResendDelivery(ctx, Resend{MailID: "pw_1", Target: "other", At: at}); !errors.Is(err, ErrNoDelivery) {
		t.Errorf("resend unknown target: %v", err)
	}
	// The schema refuses a resolution that contradicts the status.
	if _, err := s.db.Exec(`UPDATE delivery_recipients SET resolution = 'confirmed' WHERE mail_id = 'pw_1'`); err == nil {
		t.Error("confirmed on a queued recipient stored")
	}
}

func TestCloseNoteRedacted(t *testing.T) {
	at := time.Date(2026, 11, 15, 12, 0, 0, 0, time.UTC)
	s := open(t, t.TempDir(), Options{})
	if err := s.Accept(ctx, newMail("pw_1", at, "mj")); err != nil {
		t.Fatal(err)
	}
	set(t, s, "pw_1", "mj", Unknown, at)
	if _, err := s.CloseDelivery(ctx, Close{MailID: "pw_1", Target: "mj", Resolution: Confirmed,
		Note: "found at the provider for jane@example.com", Actor: "a", Via: "ui", At: at}); err != nil {
		t.Fatal(err)
	}
	for _, e := range kindsOf(t, s, KindDeliveryClosed) {
		if strings.Contains(e.Text, "jane") || !strings.Contains(e.Text, "<redacted>") {
			t.Errorf("note not redacted: %q", e.Text)
		}
	}
}

// A close removes the stored message at once when no recipient of the
// mail may still need it, and keeps it while another delivery does.
func TestCloseRemovesRaw(t *testing.T) {
	at := time.Date(2026, 11, 15, 12, 0, 0, 0, time.UTC)
	s := open(t, t.TempDir(), Options{})
	if err := s.Accept(ctx, newMail("pw_1", at, "a", "b")); err != nil {
		t.Fatal(err)
	}
	set(t, s, "pw_1", "a", Unknown, at)
	set(t, s, "pw_1", "b", Failed, at)
	if _, err := s.CloseDelivery(ctx, Close{MailID: "pw_1", Target: "a", Resolution: Confirmed, At: at}); err != nil {
		t.Fatal(err)
	}
	if !mail(t, s, "pw_1").HasRaw {
		t.Fatal("raw removed although b may still be resent")
	}
	if _, err := s.CloseDelivery(ctx, Close{MailID: "pw_1", Target: "b", Resolution: GivenUp, At: at.Add(time.Minute)}); err != nil {
		t.Fatal(err)
	}
	m := mail(t, s, "pw_1")
	if m.HasRaw || !m.RawPurgedAt.Equal(at.Add(time.Minute)) {
		t.Fatalf("raw after the last close: %v %v", m.HasRaw, m.RawPurgedAt)
	}
	// Nothing is left for retention to expire.
	c, err := s.Retain(ctx, RetainPolicy{RawKeepAfterFailure: time.Hour, RawMaxAge: 2 * time.Hour,
		RecordRetention: 100 * time.Hour, EventRetention: 100 * time.Hour}, at.Add(50*time.Hour))
	if err != nil || c.RawExpired != 0 {
		t.Fatalf("retention: %+v %v", c, err)
	}
}

// A closed unknown recipient stays resolved: raw expiry never turns it
// into failed. An unknown one that nobody closed does.
func TestClosedUnknownSurvivesRawExpiry(t *testing.T) {
	at := time.Date(2026, 11, 15, 12, 0, 0, 0, time.UTC)
	s := open(t, t.TempDir(), Options{})
	if err := s.Accept(ctx, newMail("pw_1", at, "a", "b")); err != nil {
		t.Fatal(err)
	}
	set(t, s, "pw_1", "a", Unknown, at)
	set(t, s, "pw_1", "b", Unknown, at)
	if _, err := s.CloseDelivery(ctx, Close{MailID: "pw_1", Target: "a", Resolution: Confirmed, At: at}); err != nil {
		t.Fatal(err)
	}
	p := RetainPolicy{RawKeepAfterFailure: 72 * time.Hour, RawMaxAge: 7 * 24 * time.Hour,
		RecordRetention: 90 * 24 * time.Hour, EventRetention: 90 * 24 * time.Hour}
	c, err := s.Retain(ctx, p, at.Add(73*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if c.RawExpired != 2 || c.RawPurged != 1 {
		t.Fatalf("retention: %+v", c)
	}
	if got := statuses(t, s, "pw_1", "a"); !slices.Equal(got, []string{"handed_off/confirmed", "handed_off/confirmed"}) {
		t.Fatalf("closed delivery after expiry: %v", got)
	}
	if got := statuses(t, s, "pw_1", "b"); !slices.Equal(got, []string{"failed", "failed"}) {
		t.Fatalf("open delivery after expiry: %v", got)
	}
	for _, e := range kindsOf(t, s, KindRawExpired) {
		if e.Target != "b" {
			t.Errorf("raw_expired for a closed delivery: %+v", e)
		}
	}
	// After the expiry the message is gone: no resend, a close still works.
	if _, err := s.ResendDelivery(ctx, Resend{MailID: "pw_1", Target: "b", AcceptDuplicate: true, At: at}); !errors.Is(err, ErrRawGone) {
		t.Fatalf("resend after expiry: %v", err)
	}
	if _, err := s.CloseDelivery(ctx, Close{MailID: "pw_1", Target: "b", Resolution: Confirmed, At: at}); err != nil {
		t.Fatalf("close after expiry: %v", err)
	}
	if got := statuses(t, s, "pw_1", "b"); got[0] != "handed_off/confirmed" {
		t.Fatalf("after close: %v", got)
	}
}

func TestResend(t *testing.T) {
	at := time.Date(2026, 11, 15, 12, 0, 0, 0, time.UTC)
	s := open(t, t.TempDir(), Options{PurgeRawOnHandoff: true})
	m := newMail("pw_1", at, "mj")
	m.Recipients = append(m.Recipients, "c@example.test")
	if err := s.Accept(ctx, m); err != nil {
		t.Fatal(err)
	}
	set(t, s, "pw_1", "mj", HandedOff, at)
	if _, err := s.db.Exec(`UPDATE delivery_recipients SET status = 'unknown' WHERE position = 1`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`UPDATE delivery_recipients SET status = 'failed' WHERE position = 2`); err != nil {
		t.Fatal(err)
	}
	later := at.Add(time.Hour)
	if _, err := s.ResendDelivery(ctx, Resend{MailID: "pw_1", Target: "mj", Actor: "ops", Via: "ui", At: later}); !errors.Is(err, ErrDuplicateRisk) {
		t.Fatalf("resend of unknown without consent: %v", err)
	}
	if got := statuses(t, s, "pw_1", "mj"); !slices.Equal(got, []string{"handed_off", "unknown", "failed"}) {
		t.Fatalf("refused resend changed: %v", got)
	}
	p, err := s.ResendDelivery(ctx, Resend{MailID: "pw_1", Target: "mj", AcceptDuplicate: true, Actor: "ops", Via: "ui", At: later})
	if err != nil || !slices.Equal(p, []int{1, 2}) {
		t.Fatalf("resend: %v %v", p, err)
	}
	if got := statuses(t, s, "pw_1", "mj"); !slices.Equal(got, []string{"handed_off", "queued", "queued"}) {
		t.Fatalf("after resend: %v", got)
	}
	evs := kindsOf(t, s, KindDeliveryResend)
	if len(evs) != 2 || evs[0].Text != "was=unknown via=ui" || evs[1].Text != "was=failed via=ui" ||
		evs[0].Actor != "ops" || *evs[1].Recipient != 2 {
		t.Fatalf("events: %+v", evs)
	}
	// A second resend finds nothing: it cannot queue the recipients twice.
	if _, err := s.ResendDelivery(ctx, Resend{MailID: "pw_1", Target: "mj", AcceptDuplicate: true, At: later}); !errors.Is(err, ErrNothingToResend) {
		t.Fatalf("second resend: %v", err)
	}
	// Due at once; the claim takes only the resent recipients.
	c, err := s.Claim(ctx, "mj", later)
	if err != nil || c == nil || c.Raw == nil || len(c.Recipients) != 2 || c.Recipients[0].Position != 1 {
		t.Fatalf("claim after resend: %+v %v", c, err)
	}
	// While the worker holds it, nothing can be closed or resent.
	if _, err := s.ResendDelivery(ctx, Resend{MailID: "pw_1", Target: "mj", AcceptDuplicate: true, At: later}); !errors.Is(err, ErrInProgress) {
		t.Fatalf("resend while sending: %v", err)
	}
	if _, err := s.CloseDelivery(ctx, Close{MailID: "pw_1", Target: "mj", Resolution: GivenUp, At: later}); !errors.Is(err, ErrInProgress) {
		t.Fatalf("close while sending: %v", err)
	}
	r := Result{MailID: "pw_1", Target: "mj", At: later, Attempt: c.Attempt}
	for _, rc := range c.Recipients {
		r.Recipients = append(r.Recipients, RecipientResult{Position: rc.Position, Status: HandedOff, Kind: KindDeliveryHandedOff, Level: Info})
	}
	if err := s.Finish(ctx, r); err != nil {
		t.Fatal(err)
	}
	if mail(t, s, "pw_1").HasRaw {
		t.Fatal("raw kept after the resend was handed off")
	}
	if _, err := s.ResendDelivery(ctx, Resend{MailID: "pw_1", Target: "mj", At: later}); !errors.Is(err, ErrNothingToResend) {
		t.Fatalf("resend after hand-off: %v", err)
	}
}

// The mail's timeline holds the close and the resend, with their actor,
// but not who looked at the mail.
func TestTimelineHoldsAdminActions(t *testing.T) {
	at := time.Date(2026, 11, 15, 12, 0, 0, 0, time.UTC)
	s := open(t, t.TempDir(), Options{})
	if err := s.Accept(ctx, newMail("pw_1", at, "a", "b")); err != nil {
		t.Fatal(err)
	}
	set(t, s, "pw_1", "a", Failed, at)
	set(t, s, "pw_1", "b", Failed, at)
	if _, err := s.ResendDelivery(ctx, Resend{MailID: "pw_1", Target: "a", Actor: "ops", Via: "ui", At: at}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CloseDelivery(ctx, Close{MailID: "pw_1", Target: "b", Resolution: GivenUp, Actor: "ops", Via: "ui", At: at}); err != nil {
		t.Fatal(err)
	}
	if err := s.Append(ctx, Event{Time: at, MailID: "pw_1", Kind: KindUIView, Level: Info, Actor: "ops"}); err != nil {
		t.Fatal(err)
	}
	evs, err := s.MailEvents(ctx, "pw_1", 100)
	if err != nil {
		t.Fatal(err)
	}
	var kinds []string
	for _, e := range evs {
		kinds = append(kinds, e.Kind+"/"+e.Actor)
	}
	want := []string{"mail.accepted/", "delivery.resend/ops", "delivery.resend/ops", "delivery.closed/ops", "delivery.closed/ops"}
	if !slices.Equal(kinds, want) {
		t.Fatalf("timeline %v, want %v", kinds, want)
	}
}

// A closed recipient does not hold the message back at the last hand-off.
func TestFinishPurgesWithClosedRecipients(t *testing.T) {
	at := time.Date(2026, 11, 15, 12, 0, 0, 0, time.UTC)
	s := open(t, t.TempDir(), Options{PurgeRawOnHandoff: true})
	if err := s.Accept(ctx, newMail("pw_1", at, "a", "b")); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CloseDelivery(ctx, Close{MailID: "pw_1", Target: "a", Resolution: GivenUp, At: at}); err != nil {
		t.Fatal(err)
	}
	if !mail(t, s, "pw_1").HasRaw {
		t.Fatal("raw removed while b is queued")
	}
	finish(t, s, "pw_1", "b", at, HandedOff)
	if mail(t, s, "pw_1").HasRaw {
		t.Fatal("raw kept after the last hand-off")
	}
}
