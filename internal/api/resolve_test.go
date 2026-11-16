package api

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/eneuberg/postwerk/internal/store"
)

func (h *harness) states(id string) []string {
	h.t.Helper()
	ds, err := h.st.Deliveries(context.Background(), id)
	if err != nil {
		h.t.Fatal(err)
	}
	var out []string
	for _, r := range ds[0].Recipients {
		v := string(r.Status)
		if r.Resolution != "" {
			v += "/" + string(r.Resolution)
		}
		out = append(out, v)
	}
	return out
}

// actionEvents returns "kind actor text" of the close and resend events.
func (h *harness) actionEvents() []string {
	h.t.Helper()
	var out []string
	for _, e := range h.events() {
		if e.Kind == store.KindDeliveryClosed || e.Kind == store.KindDeliveryResend {
			out = append(out, e.Kind+" "+e.Actor+" "+e.Text)
		}
	}
	return out
}

func TestActionRoles(t *testing.T) {
	h := newHarness(t)
	h.seed("pw_01", h.clock.Now().Add(-time.Hour), []string{"jane@example.test"}, []store.Status{store.Failed})
	for _, path := range []string{"/v1/mails/pw_01/targets/mailjet/close", "/v1/mails/pw_01/targets/mailjet/resend"} {
		r := h.do("POST", path, readBearer, `{"resolution": "given_up"}`)
		if r.StatusCode != 403 || !strings.Contains(r.body, `"forbidden"`) {
			t.Errorf("read token on %s: %d %s", path, r.StatusCode, r.body)
		}
		if r := h.do("POST", path, "", `{}`); r.StatusCode != 401 {
			t.Errorf("no token on %s: %d", path, r.StatusCode)
		}
		if r := h.do("GET", path, adminBearer, ""); r.StatusCode != 405 {
			t.Errorf("GET %s: %d", path, r.StatusCode)
		}
		if r := h.do("POST", path+"?resolution=given_up", adminBearer, `{}`); r.StatusCode != 400 {
			t.Errorf("URL parameters on %s: %d", path, r.StatusCode)
		}
		if r := h.do("POST", path, adminBearer, `{"resolution": "given_up", "extra": 1}`); r.StatusCode != 400 {
			t.Errorf("unknown field on %s: %d", path, r.StatusCode)
		}
	}
	if got := h.states("pw_01"); !slices.Equal(got, []string{"failed"}) {
		t.Fatalf("refused requests changed the delivery: %v", got)
	}
	if got := h.apiEvents(); !slices.Contains(got, "api.forbidden support close") || !slices.Contains(got, "api.forbidden support resend") {
		t.Errorf("events: %v", got)
	}
}

func TestCloseByAPI(t *testing.T) {
	h := newHarness(t)
	h.seed("pw_01", h.clock.Now().Add(-time.Hour), []string{"jane@example.test", "max@example.test"},
		[]store.Status{store.HandedOff, store.Unknown})
	path := "/v1/mails/pw_01/targets/mailjet/close"
	for _, c := range []struct {
		body   string
		status int
		code   string
	}{
		{`{"resolution": "delivered"}`, 400, "bad_request"},
		{`{}`, 400, "bad_request"},
		{`{"resolution": "confirmed", "note": "` + strings.Repeat("x", 201) + `"}`, 400, "bad_request"},
		{`{"resolution": "confirmed", "note": "a\u0007b"}`, 400, "bad_request"},
	} {
		r := h.do("POST", path, adminBearer, c.body)
		if r.StatusCode != c.status || !strings.Contains(r.body, `"`+c.code+`"`) {
			t.Errorf("%s: %d %s", c.body, r.StatusCode, r.body)
		}
	}
	if r := h.do("POST", "/v1/mails/pw_01/targets/other/close", adminBearer, `{"resolution": "given_up"}`); r.StatusCode != 404 {
		t.Errorf("unknown target: %d", r.StatusCode)
	}
	if r := h.do("POST", "/v1/mails/pw_99/targets/mailjet/close", adminBearer, `{"resolution": "given_up"}`); r.StatusCode != 404 {
		t.Errorf("unknown mail: %d", r.StatusCode)
	}

	r := h.do("POST", path, adminBearer, `{"resolution": "confirmed", "note": "seen in the provider log for max@example.test"}`)
	var res actionResponse
	r.json(t, &res)
	if r.StatusCode != 200 || res.ID != "pw_01" || res.Target != "mailjet" || !slices.Equal(res.Recipients, []int{1}) ||
		res.Delivery.Status != store.HandedOff || res.Delivery.Recipients[1].Resolution != store.Confirmed ||
		res.Delivery.Recipients[0].Resolution != "" {
		t.Fatalf("close: %d %s", r.StatusCode, r.body)
	}
	if got := h.actionEvents(); len(got) != 1 ||
		got[0] != "delivery.closed support.admin resolution=confirmed was=unknown via=api: seen in the provider log for <redacted>" {
		t.Fatalf("events: %v", got)
	}
	// The change's own event is the record: no api.request for it, one for
	// every refused request.
	var requests int
	for _, e := range h.apiEvents() {
		if strings.HasPrefix(e, "api.request support.admin close") {
			requests++
			if strings.Contains(e, "status=200") {
				t.Errorf("api.request for a recorded change: %s", e)
			}
		}
	}
	if requests != 6 {
		t.Errorf("%d api.request events for refused closes, want 6", requests)
	}
	m, err := h.st.Mail(context.Background(), "pw_01")
	if err != nil || m.HasRaw {
		t.Fatalf("raw after close: %v %v", m.HasRaw, err)
	}
	r = h.do("POST", path, adminBearer, `{"resolution": "given_up"}`)
	if r.StatusCode != 409 || !strings.Contains(r.body, `"nothing_to_close"`) {
		t.Fatalf("second close: %d %s", r.StatusCode, r.body)
	}

	// The detail shows the resolution and the close, with its actor only
	// for admin tokens.
	var d mailDetail
	h.do("GET", "/v1/mails/pw_01", adminBearer, "").json(t, &d)
	var closed []event
	for _, e := range d.Events {
		if e.Kind == store.KindDeliveryClosed {
			closed = append(closed, e)
		}
	}
	if len(closed) != 1 || closed[0].Actor != "support.admin" || d.Deliveries[0].Recipients[1].Resolution != store.Confirmed {
		t.Fatalf("admin detail: %+v", d.Events)
	}
	var masked mailDetail
	h.do("GET", "/v1/mails/pw_01", readBearer, "").json(t, &masked)
	for _, e := range masked.Events {
		if e.Kind == store.KindDeliveryClosed && e.Actor != "" {
			t.Fatalf("read token sees the actor: %+v", e)
		}
	}
}

