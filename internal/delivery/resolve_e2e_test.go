package delivery

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/eneuberg/postwerk/internal/config"
	"github.com/eneuberg/postwerk/internal/store"
)

// An admin's resend while a worker holds the delivery must never send a
// recipient twice: the store refuses every close and resend while the
// recipients are in sending, and of several resends at once only one finds
// anything to queue (D54).
func TestResendWithRunningWorker(t *testing.T) {
	f := newTarget(t, plain, nil, "", "")
	f.set(func(b *behavior) { b.drop = 1 })
	h := startPostwerk(t, []config.Target{target("held", f, plain)},
		harnessOpts{store: store.Options{PurgeRawOnHandoff: true}})
	ctx := context.Background()

	mailID := h.send([]string{canary}, testMsg)
	h.waitStatus(mailID, "held", store.Unknown)
	if n := len(f.got()); n != 1 {
		t.Fatalf("target got %d copies before the resend", n)
	}

	// Several admins resend at once; the target holds the message it gets.
	release, holding := make(chan struct{}), make(chan struct{})
	f.set(func(b *behavior) { b.hold, b.holding = release, holding })
	var wg sync.WaitGroup
	var mu sync.Mutex
	var ok, nothing int
	for range 8 {
		wg.Go(func() {
			_, err := h.store.ResendDelivery(ctx, store.Resend{MailID: mailID, Target: "held", AcceptDuplicate: true,
				Actor: "ops", Via: "ui", At: time.Now()})
			mu.Lock()
			defer mu.Unlock()
			switch {
			case err == nil:
				ok++
			case errors.Is(err, store.ErrNothingToResend), errors.Is(err, store.ErrInProgress):
				nothing++
			default:
				t.Errorf("resend: %v", err)
			}
		})
	}
	wg.Wait()
	if ok != 1 || nothing != 7 {
		t.Fatalf("resends at once: %d queued, %d refused; want 1 and 7", ok, nothing)
	}
	h.mgr.Wake()
	select {
	case <-holding:
	case <-time.After(timeout):
		t.Fatal("the worker never sent the resent mail")
	}

	// The worker holds the delivery: no resend and no close gets through.
	for range 5 {
		if _, err := h.store.ResendDelivery(ctx, store.Resend{MailID: mailID, Target: "held", AcceptDuplicate: true,
			Actor: "ops", Via: "api", At: time.Now()}); !errors.Is(err, store.ErrInProgress) {
			t.Fatalf("resend while sending: %v", err)
		}
		if _, err := h.store.CloseDelivery(ctx, store.Close{MailID: mailID, Target: "held", Resolution: store.GivenUp,
			Actor: "ops", Via: "api", At: time.Now()}); !errors.Is(err, store.ErrInProgress) {
			t.Fatalf("close while sending: %v", err)
		}
	}
	f.set(func(b *behavior) { b.hold, b.holding = nil, nil })
	close(release)
	h.waitStatus(mailID, "held", store.HandedOff)

	// Handed off: nothing more to resend, and the target got exactly one
	// copy more than before.
	if _, err := h.store.ResendDelivery(ctx, store.Resend{MailID: mailID, Target: "held", AcceptDuplicate: true,
		At: time.Now()}); !errors.Is(err, store.ErrNothingToResend) {
		t.Fatalf("resend after hand-off: %v", err)
	}
	time.Sleep(150 * time.Millisecond)
	if n := len(f.got()); n != 2 {
		t.Fatalf("target got %d copies, want 2 (the dropped one and one resend)", n)
	}
	var resends, attempts int
	for _, e := range h.eventsOf(mailID) {
		switch e.Kind {
		case store.KindDeliveryResend:
			resends++
			if e.Actor != "ops" || e.Text != "was=unknown via=ui" {
				t.Errorf("resend event: %+v", e)
			}
		case store.KindDeliveryAttempt:
			attempts++
		}
	}
	if resends != 1 || attempts != 2 {
		t.Fatalf("%d resend events, %d attempts; want 1 and 2", resends, attempts)
	}
	m, err := h.store.Mail(ctx, mailID)
	if err != nil || m.HasRaw {
		t.Fatalf("raw after the resend was handed off: %v %v", m.HasRaw, err)
	}
}

// A closed unknown delivery is never picked up by a worker again.
func TestClosedDeliveryIsNotSent(t *testing.T) {
	f := newTarget(t, plain, nil, "", "")
	f.set(func(b *behavior) { b.drop = 1 })
	h := startPostwerk(t, []config.Target{target("held", f, plain)}, harnessOpts{})
	ctx := context.Background()
	mailID := h.send([]string{canary}, testMsg)
	h.waitStatus(mailID, "held", store.Unknown)
	if _, err := h.store.CloseDelivery(ctx, store.Close{MailID: mailID, Target: "held", Resolution: store.Confirmed,
		Actor: "ops", Via: "ui", At: time.Now()}); err != nil {
		t.Fatal(err)
	}
	h.mgr.Wake()
	time.Sleep(200 * time.Millisecond)
	if n := len(f.got()); n != 1 {
		t.Fatalf("target got %d copies after the close", n)
	}
	if d := h.delivery(mailID, "held"); d.Status != store.HandedOff || d.Recipients[0].Resolution != store.Confirmed {
		t.Fatalf("delivery after close: %+v", d)
	}
	if m, _ := h.store.Mail(ctx, mailID); m.HasRaw {
		t.Fatal("raw kept after the close")
	}
}
