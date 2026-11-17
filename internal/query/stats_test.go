package query

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/eneuberg/postwerk/internal/store"
)

// attempt is one try of a delivery: at a time, the status of each recipient
// still open, and the event kind of each.
type attempt struct {
	at    time.Time
	sts   []store.Status
	kinds []string
}

// statsMail stores a mail and runs its attempts through the store's own
// transitions. A target without attempts stays queued.
func statsMail(t *testing.T, st *store.Store, id, sender string, at time.Time, rcpts int, deliveries map[string][]attempt,
	skipped map[string]string) {
	t.Helper()
	m := store.NewMail{ID: id, ReceivedAt: at, Sender: sender, EnvelopeFrom: "noreply@example.test", Raw: []byte("x"), Size: 10}
	for i := range rcpts {
		m.Recipients = append(m.Recipients, fmt.Sprintf("r%d@example.test", i))
	}
	for _, target := range []string{"capture", "mailjet"} {
		if kind, ok := skipped[target]; ok {
			m.Deliveries = append(m.Deliveries, store.NewDelivery{Target: target, Status: store.Skipped, Kind: kind, Level: store.Warn})
		} else if _, ok := deliveries[target]; ok {
			m.Deliveries = append(m.Deliveries, store.NewDelivery{Target: target, Status: store.Queued})
		}
	}
	if err := st.Accept(ctx, m); err != nil {
		t.Fatal(err)
	}
	for _, target := range []string{"capture", "mailjet"} {
		for _, a := range deliveries[target] {
			c, err := st.Claim(ctx, target, a.at)
			if err != nil || c == nil || c.MailID != id {
				t.Fatalf("claim %s/%s: %+v %v", id, target, c, err)
			}
			r := store.Result{MailID: id, Target: target, At: a.at, Attempt: c.Attempt, Code: 250, Text: "ok",
				NextAttempt: a.at.Add(time.Minute)}
			for i, s := range a.sts {
				r.Recipients = append(r.Recipients, store.RecipientResult{Position: c.Recipients[i].Position, Status: s,
					Code: 250, Text: "ok", Kind: a.kinds[i], Level: store.Info})
			}
			if err := st.Finish(ctx, r); err != nil {
				t.Fatal(err)
			}
		}
	}
}

func ok(at time.Time) attempt {
	return attempt{at: at, sts: []store.Status{store.HandedOff}, kinds: []string{store.KindDeliveryHandedOff}}
}

func hm(h, m, s int) time.Time { return time.Date(2026, 11, 16, h, m, s, 0, time.UTC) }

