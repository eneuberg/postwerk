package delivery

import (
	"strings"
	"testing"

	"github.com/emersion/go-smtp"

	"github.com/eneuberg/postwerk/internal/config"
	"github.com/eneuberg/postwerk/internal/store"
)

const utf8Msg = "From: Sender <noreply@example.test>\r\n" +
	"To: <" + utf8Canary + ">\r\n" +
	"Subject: Grüße\r\n" +
	"\r\n" +
	"Schöne Grüße\r\n"

func eventOf(h *harness, mailID, target, kind string) *store.Event {
	for _, e := range h.eventsOf(mailID) {
		if e.Target == target && e.Kind == kind {
			return &e
		}
	}
	return nil
}

func TestEnvelopeParamsPassedOn(t *testing.T) {
	full := newTarget(t, plain, nil, "", "")
	noUTF8 := newTarget(t, plain, nil, "", "")
	noUTF8.set(func(b *behavior) { b.hide = []string{"SMTPUTF8"} })
	no8Bit := newTarget(t, plain, nil, "", "")
	no8Bit.set(func(b *behavior) { b.hide = []string{"SMTPUTF8", "8BITMIME"} })
	h := startPostwerk(t, []config.Target{
		target("full", full, plain),
		target("noutf8", noUTF8, plain),
		target("no8bit", no8Bit, plain, func(t *config.Target) { t.Optional = true }),
	}, harnessOpts{})

	utf8 := &smtp.MailOptions{UTF8: true}
	// UTF-8 address and header: only a target with SMTPUTF8 may get it.
	intl := h.sendWith(utf8, []string{utf8Canary}, utf8Msg)
	// Declared SMTPUTF8 but pure ASCII (as Go's net/smtp does): the
	// parameter is dropped where the target lacks it, nothing is lost.
	ascii := h.sendWith(utf8, []string{canary}, testMsg)
	// 8-bit body, ASCII header and envelope: needs 8BITMIME only.
	eight := h.send([]string{canary}, strings.Replace(testMsg, "Line one", "Grüße", 1))

	h.waitStatus(intl, "full", store.HandedOff)
	for _, id := range []string{ascii, eight} {
		h.waitStatus(id, "full", store.HandedOff)
		h.waitStatus(id, "noutf8", store.HandedOff)
	}
	h.waitStatus(ascii, "no8bit", store.HandedOff)

	d := h.waitStatus(intl, "noutf8", store.Failed)
	if d.Attempts != 1 || !d.DataSent.IsZero() {
		t.Fatalf("noutf8 = %+v", d)
	}
	ev := eventOf(h, intl, "noutf8", store.KindDeliveryUnsupported)
	if ev == nil || ev.Level != store.Error || !strings.Contains(ev.Text, "SMTPUTF8") {
		t.Fatalf("unsupported event = %+v", ev)
	}
	h.waitStatus(eight, "no8bit", store.Failed)
	ev = eventOf(h, eight, "no8bit", store.KindDeliveryUnsupported)
	if ev == nil || ev.Level != store.Warn || !strings.Contains(ev.Text, "8BITMIME") {
		t.Fatalf("unsupported event (optional target) = %+v", ev)
	}

	got := full.got()
	if len(got) != 3 {
		t.Fatalf("full target got %d mails", len(got))
	}
	for _, r := range got {
		if !strings.Contains(string(r.data), "X-Postwerk-Id: ") || r.opts.Body != smtp.Body8BitMIME {
			t.Fatalf("full target received %+v", r.opts)
		}
	}
	if !got[0].opts.UTF8 || got[0].to[0] != utf8Canary || !strings.Contains(string(got[0].data), "Subject: Grüße") {
		t.Fatalf("international mail arrived as %+v %q", got[0].opts, got[0].to)
	}
	for _, r := range noUTF8.got() {
		if r.opts.UTF8 {
			t.Fatal("SMTPUTF8 sent to a target that does not offer it")
		}
		if strings.Contains(string(r.data), "Subject: Grüße") {
			t.Fatal("UTF-8 header sent to a target without SMTPUTF8")
		}
	}
	if r := no8Bit.got(); len(r) != 1 || r[0].opts.Body != "" {
		t.Fatalf("7-bit target got %+v", r)
	}
}

func TestNoUTF8AddressLeaks(t *testing.T) {
	// The leak check at cleanup also looks for the internationalized canary
	// and its parts; here every reply quotes it.
	rejecting := newTarget(t, plain, nil, "", "")
	rejecting.set(func(b *behavior) {
		b.rcptErr = &smtp.SMTPError{Code: 550, EnhancedCode: smtp.EnhancedCode{5, 1, 1},
			Message: "<" + utf8Canary + ">: unknown user " + utf8Canary}
	})
	later := newTarget(t, plain, nil, "", "")
	later.set(func(b *behavior) {
		b.dataErrs = []error{&smtp.SMTPError{Code: 451, EnhancedCode: smtp.EnhancedCode{4, 2, 0},
			Message: "mailbox " + utf8Canary + "　busy"}}
	})
	h := startPostwerk(t, []config.Target{target("rejecting", rejecting, plain), target("later", later, plain)}, harnessOpts{})

	mailID := h.sendWith(&smtp.MailOptions{UTF8: true}, []string{utf8Canary}, utf8Msg)
	d := h.waitStatus(mailID, "rejecting", store.Failed)
	if d.LastText != "5.1.1 <redacted>: unknown user <redacted>" {
		t.Fatalf("rejecting = %+v", d)
	}
	h.waitStatus(mailID, "later", store.HandedOff)
	if ev := eventOf(h, mailID, "later", store.KindDeliveryRetry); ev == nil || ev.Text != "4.2.0 mailbox <redacted> busy" {
		t.Fatalf("retry event = %+v", ev)
	}
}

func TestMessageSize(t *testing.T) {
	small := newTarget(t, plain, nil, "", "")
	small.set(func(b *behavior) { b.size = "1000" })
	big := newTarget(t, plain, nil, "", "")
	big.set(func(b *behavior) { b.size = "15728640" })
	h := startPostwerk(t, []config.Target{target("small", small, plain), target("big", big, plain)}, harnessOpts{})

	large := testMsg + strings.Repeat(strings.Repeat("x", 70)+"\r\n", 20) // about 1.6 kB
	tooLarge := h.send([]string{canary, "b@example.test"}, large)
	fits := h.send([]string{canary}, testMsg)

	d := h.waitStatus(tooLarge, "small", store.Failed)
	if d.Attempts != 1 || !d.DataSent.IsZero() {
		t.Fatalf("small = %+v", d)
	}
	if ev := eventOf(h, tooLarge, "small", store.KindDeliveryTooLarge); ev == nil || ev.Level != store.Error ||
		!strings.Contains(ev.Text, "at most 1000") {
		t.Fatalf("too_large event = %+v", ev)
	}
	h.waitStatus(tooLarge, "big", store.HandedOff)
	h.waitStatus(fits, "small", store.HandedOff)
	if got := small.got(); len(got) != 1 || got[0].opts.Size != int64(len(got[0].data)) {
		t.Fatalf("small target got %d mails, SIZE=%d", len(got), got[0].opts.Size)
	}
	for _, r := range big.got() {
		if r.opts.Size != int64(len(r.data)) {
			t.Fatalf("SIZE=%d, message has %d bytes", r.opts.Size, len(r.data))
		}
	}
}
