package store

import (
	"math/rand/v2"
	"slices"
	"testing"
	"time"
)

// setStatuses puts the recipients of one delivery into the given statuses,
// directly, so every combination can be tested.
func setStatuses(t *testing.T, s *Store, mailID, target string, statuses ...Status) {
	t.Helper()
	for i, st := range statuses {
		if _, err := s.db.Exec(`UPDATE delivery_recipients SET status = ? WHERE mail_id = ? AND target = ? AND position = ?`,
			string(st), mailID, target, i); err != nil {
			t.Fatal(err)
		}
	}
}

// The SQL state filter and Summarize must agree for every combination.
func TestListMailsStateMatchesSummarize(t *testing.T) {
	s := open(t, t.TempDir(), Options{})
	all := []Status{Queued, Sending, HandedOff, Failed, Unknown, Skipped}
	rng := rand.New(rand.NewPCG(1, 2))
	want := map[string]Status{}
	base := time.Now().Add(-time.Hour)
	for i := range 300 {
		id := "pw_" + string(rune('a'+i/26/26%26)) + string(rune('a'+i/26%26)) + string(rune('a'+i%26))
		m := newMail(id, base.Add(time.Duration(i)*time.Second), "t")
		n := 1 + rng.IntN(3)
		m.Recipients = m.Recipients[:0]
		var sts []Status
		for j := range n {
			m.Recipients = append(m.Recipients, "r"+string(rune('0'+j))+"@example.test")
			sts = append(sts, all[rng.IntN(len(all))])
		}
		if err := s.Accept(ctx, m); err != nil {
			t.Fatal(err)
		}
		setStatuses(t, s, id, "t", sts...)
		want[id] = Summarize(sts)
	}
	for _, st := range append(slices.Clone(all), Partial) {
		rows, err := s.ListMails(ctx, MailFilter{States: []Status{st}, Limit: 1000})
		if err != nil {
			t.Fatal(err)
		}
		n := 0
		for _, w := range want {
			if w == st {
				n++
			}
		}
		if len(rows) != n {
			t.Errorf("state %s: %d rows, want %d", st, len(rows), n)
		}
		for _, r := range rows {
			if want[r.ID] != st || r.Status != st {
				t.Errorf("state %s: %s is %s (summarize %s)", st, r.ID, r.Status, want[r.ID])
			}
			if len(r.Targets) != 1 || r.Targets[0].Target != "t" || r.Targets[0].Status != st {
				t.Errorf("targets of %s = %+v", r.ID, r.Targets)
			}
		}
	}
}

func TestListMailsFilters(t *testing.T) {
	s := open(t, t.TempDir(), Options{})
	day := time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)
	for i, id := range []string{"pw_1", "pw_2", "pw_3", "pw_4"} {
		m := newMail(id, day.Add(time.Duration(i)*24*time.Hour), "a", "b")
		if i == 3 {
			m.Deliveries = m.Deliveries[:1] // only target a
		}
		if err := s.Accept(ctx, m); err != nil {
			t.Fatal(err)
		}
	}
	setStatuses(t, s, "pw_1", "a", HandedOff, HandedOff)
	setStatuses(t, s, "pw_1", "b", HandedOff, Failed) // partial at b
	setStatuses(t, s, "pw_2", "a", Failed, Failed)
	setStatuses(t, s, "pw_2", "b", HandedOff, HandedOff)

	ids := func(f MailFilter) []string {
		t.Helper()
		f.Limit = max(f.Limit, 100)
		rows, err := s.ListMails(ctx, f)
		if err != nil {
			t.Fatal(err)
		}
		var out []string
		for _, r := range rows {
			out = append(out, r.ID)
		}
		return out
	}
	tests := []struct {
		name string
		f    MailFilter
		want []string
	}{
		{"all, newest first", MailFilter{}, []string{"pw_4", "pw_3", "pw_2", "pw_1"}},
		{"target b leaves out pw_4", MailFilter{Target: "b"}, []string{"pw_3", "pw_2", "pw_1"}},
		{"partial over all targets", MailFilter{States: []Status{Partial}}, []string{"pw_2", "pw_1"}},
		{"partial at b", MailFilter{Target: "b", States: []Status{Partial}}, []string{"pw_1"}},
		{"failed at a", MailFilter{Target: "a", States: []Status{Failed}}, []string{"pw_2"}},
		{"problems", MailFilter{States: []Status{Failed, Unknown, Partial}}, []string{"pw_2", "pw_1"}},
		{"queued", MailFilter{States: []Status{Queued}}, []string{"pw_4", "pw_3"}},
		{"from", MailFilter{From: day.Add(24 * time.Hour)}, []string{"pw_4", "pw_3", "pw_2"}},
		{"to (exclusive)", MailFilter{To: day.Add(24 * time.Hour)}, []string{"pw_1"}},
		{"ids", MailFilter{IDs: []string{"pw_3", "pw_1", "pw_9"}}, []string{"pw_3", "pw_1"}},
		{"empty ids", MailFilter{IDs: []string{}}, nil},
		{"page 2", MailFilter{Limit: 1, Offset: 1}, nil},
	}
	for _, tt := range tests {
		if tt.name == "page 2" {
			rows, err := s.ListMails(ctx, tt.f)
			if err != nil || len(rows) != 1 || rows[0].ID != "pw_3" {
				t.Errorf("%s: %+v %v", tt.name, rows, err)
			}
			continue
		}
		if got := ids(tt.f); !slices.Equal(got, tt.want) {
			t.Errorf("%s: %v, want %v", tt.name, got, tt.want)
		}
	}
	rows, _ := s.ListMails(ctx, MailFilter{IDs: []string{"pw_1"}, Limit: 1})
	if len(rows) != 1 || rows[0].Recipients != 2 || rows[0].Sender != "app" || rows[0].Status != Partial ||
		!slices.Equal(rows[0].Targets, []TargetStatus{{"a", HandedOff}, {"b", Partial}}) {
		t.Fatalf("row = %+v", rows)
	}
	if _, err := s.ListMails(ctx, MailFilter{States: []Status{"bogus"}, Limit: 1}); err == nil {
		t.Fatal("unknown state accepted")
	}
	if _, err := s.ListMails(ctx, MailFilter{}); err == nil {
		t.Fatal("no limit accepted")
	}
}

