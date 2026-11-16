package report

import (
	"os"
	"testing"
	"time"

	"github.com/eneuberg/postwerk/internal/config"
	"github.com/eneuberg/postwerk/internal/store"
)

// TestSentryLive sends real events to the DSN in SENTRY_LIVE_DSN, for
// checking the payload against a running Sentry or GlitchTip:
//
//	SENTRY_LIVE_DSN=http://<key>@localhost:8000/1 go test ./internal/report -run TestSentryLive -v
//
// It sends one error, the same error again (same event id, so the receiver
// keeps one), one warning and one UI event, all with SENTRY_LIVE_INSTANCE
// (default "live-test") as the environment.
func TestSentryLive(t *testing.T) {
	raw := os.Getenv("SENTRY_LIVE_DSN")
	if raw == "" {
		t.Skip("SENTRY_LIVE_DSN not set")
	}
	dsn, err := config.ParseSentryDSN(raw, true)
	if err != nil {
		t.Fatal(err)
	}
	instance := os.Getenv("SENTRY_LIVE_INSTANCE")
	if instance == "" {
		instance = "live-test"
	}
	s := NewSentry(SentryOptions{DSN: dsn, MinLevel: store.Warn, Instance: instance, Version: "live-test"})
	now := time.Now()
	failed := failedEvent()
	failed.Time = now
	failed.Seq = now.UnixMilli()
	retry := store.Event{Seq: failed.Seq + 1, Time: now, Level: store.Warn, Kind: "delivery.retry", MailID: failed.MailID,
		Target: "mailjet", Recipient: store.Position(0), Attempt: 1, Code: 451, Text: "4.7.1 try again later", Sender: "auth-service"}
	ui := store.Event{Seq: failed.Seq + 2, Time: now, Level: store.Warn, Kind: "ui.login_failed", Actor: "ops"}
	for _, e := range []store.Event{failed, failed, retry, ui} {
		if err := s.Export(ctx, e); err != nil {
			t.Fatalf("seq %d: %v", e.Seq, err)
		}
		t.Logf("sent seq %d as %s", e.Seq, EventID(instance, e))
	}
}
