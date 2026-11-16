package ui

import (
	"context"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/eneuberg/postwerk/internal/store"
)

// recipientStates returns "status" or "status/resolution" per recipient of
// one delivery.
func (h *harness) recipientStates(id, target string) []string {
	h.t.Helper()
	ds, err := h.st.Deliveries(context.Background(), id)
	if err != nil {
		h.t.Fatal(err)
	}
	for _, d := range ds {
		if d.Target == target {
			var out []string
			for _, r := range d.Recipients {
				v := string(r.Status)
				if r.Resolution != "" {
					v += "/" + string(r.Resolution)
				}
				out = append(out, v)
			}
			return out
		}
	}
	h.t.Fatalf("no delivery %s/%s", id, target)
	return nil
}

func (h *harness) hasRaw(id string) bool {
	h.t.Helper()
	m, err := h.st.Mail(context.Background(), id)
	if err != nil {
		h.t.Fatal(err)
	}
	return m.HasRaw
}

const (
	unknownMail = "pw_01KUNKNOWN0000000000000001"
	failedMail  = "pw_01KFAILED00000000000000001"
)

func seedProblems(h *harness) {
	at := h.clock.Now().Add(-time.Hour)
	h.seedMail(unknownMail, at, []string{"jane@example.com"},
		map[string][]store.Status{"mailjet": {store.Unknown}, "capture": {store.HandedOff}})
	h.seedMail(failedMail, at, []string{"max@example.com", "eva@example.org"},
		map[string][]store.Status{"mailjet": {store.HandedOff, store.Failed}})
}

func TestMailPageOffersActions(t *testing.T) {
	h := newHarness(t)
	seedProblems(h)
	b := h.loggedIn("ops", adminToken)

	page := b.get("/mails/" + unknownMail)
	for _, want := range []string{
		`action="/mails/` + unknownMail + `/targets/mailjet/resend"`,
		`action="/mails/` + unknownMail + `/targets/mailjet/close"`,
		`name="duplicate" value="yes" required`,
		"may already have this mail",
		`value="confirmed">`,
	} {
		if !strings.Contains(page.body, want) {
			t.Errorf("unknown mail page lacks %q", want)
		}
	}
	// capture handed it off: nothing to resolve there, so nothing is shown.
	if strings.Contains(page.body, `/targets/capture/resend"`) || strings.Contains(page.body, `/targets/capture/close"`) ||
		strings.Contains(page.body, "Admin actions for capture") || strings.Contains(page.body, "Nothing to") {
		t.Error("actions shown for a handed-off delivery")
	}
	page = b.get("/mails/" + failedMail)
	if !strings.Contains(page.body, `/targets/mailjet/resend"`) || strings.Contains(page.body, `name="duplicate"`) {
		t.Error("failed delivery: resend form missing or asks for a duplicate consent")
	}
	if !strings.Contains(page.body, "Queues the 1 recipient that is failed or unknown") {
		t.Error("resend does not say how many recipients it sends")
	}
}