// statsData stores the dataset of TestStats (see the expectations there).
func statsData(t *testing.T, st *store.Store) {
	t.Helper()
	event := func(at time.Time, kind, target, sender, text string) {
		t.Helper()
		if err := st.Append(ctx, store.Event{Time: at, Kind: kind, Level: store.Warn, Target: target, Sender: sender, Text: text}); err != nil {
			t.Fatal(err)
		}
	}
	// Before and after the range.
	statsMail(t, st, "pw_before", "shop", hm(9, 59, 59), 1, map[string][]attempt{"mailjet": {ok(hm(10, 0, 1))}}, nil)
	statsMail(t, st, "pw_after", "shop", hm(13, 0, 0), 1, map[string][]attempt{"mailjet": {ok(hm(13, 0, 1))}}, nil)
	// Bucket 10:00.
	statsMail(t, st, "pw_m1", "shop", hm(10, 10, 0), 1, map[string][]attempt{
		"mailjet": {ok(hm(10, 10, 2))}, "capture": {ok(hm(10, 10, 1))}}, nil)
	// Received just before 11:00, a retry after it, handed off at 11:05.
	statsMail(t, st, "pw_m2", "shop", hm(10, 59, 59), 1, map[string][]attempt{"mailjet": {
		{at: hm(11, 0, 1), sts: []store.Status{store.Queued}, kinds: []string{store.KindDeliveryRetry}},
		ok(hm(11, 5, 0))}}, nil)
	// Bucket 11:00: partial at mailjet (one rejected), failed at the
	// optional capture: the mail is partial.
	statsMail(t, st, "pw_m3", "billing", hm(11, 30, 0), 2, map[string][]attempt{
		"mailjet": {{at: hm(11, 30, 2), sts: []store.Status{store.HandedOff, store.Failed},
			kinds: []string{store.KindDeliveryHandedOff, store.KindDeliveryRejected}}},
		"capture": {{at: hm(11, 30, 1), sts: []store.Status{store.Failed, store.Failed},
			kinds: []string{store.KindDeliveryFailed, store.KindDeliveryFailed}}}}, nil)
	// Unknown, then closed as confirmed: handed off, but no latency.
	statsMail(t, st, "pw_m4", "billing", hm(11, 40, 0), 1, map[string][]attempt{"mailjet": {
		{at: hm(11, 40, 3), sts: []store.Status{store.Unknown}, kinds: []string{store.KindDeliveryUnknown}}}}, nil)
	if _, err := st.CloseDelivery(ctx, store.Close{MailID: "pw_m4", Target: "mailjet", Resolution: store.Confirmed,
		Actor: "alice", Via: "ui", At: hm(12, 10, 0)}); err != nil {
		t.Fatal(err)
	}
	// Bucket 12:00, received on its first millisecond: only the optional
	// target, failed; so the mail is failed.
	statsMail(t, st, "pw_m5", "shop", hm(12, 0, 0), 1, map[string][]attempt{"capture": {
		{at: hm(12, 0, 1), sts: []store.Status{store.Failed}, kinds: []string{store.KindDeliveryFailed}}}}, nil)
	// Skipped at mailjet (allow-list), handed off at capture: skipped.
	statsMail(t, st, "pw_m7", "shop", hm(12, 45, 0), 1, map[string][]attempt{"capture": {ok(hm(12, 45, 1))}},
		map[string]string{"mailjet": store.KindDeliveryNotAllowed})
	// Still queued at mailjet.
	statsMail(t, st, "pw_m6", "shop", hm(12, 30, 0), 1, map[string][]attempt{"mailjet": nil, "capture": {ok(hm(12, 30, 1))}}, nil)

	event(hm(10, 30, 0), "ingress.auth_failed", "", "shop", "failures=3")
	event(hm(11, 15, 0), "ingress.auth_failed", "", "", "failures=2")
	event(hm(12, 20, 0), "ingress.rejected", "", "billing", "reason=too_large rejected=4")
	event(hm(14, 0, 0), "ingress.rejected", "", "billing", "reason=too_large rejected=50") // after the range
	event(hm(9, 0, 0), store.KindTargetDown, "mailjet", "", "auth: 535")
	event(hm(10, 30, 0), store.KindTargetRecovered, "mailjet", "", "auth, down for 1h30m")
	event(hm(12, 50, 0), store.KindTargetDown, "mailjet", "", "unreachable: refused")
}

