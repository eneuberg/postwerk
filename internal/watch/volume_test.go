package watch

import (
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/eneuberg/postwerk/internal/id"
	"github.com/eneuberg/postwerk/internal/store"
)

func TestSenderVolume(t *testing.T) {
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "pw.db"), store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	now := time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC)
	mails := func(sender string, n int, at time.Time) {
		t.Helper()
		for i := range n {
			when := at.Add(time.Duration(i) * time.Second)
			if err := st.Accept(ctx, store.NewMail{ID: id.NewAt(when), ReceivedAt: when, Sender: sender,
				Recipients: []string{"r@example.test"}, Raw: []byte{0}}); err != nil {
				t.Fatal(err)
			}
		}
	}
	// History over the week before the last hour: "steady" 1 mail an
	// hour on average, "busy" 5.
	for d := range 7 {
		mails("steady", 24, now.Add(-2*time.Hour-time.Duration(d)*24*time.Hour))
		mails("busy", 120, now.Add(-2*time.Hour-time.Duration(d)*24*time.Hour))
	}
	// The last hour: steady 50 (above 10 × 1, but below Min), busy 49
	// (below 10 × 5), spike 110 without any history.
	mails("steady", 50, now.Add(-30*time.Minute))
	mails("busy", 49, now.Add(-30*time.Minute))
	mails("spike", 110, now.Add(-30*time.Minute))
	clock := now
	v, err := NewVolume(ctx, VolumeOptions{Store: st, Factor: 10, Min: 100, Max: 200, Now: func() time.Time { return clock }})
	if err != nil {
		t.Fatal(err)
	}
	if err := v.Check(ctx); err != nil {
		t.Fatal(err)
	}
	got := volumeEvents(t, st)
	if len(got) != 1 || !strings.HasPrefix(got[0], "spike mails_last_hour=110 hourly_average=0.0 factor=10") {
		t.Fatalf("events %q", got)
	}
	// Once an hour per sender, even while it continues.
	mails("spike", 10, now.Add(-time.Minute))
	clock = now.Add(30 * time.Minute)
	v.Check(ctx)
	if n := len(volumeEvents(t, st)); n != 1 {
		t.Fatalf("repeated within the hour: %d", n)
	}
	// A restart remembers the last event.
	v2, err := NewVolume(ctx, VolumeOptions{Store: st, Factor: 10, Min: 100, Now: func() time.Time { return clock }})
	if err != nil {
		t.Fatal(err)
	}
	v2.Check(ctx)
	if n := len(volumeEvents(t, st)); n != 1 {
		t.Fatalf("repeated after restart: %d", n)
	}
	// The absolute maximum applies whatever the history.
	mails("busy", 160, now.Add(20*time.Minute))
	v.Check(ctx)
	got = volumeEvents(t, st)
	if len(got) != 2 || !strings.HasPrefix(got[1], "busy mails_last_hour=209 hourly_average=5.0 max=200") {
		t.Fatalf("events %q", got)
	}
	// Off: no queries at all.
	off, err := NewVolume(ctx, VolumeOptions{Store: nil})
	if err != nil || off.Check(ctx) != nil {
		t.Fatal("an off check touched the store")
	}
}

func volumeEvents(t *testing.T, st *store.Store) []string {
	t.Helper()
	evs, err := st.EventsAfter(ctx, 0, 100000)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, e := range evs {
		if e.Kind == KindSenderVolumeHigh {
			if e.Level != store.Error {
				t.Fatalf("level %s", e.Level)
			}
			out = append(out, e.Sender+" "+e.Text)
		}
	}
	return out
}
