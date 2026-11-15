package report

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/eneuberg/postwerk/internal/store"
	"github.com/eneuberg/postwerk/internal/testutil"
)

var ctx = context.Background()

func openStore(t *testing.T) *store.Store {
	t.Helper()
	s, err := store.Open(ctx, filepath.Join(t.TempDir(), "pw.db"), store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func addMail(t *testing.T, s *store.Store, id string) {
	t.Helper()
	err := s.Accept(ctx, store.NewMail{
		ID: id, ReceivedAt: time.UnixMilli(1_763_000_000_123), Sender: "app", EnvelopeFrom: "f@example.test",
		Recipients: []string{"r@example.test"}, Raw: []byte("x"),
		Deliveries: []store.NewDelivery{{Target: "t", Status: store.Skipped, Kind: store.KindDeliverySkipped, Level: store.Info, Text: "skipped"}},
	})
	if err != nil {
		t.Fatal(err)
	}
}

// flaky fails after n successful exports, until healed.
type flaky struct {
	n      int
	got    []int64
	broken bool
}

func (f *flaky) Name() string { return "flaky" }
func (f *flaky) Export(_ context.Context, e store.Event) error {
	if f.broken || (f.n >= 0 && len(f.got) >= f.n) {
		return errors.New("sink down")
	}
	f.got = append(f.got, e.Seq)
	return nil
}

func TestStdoutLine(t *testing.T) {
	s := openStore(t)
	addMail(t, s, "pw_1")
	var buf bytes.Buffer
	r := New(Options{Store: s, Sinks: []Sink{NewStdout(&buf, "prod")}})
	if err := r.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	if len(lines) != 2 {
		t.Fatalf("lines = %q", lines)
	}
	want := `{"seq":1,"time":"2025-11-13T02:13:20.123Z","level":"info","kind":"mail.accepted","mail_id":"pw_1","sender":"app","instance":"prod"}`
	if lines[0] != want {
		t.Fatalf("line:\n%s\nwant\n%s", lines[0], want)
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(lines[1]), &m); err != nil {
		t.Fatal(err)
	}
	if m["kind"] != "delivery.skipped" || m["target"] != "t" || m["recipient"] != float64(0) ||
		m["smtp"].(map[string]any)["text"] != "skipped" {
		t.Fatalf("second line = %s", lines[1])
	}
	if strings.Contains(buf.String(), "@") {
		t.Fatal("address in output")
	}

	// Nothing new: nothing written.
	buf.Reset()
	if err := r.Flush(ctx); err != nil || buf.Len() != 0 {
		t.Fatalf("second flush wrote %q, %v", buf.String(), err)
	}

	// A UI event names its actor.
	at := time.UnixMilli(1763000000123)
	if err := s.Append(ctx, store.Event{Time: at, Kind: "ui.login", Level: store.Info, Actor: "ops", Text: "admin"}); err != nil {
		t.Fatal(err)
	}
	if err := r.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	want = `{"seq":3,"time":"2025-11-13T02:13:20.123Z","level":"info","kind":"ui.login","actor":"ops","text":"admin","instance":"prod"}`
	if got := strings.TrimSpace(buf.String()); got != want {
		t.Fatalf("line:\n%s\nwant\n%s", got, want)
	}
}

func TestFailingSinkCatchesUp(t *testing.T) {
	s := openStore(t)
	for i := range 3 {
		addMail(t, s, "pw_"+string(rune('a'+i))) // 2 events each
	}
	f := &flaky{n: 4}
	var buf bytes.Buffer
	r := New(Options{Store: s, Sinks: []Sink{f, NewStdout(&buf, "")}})
	if err := r.Flush(ctx); err == nil {
		t.Fatal("no error from failing sink")
	}
	if c, _ := s.Cursor(ctx, "flaky"); c != 4 {
		t.Fatalf("flaky cursor = %d, want 4", c)
	}
	if c, _ := s.Cursor(ctx, "stdout"); c != 6 {
		t.Fatalf("stdout cursor = %d, want 6 (other sinks must not wait)", c)
	}

	f.broken = true
	_ = r.Flush(ctx)
	if c, _ := s.Cursor(ctx, "flaky"); c != 4 {
		t.Fatalf("cursor moved while broken: %d", c)
	}

	f.broken, f.n = false, -1
	if err := r.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	if len(f.got) != 6 || f.got[5] != 6 {
		t.Fatalf("got %v", f.got)
	}
	for i, seq := range f.got {
		if seq != int64(i+1) {
			t.Fatalf("out of order or duplicate: %v", f.got)
		}
	}
}

func TestRunWakes(t *testing.T) {
	s := openStore(t)
	var buf testutil.SafeBuffer
	r := New(Options{Store: s, Sinks: []Sink{NewStdout(&buf, "")}, Interval: time.Hour})
	ctx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() { r.Run(ctx); close(done) }()
	addMail(t, s, "pw_1")
	r.Wake()
	deadline := time.Now().Add(5 * time.Second)
	for !strings.Contains(buf.String(), "delivery.skipped") {
		if time.Now().After(deadline) {
			t.Fatal("event not exported after wake")
		}
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	<-done
}