func TestCloseFromUI(t *testing.T) {
	h := newHarness(t)
	seedProblems(h)
	b := h.loggedIn("ops", adminToken)
	path := "/mails/" + unknownMail + "/targets/mailjet/close"
	page := "/mails/" + unknownMail
	post := func(form url.Values) response { return b.post(page, path, form) }

	for _, c := range []struct {
		name   string
		form   url.Values
		status int
		msg    string
	}{
		{"no resolution", url.Values{"current": {adminToken}}, 400, "Choose a resolution"},
		{"bad resolution", url.Values{"resolution": {"delivered"}, "current": {adminToken}}, 400, "Choose a resolution"},
		{"long note", url.Values{"resolution": {"confirmed"}, "note": {strings.Repeat("x", 201)}, "current": {adminToken}}, 400, "too long"},
		{"note with newline", url.Values{"resolution": {"confirmed"}, "note": {"a\nb"}, "current": {adminToken}}, 400, "plain text"},
		{"wrong password", url.Values{"resolution": {"confirmed"}, "current": {"wrong"}}, 403, "Your current password is wrong."},
		{"no password", url.Values{"resolution": {"confirmed"}}, 403, "Your current password is wrong."},
	} {
		r := post(c.form)
		if r.StatusCode != c.status || !strings.Contains(r.body, c.msg) {
			t.Errorf("%s: %d, want %d %q", c.name, r.StatusCode, c.status, c.msg)
		}
		if got := h.recipientStates(unknownMail, "mailjet"); !slices.Equal(got, []string{"unknown"}) {
			t.Fatalf("%s changed the delivery: %v", c.name, got)
		}
	}
	// Without the CSRF token nothing happens either.
	if r := b.do("POST", path, url.Values{"resolution": {"confirmed"}, "current": {adminToken}}); r.StatusCode != 403 {
		t.Fatalf("close without csrf: %d", r.StatusCode)
	}
	if !h.hasRaw(unknownMail) {
		t.Fatal("raw removed by a refused close")
	}

	r := post(url.Values{"resolution": {"confirmed"}, "note": {"found in the provider log for jane@example.com"}, "current": {adminToken}})
	if r.StatusCode != http.StatusSeeOther || r.Header.Get("Location") != page {
		t.Fatalf("close: %d %s", r.StatusCode, r.body)
	}
	if got := h.recipientStates(unknownMail, "mailjet"); !slices.Equal(got, []string{"handed_off/confirmed"}) {
		t.Fatalf("after close: %v", got)
	}
	if h.hasRaw(unknownMail) {
		t.Fatal("raw kept after the last open delivery was closed")
	}
	shown := b.get(page)
	for _, want := range []string{"Delivery to mailjet closed as confirmed (1 recipient).", "closed: confirmed",
		"delivery.closed</span> <span class=\"muted\">by ops", "resolution=confirmed was=unknown via=ui: found in the provider log for &lt;redacted&gt;"} {
		if !strings.Contains(shown.body, want) {
			t.Errorf("page after close lacks %q", want)
		}
	}
	if strings.Contains(shown.body, "for jane@example.com") {
		t.Error("the note kept the address")
	}
	if got := h.uiEvents(store.KindDeliveryClosed); len(got) != 1 || !strings.HasPrefix(got[0], "delivery.closed/ops/resolution=confirmed") {
		t.Fatalf("events: %v", got)
	}
	// A failed password check is a ui.login_failed with text confirm.
	if got := h.uiEvents(store.KindUILoginFailed); len(got) != 2 || got[0] != "ui.login_failed/ops/confirm" {
		t.Errorf("failed checks: %v", got)
	}
	// Closed is final.
	if r := post(url.Values{"resolution": {"given_up"}, "current": {adminToken}}); r.StatusCode != 409 ||
		!strings.Contains(r.body, "Nothing to close") {
		t.Fatalf("second close: %d", r.StatusCode)
	}
}

func TestResendFromUI(t *testing.T) {
	h := newHarness(t)
	seedProblems(h)
	b := h.loggedIn("viewer", viewerPassword)
	page := "/mails/" + unknownMail
	path := page + "/targets/mailjet/resend"

	r := b.post(page, path, url.Values{"current": {viewerPassword}})
	if r.StatusCode != 409 || !strings.Contains(r.body, "may deliver it twice") {
		t.Fatalf("resend of unknown without consent: %d", r.StatusCode)
	}
	if h.wakes.Load() != 0 {
		t.Fatal("refused resend woke the workers")
	}
	r = b.post(page, path, url.Values{"duplicate": {"yes"}, "current": {"wrong"}})
	if r.StatusCode != 403 {
		t.Fatalf("resend with a wrong password: %d", r.StatusCode)
	}
	r = b.post(page, path, url.Values{"duplicate": {"yes"}, "current": {viewerPassword}})
	if r.StatusCode != http.StatusSeeOther {
		t.Fatalf("resend: %d %s", r.StatusCode, r.body)
	}
	if got := h.recipientStates(unknownMail, "mailjet"); !slices.Equal(got, []string{"queued"}) {
		t.Fatalf("after resend: %v", got)
	}
	if h.wakes.Load() != 1 {
		t.Fatalf("wakes = %d", h.wakes.Load())
	}
	if got := h.uiEvents(store.KindDeliveryResend); !slices.Equal(got, []string{"delivery.resend/viewer/was=unknown via=ui"}) {
		t.Fatalf("events: %v", got)
	}
	if shown := b.get(page); !strings.Contains(shown.body, "Delivery to mailjet queued again (1 recipient).") ||
		!strings.Contains(shown.body, "Nothing to resend") || !strings.Contains(shown.body, "Close…") {
		t.Error("page after resend")
	}
	// The resent delivery is queued: it can be given up, not confirmed.
	shown := b.get(page)
	if !strings.Contains(shown.body, `value="confirmed" disabled>`) {
		t.Error("confirmed offered for a queued delivery")
	}
	r = b.post(page, page+"/targets/mailjet/close", url.Values{"resolution": {"confirmed"}, "current": {viewerPassword}})
	if r.StatusCode != 409 || !strings.Contains(r.body, "cannot be confirmed") {
		t.Fatalf("confirm a queued delivery: %d", r.StatusCode)
	}
}

