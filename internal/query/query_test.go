package query

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/eneuberg/postwerk/internal/addrhash"
	"github.com/eneuberg/postwerk/internal/keys"
	"github.com/eneuberg/postwerk/internal/store"
)

var ctx = context.Background()

func openStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "pw.db"), store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

func hasher(t *testing.T) *addrhash.Hasher {
	t.Helper()
	ring, err := keys.Parse([]byte(keys.Generate(time.Now())))
	if err != nil {
		t.Fatal(err)
	}
	return addrhash.New(ring)
}

// accept stores a mail for the targets and moves each target's recipients
// to the given statuses through the store's own transitions.
func accept(t *testing.T, st *store.Store, id string, at time.Time, rcpts []string, results map[string][]store.Status) {
	t.Helper()
	m := store.NewMail{
		ID: id, ReceivedAt: at, Sender: "app", EnvelopeFrom: "noreply@example.test", Recipients: rcpts,
		MessageID: "<" + id + "@example.test>", Ref: "ref-" + id, Raw: []byte("x"), Size: 1234,
	}
	targets := slices.Sorted(maps.Keys(results))
	for _, target := range targets {
		m.Deliveries = append(m.Deliveries, store.NewDelivery{Target: target, Status: store.Queued})
	}
	if err := st.Accept(ctx, m); err != nil {
		t.Fatal(err)
	}
	for _, target := range targets {
		sts := results[target]
		if len(sts) == 0 {
			continue // stays queued
		}
		c, err := st.Claim(ctx, target, at.Add(time.Second))
		if err != nil || c == nil || c.MailID != id {
			t.Fatalf("claim %s/%s: %+v %v", id, target, c, err)
		}
		r := store.Result{MailID: id, Target: target, At: at.Add(2 * time.Second), Attempt: c.Attempt,
			Code: 250, Text: "ok", NextAttempt: at.Add(time.Hour)}
		for i, s := range sts {
			r.Recipients = append(r.Recipients, store.RecipientResult{
				Position: i, Status: s, Code: 250, Text: "reply for " + rcpts[i], Kind: "delivery.x", Level: store.Info})
		}
		if err := st.Finish(ctx, r); err != nil {
			t.Fatal(err)
		}
	}
}

