package retention

import (
	"context"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/eneuberg/postwerk/internal/store"
	"github.com/eneuberg/postwerk/internal/testutil"
)

func TestRunner(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "pw.db"), store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	t0 := time.UnixMilli(10_000_000)
	err = st.Accept(ctx, store.NewMail{
		ID: "pw_1", ReceivedAt: t0, Sender: "app", EnvelopeFrom: "a@example.test",
		Recipients: []string{"b@example.test"}, Raw: []byte{0, 'x'}, Size: 1,
		Deliveries: []store.NewDelivery{{Target: "t", Status: store.Queued}},
	})
	if err != nil {
		t.Fatal(err)
	}

	var now atomic.Int64
	now.Store(t0.UnixMilli())
	var logs testutil.SafeBuffer
	r := New(Options{
		Store: st, Logger: testutil.Logger(&logs), Interval: 10 * time.Millisecond,
		Now:    func() time.Time { return time.UnixMilli(now.Load()) },
		Policy: store.RetainPolicy{RawMaxAge: time.Hour, RecordRetention: 2 * time.Hour, EventRetention: 2 * time.Hour},
	})
	if c, err := r.Once(ctx); err != nil || c != (store.RetainCounts{}) {
		t.Fatalf("first run = %+v %v", c, err)
	}
	if logs.String() != "" {
		t.Fatalf("empty run logged: %s", logs.String())
	}

	runCtx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() { r.Run(runCtx); close(done) }()
	now.Store(t0.Add(3 * time.Hour).UnixMilli())
	testutil.Eventually(t, 5*time.Second, "a run by the loop", func() bool {
		return strings.Contains(logs.String(), `"msg":"retention run"`)
	})
	cancel()
	<-done
	if _, err := st.Mail(ctx, "pw_1"); err != store.ErrNotFound {
		t.Fatalf("record not deleted: %v", err)
	}
	if !strings.Contains(logs.String(), `"records_deleted":1`) || strings.Contains(logs.String(), "example.test") {
		t.Fatalf("logs = %s", logs.String())
	}
}

func TestRunnerReportsErrors(t *testing.T) {
	st, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "pw.db"), store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	st.Close()
	var logs testutil.SafeBuffer
	r := New(Options{Store: st, Logger: testutil.Logger(&logs), Policy: store.RetainPolicy{RawMaxAge: time.Hour}})
	if _, err := r.Once(context.Background()); err == nil {
		t.Fatal("no error on a closed store")
	}
	if !strings.Contains(logs.String(), "retention run incomplete") {
		t.Fatalf("logs = %s", logs.String())
	}
}