// Once retention removed the message, resend is not offered and refused;
// close still works.
func TestResendAfterRawExpiry(t *testing.T) {
	h := newHarness(t)
	seedProblems(h)
	if _, err := h.st.Retain(context.Background(), store.RetainPolicy{RawKeepAfterFailure: time.Minute,
		RawMaxAge: time.Hour, RecordRetention: 90 * 24 * time.Hour, EventRetention: 90 * 24 * time.Hour}, h.clock.Now()); err != nil {
		t.Fatal(err)
	}
	if h.hasRaw(unknownMail) || !slices.Equal(h.recipientStates(unknownMail, "mailjet"), []string{"failed"}) {
		t.Fatal("seed: raw not expired")
	}
	b := h.loggedIn("ops", adminToken)
	page := "/mails/" + unknownMail
	shown := b.get(page)
	if strings.Contains(shown.body, `/targets/mailjet/resend"`) || !strings.Contains(shown.body, "The stored message was removed") {
		t.Fatal("resend offered without the message")
	}
	if r := b.post(page, page+"/targets/mailjet/resend", url.Values{"duplicate": {"yes"}, "current": {adminToken}}); r.StatusCode != 409 {
		t.Fatalf("resend after expiry: %d", r.StatusCode)
	}
	// The record said failed after the expiry; the admin checked and closes it.
	r := b.post(page, page+"/targets/mailjet/close", url.Values{"resolution": {"confirmed"}, "current": {adminToken}})
	if r.StatusCode != http.StatusSeeOther {
		t.Fatalf("close after expiry: %d %s", r.StatusCode, r.body)
	}
	if got := h.recipientStates(unknownMail, "mailjet"); !slices.Equal(got, []string{"handed_off/confirmed"}) {
		t.Fatalf("after close: %v", got)
	}
}

func TestActionsWhileSendingAndUnknownPaths(t *testing.T) {
	h := newHarness(t)
	seedProblems(h)
	b := h.loggedIn("ops", adminToken)
	page := "/mails/" + failedMail
	// A worker claims a resent delivery and holds it.
	if r := b.post(page, page+"/targets/mailjet/resend", url.Values{"current": {adminToken}}); r.StatusCode != http.StatusSeeOther {
		t.Fatalf("resend: %d", r.StatusCode)
	}
	if c, err := h.st.Claim(context.Background(), "mailjet", h.clock.Now().Add(time.Hour)); err != nil || c == nil {
		t.Fatalf("claim: %v %v", c, err)
	}
	if shown := b.get(page); !strings.Contains(shown.body, "being sent right now") {
		t.Error("page does not say the delivery is being sent")
	}
	for _, action := range []string{"resend", "close"} {
		r := b.post(page, page+"/targets/mailjet/"+action, url.Values{"resolution": {"given_up"}, "current": {adminToken}})
		if r.StatusCode != 409 || !strings.Contains(r.body, "being sent right now") {
			t.Errorf("%s while sending: %d", action, r.StatusCode)
		}
	}
	for _, p := range []string{
		"/mails/" + failedMail + "/targets/nope/close",
		"/mails/pw_01KNOSUCHMAIL0000000000001/targets/mailjet/close",
		"/mails/" + failedMail + "/targets/Bad-Name/close",
	} {
		if r := b.post(page, p, url.Values{"resolution": {"given_up"}, "current": {adminToken}}); r.StatusCode != 404 {
			t.Errorf("%s: %d", p, r.StatusCode)
		}
	}
	if r := b.do("GET", page+"/targets/mailjet/close", nil); r.StatusCode != 405 {
		t.Errorf("GET close: %d", r.StatusCode)
	}
}

func TestResendToUnconfiguredTarget(t *testing.T) {
	h := newHarness(t)
	h.seedMail(failedMail, h.clock.Now().Add(-time.Hour), []string{"max@example.com"},
		map[string][]store.Status{"retired": {store.Failed}})
	b := h.loggedIn("ops", adminToken)
	page := "/mails/" + failedMail
	if shown := b.get(page); !strings.Contains(shown.body, "no longer configured") {
		t.Error("page does not say the target is not configured")
	}
	if r := b.post(page, page+"/targets/retired/resend", url.Values{"current": {adminToken}}); r.StatusCode != 409 {
		t.Fatalf("resend to a retired target: %d", r.StatusCode)
	}
	// Giving it up works.
	if r := b.post(page, page+"/targets/retired/close", url.Values{"resolution": {"given_up"}, "current": {adminToken}}); r.StatusCode != http.StatusSeeOther {
		t.Fatalf("close: %d", r.StatusCode)
	}
}