func TestStats(t *testing.T) {
	st := openStore(t)
	statsData(t, st)
	now := hm(13, 30, 0)
	health := []TargetHealth{{Target: "mailjet", Down: true, Since: hm(12, 50, 0)}, {Target: "capture", Since: hm(8, 0, 0)}}
	svc := New(Options{Store: st, Targets: []string{"mailjet", "capture"}, Optional: []string{"capture"},
		Health: func() []TargetHealth { return health }, Now: func() time.Time { return now }})
	r, err := ParseStatsRange("", "2026-11-16T10:00", "2026-11-16T13:00", true)
	if err != nil {
		t.Fatal(err)
	}
	s, err := svc.Stats(ctx, StatsRequest{Range: r})
	if err != nil {
		t.Fatal(err)
	}
	if s.Width != 5*time.Minute || !s.From.Equal(hm(10, 0, 0)) || !s.To.Equal(hm(13, 0, 0)) || len(s.Buckets) != 36 {
		t.Fatalf("range %s–%s width %s buckets %d", s.From, s.To, s.Width, len(s.Buckets))
	}
	want := Counts{Total: 7, HandedOff: 3, Partial: 1, Failed: 1, InFlight: 1, Skipped: 1, Rejected: 1, NotAllowed: 1, Retries: 1}
	if s.Totals != want {
		t.Errorf("totals\n got %+v\nwant %+v", s.Totals, want)
	}
	if rate, ok := s.Totals.SuccessRate(); !ok || rate != 0.6 {
		t.Errorf("success rate %v %v", rate, ok)
	}
	if s.Ingress != (Ingress{AuthFailed: 5, Rejected: 4}) {
		t.Errorf("ingress %+v", s.Ingress)
	}
	// Overall latency over mailjet only: m1 2 s, m3 2 s, m2 5 min 1 s
	// (rounded down to 5 min); m4 was closed by an admin.
	if s.Latency != (Latency{Count: 3, P50: 2 * time.Second, P95: 5 * time.Minute}) {
		t.Errorf("latency %+v", s.Latency)
	}

	// By bucket: m2 counts at 10:55 where it was received, its retry at
	// 11:00 where it happened.
	at := func(h, m int) Bucket { return s.Buckets[int(hm(h, m, 0).Sub(s.From)/s.Width)] }
	for _, c := range []struct {
		b    Bucket
		want Counts
		in   Ingress
	}{
		{at(10, 10), Counts{Total: 1, HandedOff: 1}, Ingress{}},
		{at(10, 55), Counts{Total: 1, HandedOff: 1}, Ingress{}},
		{at(11, 0), Counts{Retries: 1}, Ingress{}},
		{at(10, 30), Counts{}, Ingress{AuthFailed: 3}},
		{at(11, 30), Counts{Total: 1, Partial: 1, Rejected: 1}, Ingress{}},
		{at(11, 40), Counts{Total: 1, HandedOff: 1}, Ingress{}},
		{at(12, 0), Counts{Total: 1, Failed: 1}, Ingress{}},
		{at(12, 30), Counts{Total: 1, InFlight: 1}, Ingress{}},
		{at(12, 45), Counts{Total: 1, Skipped: 1, NotAllowed: 1}, Ingress{}},
		{at(12, 20), Counts{}, Ingress{Rejected: 4}},
	} {
		if c.b.Counts != c.want || c.b.Ingress != c.in {
			t.Errorf("bucket %s: %+v %+v, want %+v %+v", c.b.Start.Format("15:04"), c.b.Counts, c.b.Ingress, c.want, c.in)
		}
	}
	sum := Counts{}
	for _, b := range s.Buckets {
		sum.Total += b.Total
		sum.Retries += b.Retries
	}
	if sum.Total != 7 || sum.Retries != 1 {
		t.Errorf("buckets add up to %+v", sum)
	}

	if len(s.Targets) != 2 || s.Targets[0].Target != "mailjet" || s.Targets[1].Target != "capture" {
		t.Fatalf("targets %+v", s.Targets)
	}
	mj, cp := s.Targets[0], s.Targets[1]
	if want := (Counts{Total: 6, HandedOff: 3, Partial: 1, InFlight: 1, Skipped: 1, Rejected: 1, NotAllowed: 1, Retries: 1}); mj.Counts != want {
		t.Errorf("mailjet\n got %+v\nwant %+v", mj.Counts, want)
	}
	if mj.Latency != s.Latency || !mj.DownNow || mj.Optional {
		t.Errorf("mailjet %+v", mj)
	}
	// Down from before the range until 10:30, and from 12:50 to the end.
	if mj.Down != 40*time.Minute || len(mj.Outages) != 2 || !mj.Outages[1].Open {
		t.Errorf("mailjet down %s %+v", mj.Down, mj.Outages)
	}
	if want := (Counts{Total: 5, HandedOff: 3, Failed: 2}); cp.Counts != want || !cp.Optional || cp.Down != 0 {
		t.Errorf("capture\n got %+v\nwant %+v", cp, want)
	}
	if cp.Latency != (Latency{Count: 3, P50: time.Second, P95: time.Second}) {
		t.Errorf("capture latency %+v", cp.Latency)
	}

	senders := map[string]SenderStats{}
	var names []string
	for _, x := range s.Senders {
		senders[x.Sender] = x
		names = append(names, x.Sender)
	}
	if strings.Join(names, ",") != "shop,billing," {
		t.Errorf("senders in order %q", names)
	}
	if want := (Counts{Total: 5, HandedOff: 2, Failed: 1, InFlight: 1, Skipped: 1, NotAllowed: 1, Retries: 1}); senders["shop"].Counts != want ||
		senders["shop"].Ingress != (Ingress{AuthFailed: 3}) {
		t.Errorf("shop %+v", senders["shop"])
	}
	if want := (Counts{Total: 2, HandedOff: 1, Partial: 1, Rejected: 1}); senders["billing"].Counts != want ||
		senders["billing"].Ingress != (Ingress{Rejected: 4}) {
		t.Errorf("billing %+v", senders["billing"])
	}
	if senders[""].Ingress != (Ingress{AuthFailed: 2}) || senders[""].Total != 0 {
		t.Errorf("unknown sender %+v", senders[""])
	}

	// A target filter: the state at that target, the optional one too.
	s, err = svc.Stats(ctx, StatsRequest{Range: r, Target: "capture"})
	if err != nil {
		t.Fatal(err)
	}
	if want := (Counts{Total: 5, HandedOff: 3, Failed: 2}); s.Totals != want || len(s.Targets) != 1 || s.Ingress.AuthFailed != 5 {
		t.Errorf("target=capture: %+v %d targets, ingress %+v", s.Totals, len(s.Targets), s.Ingress)
	}
	if s.Latency.Count != 3 {
		t.Errorf("target=capture latency %+v", s.Latency)
	}
	// A sender filter.
	s, err = svc.Stats(ctx, StatsRequest{Range: r, Sender: "billing"})
	if err != nil {
		t.Fatal(err)
	}
	if want := (Counts{Total: 2, HandedOff: 1, Partial: 1, Rejected: 1}); s.Totals != want ||
		s.Ingress != (Ingress{Rejected: 4}) || len(s.Senders) != 1 {
		t.Errorf("sender=billing: %+v %+v %+v", s.Totals, s.Ingress, s.Senders)
	}
	if s.Targets[1].Counts != (Counts{Total: 1, Failed: 1}) {
		t.Errorf("sender=billing, capture: %+v", s.Targets[1])
	}
	// Unknown filters are refused, not counted as nothing.
	for _, req := range []StatsRequest{{Range: r, Target: "Mail Jet"}, {Range: r, Sender: "a@b.c d"}, {Range: StatsRange{Preset: "2h"}}} {
		if _, err := svc.Stats(ctx, req); !errors.Is(err, ErrInvalid) {
			t.Errorf("%+v: %v", req, err)
		}
	}
}