func TestTargetStatsAndLookups(t *testing.T) {
	s := open(t, t.TempDir(), Options{})
	now := time.Now()
	old := newMail("pw_old", now.Add(-48*time.Hour), "a")
	old.Ref, old.MessageID = "ref-old", "<old@x>"
	recent := newMail("pw_new", now.Add(-time.Hour), "a", "b")
	for _, m := range []NewMail{old, recent} {
		if err := s.Accept(ctx, m); err != nil {
			t.Fatal(err)
		}
	}
	setStatuses(t, s, "pw_new", "a", HandedOff, Failed)
	stats, err := s.TargetStatsSince(ctx, now.Add(-24*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if len(stats) != 2 || stats[0].Target != "a" || stats[1].Target != "b" {
		t.Fatalf("stats = %+v", stats)
	}
	a, b := stats[0], stats[1]
	if a.Recent[HandedOff] != 1 || a.Recent[Failed] != 1 || a.Recent[Queued] != 0 || a.QueuedMails != 1 ||
		a.OldestQueued.UnixMilli() != old.ReceivedAt.UnixMilli() {
		t.Errorf("a = %+v", a)
	}
	if b.Recent[Queued] != 2 || b.QueuedMails != 1 || b.OldestQueued.UnixMilli() != recent.ReceivedAt.UnixMilli() {
		t.Errorf("b = %+v", b)
	}

	if ids, err := s.MailIDsByRef(ctx, "ref-old", 10); err != nil || !slices.Equal(ids, []string{"pw_old"}) {
		t.Errorf("by ref: %v %v", ids, err)
	}
	if ids, err := s.MailIDsByRef(ctx, "r1", 10); err != nil || !slices.Equal(ids, []string{"pw_new"}) {
		t.Errorf("by ref r1: %v %v", ids, err)
	}
	if ids, err := s.MailIDsByMessageID(ctx, []string{"old@x", "<old@x>"}, 10); err != nil || !slices.Equal(ids, []string{"pw_old"}) {
		t.Errorf("by message id: %v %v", ids, err)
	}
	if ids, _ := s.MailIDsByRef(ctx, "", 10); ids != nil {
		t.Errorf("empty ref found %v", ids)
	}

	evs, err := s.MailEvents(ctx, "pw_new", 100)
	if err != nil || len(evs) != 1 || evs[0].Kind != KindMailAccepted || evs[0].Sender != "app" {
		t.Fatalf("events = %+v %v", evs, err)
	}
}

func TestAppendWithActor(t *testing.T) {
	s := open(t, t.TempDir(), Options{})
	if err := s.Append(ctx, Event{Time: time.Now(), Kind: "ui.login", Level: Info, Actor: "ops", Text: "admin"}); err != nil {
		t.Fatal(err)
	}
	if err := s.Append(ctx, Event{Time: time.Now(), Kind: "ui.search", Level: "loud"}); err == nil {
		t.Fatal("invalid level accepted")
	}
	if err := s.Append(ctx, Event{Time: time.Now(), Level: Info}); err == nil {
		t.Fatal("event without kind accepted")
	}
	evs, err := s.EventsAfter(ctx, 0, 10)
	if err != nil || len(evs) != 1 {
		t.Fatalf("events = %+v %v", evs, err)
	}
	if e := evs[0]; e.Kind != "ui.login" || e.Actor != "ops" || e.Text != "admin" || e.MailID != "" || e.Sender != "" {
		t.Fatalf("event = %+v", e)
	}
}