func TestResendByAPI(t *testing.T) {
	h := newHarness(t)
	h.seed("pw_01", h.clock.Now().Add(-time.Hour), []string{"jane@example.test"}, []store.Status{store.Unknown})
	path := "/v1/mails/pw_01/targets/mailjet/resend"
	if r := h.do("POST", path, adminBearer, ""); r.StatusCode != 415 {
		t.Errorf("no body: %d", r.StatusCode)
	}
	r := h.do("POST", path, adminBearer, `{}`)
	if r.StatusCode != 409 || !strings.Contains(r.body, `"duplicate_risk"`) {
		t.Fatalf("resend of unknown without consent: %d %s", r.StatusCode, r.body)
	}
	r = h.do("POST", path, adminBearer, `{"accept_duplicate": true}`)
	var res actionResponse
	r.json(t, &res)
	if r.StatusCode != 200 || res.Delivery.Status != store.Queued || res.Delivery.NextAttempt == nil {
		t.Fatalf("resend: %d %s", r.StatusCode, r.body)
	}
	if h.wakes.Load() != 1 {
		t.Errorf("wakes = %d", h.wakes.Load())
	}
	if got := h.actionEvents(); !slices.Equal(got, []string{"delivery.resend support.admin was=unknown via=api"}) {
		t.Fatalf("events: %v", got)
	}
	// Twice is not possible: the recipient is queued now.
	r = h.do("POST", path, adminBearer, `{"accept_duplicate": true}`)
	if r.StatusCode != 409 || !strings.Contains(r.body, `"nothing_to_resend"`) {
		t.Fatalf("second resend: %d %s", r.StatusCode, r.body)
	}
	// While a worker holds it, neither action.
	if c, err := h.st.Claim(context.Background(), "mailjet", h.clock.Now()); err != nil || c == nil {
		t.Fatalf("claim: %v %v", c, err)
	}
	for p, body := range map[string]string{
		path:                                    `{"accept_duplicate": true}`,
		"/v1/mails/pw_01/targets/mailjet/close": `{"resolution": "given_up"}`,
	} {
		r := h.do("POST", p, adminBearer, body)
		if r.StatusCode != 409 || !strings.Contains(r.body, `"in_progress"`) {
			t.Errorf("%s while sending: %d %s", p, r.StatusCode, r.body)
		}
	}
}

func TestResendAfterRawGone(t *testing.T) {
	h := newHarness(t)
	h.seed("pw_01", h.clock.Now().Add(-time.Hour), []string{"jane@example.test"}, []store.Status{store.Failed})
	if _, err := h.st.Retain(context.Background(), store.RetainPolicy{RawKeepAfterFailure: time.Minute, RawMaxAge: time.Hour,
		RecordRetention: 90 * 24 * time.Hour, EventRetention: 90 * 24 * time.Hour}, h.clock.Now()); err != nil {
		t.Fatal(err)
	}
	r := h.do("POST", "/v1/mails/pw_01/targets/mailjet/resend", adminBearer, `{}`)
	if r.StatusCode != 409 || !strings.Contains(r.body, `"message_removed"`) {
		t.Fatalf("resend after expiry: %d %s", r.StatusCode, r.body)
	}
}