func TestMask(t *testing.T) {
	for in, want := range map[string]string{
		"jane@example.com":           "j•••@e•••.com",
		"canary-leak@example.test":   "c•••@e•••.test",
		"Ünïcode@exämple.de":         "Ü•••@e•••.de",
		"root@localhost":             "r•••@l•••",
		"weird@trailing.":            "w•••@t•••",
		"no-at-sign":                 "n•••",
		`"a@b"@example.org`:          `"•••@e•••.org`,
		"a@sub.domain.example.co.uk": "a•••@s•••.uk",
	} {
		if got := Mask(in); got != want {
			t.Errorf("Mask(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestMailDetailAccess(t *testing.T) {
	st := openStore(t)
	h := hasher(t)
	q := New(Options{Store: st, Hasher: h, Targets: []string{"a", "b"}})
	now := time.Now()
	accept(t, st, "pw_1", now.Add(-time.Hour), []string{"jane@example.com", "joe@example.org"},
		map[string][]store.Status{"a": {store.HandedOff, store.Failed}, "b": {store.HandedOff, store.HandedOff}})

	full, err := q.Mail(ctx, "pw_1", Full)
	if err != nil {
		t.Fatal(err)
	}
	if full.Status != store.Partial || len(full.Deliveries) != 2 || full.Deliveries[0].Status != store.Partial ||
		full.Deliveries[1].Status != store.HandedOff {
		t.Fatalf("statuses: %+v", full)
	}
	if full.EnvelopeFrom != (Address{"noreply@example.test", Plain}) ||
		full.Deliveries[0].Recipients[1].Address != (Address{"joe@example.org", Plain}) ||
		full.Deliveries[0].Recipients[1].Status != store.Failed {
		t.Fatalf("full addresses: %+v", full.Deliveries[0].Recipients)
	}
	if full.Size != 1234 || full.Ref != "ref-pw_1" || full.MessageID != "<pw_1@example.test>" || full.Sender != "app" {
		t.Fatalf("record: %+v", full)
	}
	if len(full.Events) == 0 || full.Events[0].Kind != store.KindMailAccepted {
		t.Fatalf("events: %+v", full.Events)
	}

	masked, err := q.Mail(ctx, "pw_1", Masked)
	if err != nil {
		t.Fatal(err)
	}
	if masked.Recipients[0] != (Address{"j•••@e•••.com", MaskedAddr}) || masked.EnvelopeFrom.State != MaskedAddr ||
		masked.Deliveries[1].Recipients[0].Address.Text != "j•••@e•••.com" {
		t.Fatalf("masked: %+v", masked)
	}
	if strings.Contains(fmt.Sprintf("%+v", masked), "jane@") {
		t.Fatal("full address in a masked detail")
	}
	// The zero Access is the masked one.
	if zero, _ := q.Mail(ctx, "pw_1", Access(0)); zero.Recipients[0].State != MaskedAddr {
		t.Fatal("zero access is not masked")
	}

	if _, err := q.Mail(ctx, "pw_nope", Full); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing mail: %v", err)
	}

	// After pseudonymization nobody sees the address, and it is still found.
	if _, err := st.Retain(ctx, store.RetainPolicy{
		RawMaxAge: time.Minute, PseudonymizeAfter: time.Minute, RecordRetention: 24 * time.Hour,
		EventRetention: 24 * time.Hour, Pseudonymize: h.Hash,
	}, now); err != nil {
		t.Fatal(err)
	}
	after, err := q.Mail(ctx, "pw_1", Full)
	if err != nil {
		t.Fatal(err)
	}
	if after.PseudonymizedAt.IsZero() || after.Recipients[0] != (Address{State: Pseudonymized}) ||
		after.EnvelopeFrom.State != Pseudonymized || after.Deliveries[0].Recipients[0].Address.State != Pseudonymized {
		t.Fatalf("after pseudonymization: %+v", after)
	}
	rows, err := q.Search(ctx, ByAddress, "  JANE@example.com ")
	if err != nil || len(rows) != 1 || rows[0].ID != "pw_1" {
		t.Fatalf("search after pseudonymization: %+v %v", rows, err)
	}
}

func TestSearch(t *testing.T) {
	st := openStore(t)
	q := New(Options{Store: st, Targets: []string{"a"}}) // no hasher: plain addresses only
	now := time.Now()
	accept(t, st, "pw_1", now.Add(-2*time.Hour), []string{"jane@example.com"}, map[string][]store.Status{"a": {store.HandedOff}})
	accept(t, st, "pw_2", now.Add(-time.Hour), []string{"joe@example.com", "jane@example.com"}, map[string][]store.Status{"a": nil})

	ids := func(by SearchBy, text string) []string {
		t.Helper()
		rows, err := q.Search(ctx, by, text)
		if err != nil {
			t.Fatalf("search %s %q: %v", by, text, err)
		}
		var out []string
		for _, r := range rows {
			out = append(out, r.ID)
		}
		return out
	}
	tests := []struct {
		by   SearchBy
		text string
		want []string
	}{
		{ByID, "pw_1", []string{"pw_1"}},
		{ByID, "pw_9", nil},
		{ByRef, "ref-pw_2", []string{"pw_2"}},
		{ByMessageID, "<pw_1@example.test>", []string{"pw_1"}},
		{ByMessageID, "pw_1@example.test", []string{"pw_1"}},
		{ByAddress, "Jane@Example.com", []string{"pw_2", "pw_1"}},
		{ByAddress, "joe@example.com", []string{"pw_2"}},
		{ByAddress, "nobody@example.com", nil},
		{ByAny, "pw_2", []string{"pw_2"}},
		{ByAny, "ref-pw_1", []string{"pw_1"}},
		{ByAny, "jane@example.com", []string{"pw_2", "pw_1"}},
		{ByAny, "pw_1@example.test", []string{"pw_1"}},
		{ByAny, "noreply@example.test", []string{"pw_2", "pw_1"}}, // the envelope sender
	}
	for _, tt := range tests {
		if got := ids(tt.by, tt.text); !slices.Equal(got, tt.want) {
			t.Errorf("search %s %q = %v, want %v", tt.by, tt.text, got, tt.want)
		}
	}
	if _, err := q.Search(ctx, ByAny, "   "); !errors.Is(err, ErrEmptyQuery) {
		t.Errorf("empty: %v", err)
	}
	if _, err := q.Search(ctx, ByAny, strings.Repeat("x", 999)); !errors.Is(err, ErrQueryTooLong) {
		t.Errorf("long: %v", err)
	}
	if _, err := q.Search(ctx, "subject", "x"); !errors.Is(err, ErrInvalid) {
		t.Errorf("kind: %v", err)
	}
	if _, err := q.Search(ctx, ByAny, "\xff"); !errors.Is(err, ErrInvalid) {
		t.Errorf("invalid UTF-8: %v", err)
	}
}

func TestListAndOverview(t *testing.T) {
	st := openStore(t)
	now := time.Date(2026, 11, 15, 12, 0, 0, 0, time.UTC)
	q := New(Options{Store: st, Targets: []string{"a", "b"}, Now: func() time.Time { return now }})
	// 120 handed-off mails over the last five days, newest last.
	for i := range 120 {
		accept(t, st, fmt.Sprintf("pw_%03d", i), now.Add(-time.Duration(120-i)*time.Hour),
			[]string{"r@example.test"}, map[string][]store.Status{"a": {store.HandedOff}})
	}
	accept(t, st, "pw_fail", now.Add(-30*time.Minute), []string{"r@example.test", "s@example.test"},
		map[string][]store.Status{"a": {store.HandedOff, store.Failed}})
	accept(t, st, "pw_queued", now.Add(-10*time.Minute), []string{"r@example.test"},
		map[string][]store.Status{"a": nil})
	accept(t, st, "pw_gone", now.Add(-5*time.Minute), []string{"r@example.test"},
		map[string][]store.Status{"old": nil})

	p1, err := q.List(ctx, ListFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(p1.Rows) != PageSize || !p1.HasNext || p1.Rows[0].ID != "pw_gone" || p1.Filter.Page != 1 {
		t.Fatalf("page 1: %d rows, next %v, first %s", len(p1.Rows), p1.HasNext, p1.Rows[0].ID)
	}
	p3, err := q.List(ctx, ListFilter{Page: 3})
	if err != nil {
		t.Fatal(err)
	}
	if len(p3.Rows) != 123-2*PageSize || p3.HasNext || p3.Rows[len(p3.Rows)-1].ID != "pw_000" {
		t.Fatalf("page 3: %d rows, next %v", len(p3.Rows), p3.HasNext)
	}
	seen := map[string]bool{}
	for page := 1; page <= 3; page++ {
		p, _ := q.List(ctx, ListFilter{Page: page})
		for _, r := range p.Rows {
			if seen[r.ID] {
				t.Fatalf("%s on two pages", r.ID)
			}
			seen[r.ID] = true
		}
	}
	if len(seen) != 123 {
		t.Fatalf("pages hold %d mails", len(seen))
	}

	if p, err := q.List(ctx, ListFilter{State: "partial"}); err != nil || len(p.Rows) != 1 || p.Rows[0].ID != "pw_fail" {
		t.Fatalf("partial: %+v %v", p.Rows, err)
	}
	if p, err := q.List(ctx, ListFilter{Target: "old"}); err != nil || len(p.Rows) != 1 || p.Rows[0].ID != "pw_gone" {
		t.Fatalf("target old: %+v %v", p.Rows, err)
	}
	day := func(d int) time.Time { return time.Date(2026, 11, d, 0, 0, 0, 0, time.UTC) }
	// Received on 15 November: hours 0..11 of mails pw_108..pw_119 plus the three special ones.
	if p, err := q.List(ctx, ListFilter{From: day(15), To: day(15)}); err != nil || len(p.Rows) != 15 {
		t.Fatalf("one day: %d %v", len(p.Rows), err)
	}
	for _, bad := range []ListFilter{{State: "nope"}, {Target: "Bad-Name"}, {Page: -1}, {Page: MaxPage + 1},
		{From: day(15), To: day(14)}} {
		if _, err := q.List(ctx, bad); !errors.Is(err, ErrInvalid) {
			t.Errorf("filter %+v: %v", bad, err)
		}
	}

	o, err := q.Overview(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(o.Targets) != 3 || o.Targets[0].Target != "a" || o.Targets[1].Target != "b" || o.Targets[2].Target != "old" ||
		o.Targets[2].Configured || !o.Targets[1].Configured {
		t.Fatalf("targets: %+v", o.Targets)
	}
	a := o.Targets[0]
	counts := map[store.Status]int{}
	for _, c := range a.Recent {
		counts[c.Status] = c.Count
	}
	// Last 24 h at a: 24 hourly mails (pw_096..pw_119; pw_096 is exactly 24 h old) plus pw_fail (2) and pw_queued.
	if counts[store.HandedOff] != 25 || counts[store.Failed] != 1 || counts[store.Queued] != 1 || a.RecentTotal != 27 {
		t.Fatalf("recent at a: %v total %d", counts, a.RecentTotal)
	}
	if a.Queued != 1 || a.OldestQueuedAge != 10*time.Minute {
		t.Fatalf("queued at a: %d, %v", a.Queued, a.OldestQueuedAge)
	}
	if b := o.Targets[1]; b.RecentTotal != 0 || b.Queued != 0 || !b.OldestQueued.IsZero() {
		t.Fatalf("b: %+v", b)
	}
	if len(o.Problems) != 1 || o.Problems[0].ID != "pw_fail" || o.Problems[0].Status != store.Partial {
		t.Fatalf("problems: %+v", o.Problems)
	}
}