// The counts of an hour that is over are kept, and an event about one of
// its mails makes them count again.
func TestStatsCacheFollowsChanges(t *testing.T) {
	st := openStore(t)
	statsData(t, st)
	now := hm(15, 0, 0)
	svc := New(Options{Store: st, Targets: []string{"mailjet", "capture"}, Optional: []string{"capture"},
		Now: func() time.Time { return now }, RecordRetention: 90 * 24 * time.Hour, EventRetention: 90 * 24 * time.Hour})
	r := StatsRange{From: hm(10, 0, 0), To: hm(13, 0, 0)}
	get := func() Counts {
		t.Helper()
		s, err := svc.Stats(ctx, StatsRequest{Range: r})
		if err != nil {
			t.Fatal(err)
		}
		return s.Totals
	}
	if c := get(); c.InFlight != 1 || c.HandedOff != 3 {
		t.Fatalf("before: %+v", c)
	}
	if c := get(); c.InFlight != 1 || c.HandedOff != 3 {
		t.Fatalf("from the cache: %+v", c)
	}
	// m6 (12:30) goes out at 14:00; m3 (11:30) is closed for its failed
	// recipient at mailjet.
	c, err := st.Claim(ctx, "mailjet", hm(14, 0, 0))
	if err != nil || c == nil || c.MailID != "pw_m6" {
		t.Fatalf("claim: %+v %v", c, err)
	}
	if err := st.Finish(ctx, store.Result{MailID: "pw_m6", Target: "mailjet", At: hm(14, 0, 1), Attempt: c.Attempt,
		Recipients: []store.RecipientResult{{Position: 0, Status: store.HandedOff, Code: 250, Kind: store.KindDeliveryHandedOff, Level: store.Info}}}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.CloseDelivery(ctx, store.Close{MailID: "pw_m3", Target: "mailjet", Resolution: store.Confirmed,
		Actor: "alice", Via: "ui", At: hm(14, 10, 0)}); err != nil {
		t.Fatal(err)
	}
	if c := get(); c.InFlight != 0 || c.HandedOff != 5 || c.Partial != 0 {
		t.Errorf("after the changes: %+v", c)
	}
}

func TestParseStatsRange(t *testing.T) {
	for _, c := range []struct {
		rng, from, to string
		strict        bool
		want          StatsRange
		bad           bool
	}{
		{want: StatsRange{Preset: "24h"}},
		{rng: "7d", want: StatsRange{Preset: "7d"}},
		{rng: "7d", from: "2026-11-01", want: StatsRange{Preset: "7d"}},
		{rng: "7d", from: "2026-11-01", strict: true, bad: true},
		{rng: "2h", bad: true},
		{rng: "custom", bad: true},
		{from: "2026-11-01", want: StatsRange{From: time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)}},
		{rng: "custom", from: "2026-11-01", to: "2026-11-02", want: StatsRange{
			From: time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC), To: time.Date(2026, 11, 3, 0, 0, 0, 0, time.UTC)}},
		{from: "2026-11-01T08:30", to: "2026-11-01T09:00:30", want: StatsRange{
			From: time.Date(2026, 11, 1, 8, 30, 0, 0, time.UTC), To: time.Date(2026, 11, 1, 9, 0, 30, 0, time.UTC)}},
		{from: "2026-11-01T08:30:00+02:00", want: StatsRange{From: time.Date(2026, 11, 1, 6, 30, 0, 0, time.UTC)}},
		{from: "2026-11-02", to: "2026-11-01", bad: true},
		{from: "2026-11-01T10:00", to: "2026-11-01T10:00", bad: true},
		{to: "2026-11-01", bad: true},
		{from: "yesterday", bad: true},
		{from: "2026-13-01", bad: true},
		{from: strings.Repeat("9", 50), bad: true},
	} {
		got, err := ParseStatsRange(c.rng, c.from, c.to, c.strict)
		if c.bad {
			if !errors.Is(err, ErrInvalid) {
				t.Errorf("%+v: %+v, %v; want ErrInvalid", c, got, err)
			}
			continue
		}
		if err != nil || got != c.want {
			t.Errorf("%+v: %+v, %v", c, got, err)
		}
	}
}

