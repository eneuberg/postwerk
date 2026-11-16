package resolve

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/eneuberg/postwerk/internal/query"
	"github.com/eneuberg/postwerk/internal/store"
)

func TestCheckNote(t *testing.T) {
	for in, want := range map[string]error{
		"":                             nil,
		"  checked at the provider  ":  nil,
		strings.Repeat("ü", MaxNote):   nil,
		strings.Repeat("x", MaxNote+1): ErrNoteTooLong,
		"two\nlines":                   ErrNoteInvalid,
		"tab\there":                    ErrNoteInvalid,
		"line separator":               ErrNoteInvalid,
		"\xff":                         ErrNoteInvalid,
	} {
		if _, err := CheckNote(in); !errors.Is(err, want) {
			t.Errorf("CheckNote(%q) = %v, want %v", in, err, want)
		}
	}
	if got, _ := CheckNote("  ok  "); got != "ok" {
		t.Errorf("not trimmed: %q", got)
	}
}

func TestMessagesForEveryError(t *testing.T) {
	for _, err := range []error{store.ErrNoDelivery, store.ErrInProgress, store.ErrNothingToClose, store.ErrConfirmQueued,
		store.ErrNothingToResend, store.ErrRawGone, store.ErrDuplicateRisk, ErrResolution, ErrNoteTooLong, ErrNoteInvalid,
		ErrNotConfigured} {
		if Message(err) == "" || Message(errors.Join(errors.New("wrapped"), err)) == "" {
			t.Errorf("no message for %v", err)
		}
	}
	if Message(errors.New("disk full")) != "" {
		t.Error("message for an internal error")
	}
}

func TestServiceRules(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "pw.db"), store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	at := time.Date(2026, 11, 15, 12, 0, 0, 0, time.UTC)
	m := store.NewMail{ID: "pw_1", ReceivedAt: at, Sender: "app", EnvelopeFrom: "from@example.test",
		Recipients: []string{"a@example.test"}, Raw: []byte("x"),
		Deliveries: []store.NewDelivery{{Target: "live", Status: store.Queued}, {Target: "gone", Status: store.Queued}}}
	if err := st.Accept(ctx, m); err != nil {
		t.Fatal(err)
	}
	wakes := 0
	s := New(Options{Store: st, Targets: []string{"live"}, Wake: func() { wakes++ }, Now: func() time.Time { return at }})
	q := query.New(query.Options{Store: st, Targets: []string{"live"}})

	if _, err := s.Close(ctx, "pw_1", "live", "delivered", "", "ops", ViaUI); !errors.Is(err, ErrResolution) {
		t.Errorf("bad resolution: %v", err)
	}
	if _, err := s.Close(ctx, "pw_1", "live", store.GivenUp, "a\nb", "ops", ViaUI); !errors.Is(err, ErrNoteInvalid) {
		t.Errorf("bad note: %v", err)
	}
	if _, err := s.Resend(ctx, "pw_1", "gone", true, "ops", ViaUI); !errors.Is(err, ErrNotConfigured) {
		t.Errorf("resend to an unconfigured target: %v", err)
	}
	if _, err := s.Resend(ctx, "pw_1", "never", true, "ops", ViaUI); !errors.Is(err, store.ErrNoDelivery) {
		t.Errorf("resend to a target without delivery: %v", err)
	}

	d, err := q.Mail(ctx, "pw_1", query.Full)
	if err != nil {
		t.Fatal(err)
	}
	for _, dl := range d.Deliveries {
		c, r := s.Available(dl, d.Stored)
		// Queued: may be given up, not confirmed, and nothing to resend.
		if !c.OK || c.ConfirmOK || c.Recipients != 1 || r.OK || r.Reason == "" {
			t.Errorf("%s queued: close %+v resend %+v", dl.Target, c, r)
		}
	}
	if _, err := s.Close(ctx, "pw_1", "live", store.GivenUp, "", "ops", ViaUI); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Resend(ctx, "pw_1", "live", false, "ops", ViaUI); !errors.Is(err, store.ErrNothingToResend) {
		t.Errorf("resend of a closed delivery: %v", err)
	}
	if wakes != 0 {
		t.Errorf("wakes = %d", wakes)
	}
}
