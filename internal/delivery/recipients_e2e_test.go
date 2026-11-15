package delivery

import (
	"context"
	"slices"
	"testing"

	"github.com/emersion/go-smtp"

	"github.com/eneuberg/postwerk/internal/config"
	"github.com/eneuberg/postwerk/internal/store"
)

var (
	rcpt550 = &smtp.SMTPError{Code: 550, EnhancedCode: smtp.EnhancedCode{5, 1, 1}, Message: "no such user"}
	rcpt451 = &smtp.SMTPError{Code: 451, EnhancedCode: smtp.EnhancedCode{4, 2, 0}, Message: "try later"}
	rcpt552 = &smtp.SMTPError{Code: 552, EnhancedCode: smtp.EnhancedCode{5, 5, 3}, Message: "too many recipients"}
)

func recipientStatuses(d store.Delivery) []store.Status {
	var out []store.Status
	for _, r := range d.Recipients {
		out = append(out, r.Status)
	}
	return out
}

// resultEvents lists "kind/position" of the events of one target that name a
// recipient.
func resultEvents(h *harness, mailID, target string) []string {
	var out []string
	for _, e := range h.eventsOf(mailID) {
		if e.Target == target && e.Recipient != nil {
			out = append(out, e.Kind+"/"+string(rune('0'+*e.Recipient)))
		}
	}
	return out
}

func TestPerRecipientResults(t *testing.T) {
	f := newTarget(t, plain, nil, "", "")
	f.set(func(b *behavior) {
		b.rcptErrs = map[string][]error{
			canary:               {rcpt550},
			"later@example.test": {rcpt451},
			"many@example.test":  {rcpt552}, // 552 at RCPT is temporary (RFC 5321 4.5.3.1.10)
		}
	})
	h := startPostwerk(t, []config.Target{target("t", f, plain)}, harnessOpts{store: store.Options{PurgeRawOnHandoff: true}})

	rcpts := []string{canary, "ok@example.test", "later@example.test", "many@example.test"}
	mailID := h.send(rcpts, testMsg)
	d := h.waitStatus(mailID, "t", store.Partial)
	if d.Attempts != 2 || !slices.Equal(recipientStatuses(d),
		[]store.Status{store.Failed, store.HandedOff, store.HandedOff, store.HandedOff}) {
		t.Fatalf("delivery = %+v", d)
	}
	if d.Recipients[0].LastCode != 550 || d.Recipients[2].LastCode != 250 {
		t.Fatalf("recipients = %+v", d.Recipients)
	}
	got := f.got()
	if len(got) != 2 || !slices.Equal(got[0].to, []string{"ok@example.test"}) ||
		!slices.Equal(got[1].to, []string{"later@example.test", "many@example.test"}) {
		t.Fatalf("target received %+v", got)
	}
	want := []string{
		"delivery.failed/0", "delivery.retry/2", "delivery.retry/3", "delivery.handed_off/1",
		"delivery.handed_off/2", "delivery.handed_off/3",
	}
	if ev := resultEvents(h, mailID, "t"); !slices.Equal(ev, want) {
		t.Fatalf("events = %q", ev)
	}
	// A recipient failed: the message is kept for a resend (failure rule),
	// not purged at hand-off.
	if rec, _ := h.store.Mail(context.Background(), mailID); !rec.HasRaw {
		t.Fatal("message purged although a recipient failed")
	}
}

func TestAllRecipientsRejected(t *testing.T) {
	f := newTarget(t, plain, nil, "", "")
	f.set(func(b *behavior) { b.rcptErr = rcpt550 })
	h := startPostwerk(t, []config.Target{target("t", f, plain)}, harnessOpts{})

	mailID := h.send([]string{canary, "b@example.test"}, testMsg)
	d := h.waitStatus(mailID, "t", store.Failed)
	if d.Attempts != 1 || !d.DataSent.IsZero() || d.LastCode != 550 {
		t.Fatalf("delivery = %+v", d)
	}
	if len(f.got()) != 0 {
		t.Fatal("DATA sent although every recipient was rejected")
	}
}

func TestUnclearOutcomeOnlyForThatAttempt(t *testing.T) {
	f := newTarget(t, plain, nil, "", "")
	// First attempt: recipient 1 is deferred, the message to recipient 0 is
	// cut after DATA. Second attempt: recipient 1 alone, delivered.
	f.set(func(b *behavior) {
		b.rcptErrs = map[string][]error{"later@example.test": {rcpt451}}
		b.drop = 1
	})
	h := startPostwerk(t, []config.Target{target("t", f, plain)}, harnessOpts{})

	mailID := h.send([]string{canary, "later@example.test"}, testMsg)
	d := h.waitStatus(mailID, "t", store.Unknown)
	if !slices.Equal(recipientStatuses(d), []store.Status{store.Unknown, store.HandedOff}) || d.Attempts != 2 {
		t.Fatalf("delivery = %+v", d)
	}
	if got := f.got(); len(got) != 2 || !slices.Equal(got[1].to, []string{"later@example.test"}) {
		t.Fatalf("target received %+v", got)
	}
}

func TestUTF8RecipientFailsAlone(t *testing.T) {
	f := newTarget(t, plain, nil, "", "")
	f.set(func(b *behavior) { b.hide = []string{"SMTPUTF8"} })
	h := startPostwerk(t, []config.Target{target("t", f, plain)}, harnessOpts{})

	mailID := h.sendWith(&smtp.MailOptions{UTF8: true}, []string{utf8Canary, "ok@example.test"}, testMsg)
	d := h.waitStatus(mailID, "t", store.Partial)
	if !slices.Equal(recipientStatuses(d), []store.Status{store.Failed, store.HandedOff}) {
		t.Fatalf("delivery = %+v", d)
	}
	if got := f.got(); len(got) != 1 || !slices.Equal(got[0].to, []string{"ok@example.test"}) || got[0].opts.UTF8 {
		t.Fatalf("target received %+v", got)
	}
	if ev := resultEvents(h, mailID, "t"); !slices.Equal(ev, []string{"delivery.unsupported/0", "delivery.handed_off/1"}) {
		t.Fatalf("events = %q", ev)
	}
}