func TestBucketWidth(t *testing.T) {
	want := map[string]time.Duration{"1h": time.Minute, "6h": 5 * time.Minute, "24h": 30 * time.Minute,
		"7d": 3 * time.Hour, "30d": 24 * time.Hour}
	for _, p := range StatsPresets {
		if got := BucketWidth(p.Span); got != want[p.Name] {
			t.Errorf("%s: %s, want %s", p.Name, got, want[p.Name])
		}
	}
	for span, w := range map[time.Duration]time.Duration{
		10 * time.Minute: time.Minute, 3 * time.Hour: 5 * time.Minute, 90 * 24 * time.Hour: 24 * time.Hour,
		365 * 24 * time.Hour: 7 * 24 * time.Hour, 3 * 24 * time.Hour: time.Hour, 4 * 24 * time.Hour: 2 * time.Hour,
	} {
		if got := BucketWidth(span); got != w {
			t.Errorf("%s: %s, want %s", span, got, w)
		}
	}
}

// The range starts no earlier than retention keeps, and says so.
func TestStatsClampedToRetention(t *testing.T) {
	st := openStore(t)
	now := hm(12, 0, 0)
	svc := New(Options{Store: st, Now: func() time.Time { return now }, RecordRetention: 90 * 24 * time.Hour,
		EventRetention: 30 * 24 * time.Hour})
	s, err := svc.Stats(ctx, StatsRequest{Range: StatsRange{From: now.AddDate(0, 0, -60)}})
	if err != nil {
		t.Fatal(err)
	}
	if !s.Clamped || !s.Retained.Equal(now.AddDate(0, 0, -30)) || s.From.After(s.Retained) || s.Width != 24*time.Hour {
		t.Errorf("clamped %v retained %s from %s width %s", s.Clamped, s.Retained, s.From, s.Width)
	}
	if !s.Empty() || len(s.Buckets) != 31 {
		t.Errorf("empty %v, %d buckets", s.Empty(), len(s.Buckets))
	}
	if _, err := svc.Stats(ctx, StatsRequest{Range: StatsRange{From: now.AddDate(0, 0, -60), To: now.AddDate(0, 0, -40)}}); !errors.Is(err, ErrInvalid) {
		t.Errorf("a range wholly before retention: %v", err)
	}
	if _, err := svc.Stats(ctx, StatsRequest{Range: StatsRange{From: now.Add(time.Hour)}}); !errors.Is(err, ErrInvalid) {
		t.Errorf("a range in the future: %v", err)
	}
}
