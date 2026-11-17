package ui

import (
	"bytes"
	"cmp"
	"context"
	"fmt"
	"maps"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/eneuberg/postwerk/internal/addrhash"
	"github.com/eneuberg/postwerk/internal/keys"
	"github.com/eneuberg/postwerk/internal/query"
	"github.com/eneuberg/postwerk/internal/report"
	"github.com/eneuberg/postwerk/internal/store"
)

const (
	canary     = "canary-leak@example.test"
	utf8Canary = "kanarienvögel-leck@exämple.test"
	// bodyMarker is only in the stored message; no page may ever show it.
	bodyMarker = "BODY-MARKER-7f3a9c"
)

// seedMail stores a mail whose message holds bodyMarker and moves each
// target's recipients to the given statuses (nil: stays queued).
func (h *harness) seedMail(id string, at time.Time, rcpts []string, results map[string][]store.Status) {
	h.t.Helper()
	ctx := context.Background()
	m := store.NewMail{
		ID: id, ReceivedAt: at, Sender: cmp.Or(h.sender, "shop"), EnvelopeFrom: "noreply@example.test", Recipients: rcpts,
		MessageID: "<" + id + "@shop.example.test>", Ref: "order-" + id,
		Raw:  []byte("Subject: Your code\r\nTo: <" + rcpts[0] + ">\r\n\r\nYour code: " + bodyMarker + "\r\n"),
		Size: 2345, Audit: h.audit,
	}
	targets := slices.Sorted(maps.Keys(results))
	for _, t := range targets {
		m.Deliveries = append(m.Deliveries, store.NewDelivery{Target: t, Status: store.Queued})
	}
	if err := h.st.Accept(ctx, m); err != nil {
		h.t.Fatal(err)
	}
	for _, t := range targets {
		sts := results[t]
		if sts == nil {
			continue
		}
		c, err := h.st.Claim(ctx, t, at.Add(time.Second))
		if err != nil || c == nil || c.MailID != id {
			h.t.Fatalf("claim %s/%s: %v %v", id, t, c, err)
		}
		res := store.Result{MailID: id, Target: t, At: at.Add(2 * time.Second), Attempt: c.Attempt, Code: 250,
			Text: "2.0.0 OK queued", NextAttempt: at.Add(time.Hour)}
		for i, s := range sts {
			code, text, kind, level := 250, "2.0.0 OK", store.KindDeliveryHandedOff, store.Info
			switch s {
			case store.Failed:
				code, text, kind, level = 550, "5.1.1 <"+rcpts[i]+">: mailbox unavailable", store.KindDeliveryFailed, store.Error
			case store.Unknown:
				code, text, kind, level = 0, "connection reset after the message", store.KindDeliveryUnknown, store.Error
			case store.Queued:
				code, text, kind, level = 451, "4.7.1 try again later", store.KindDeliveryRetry, store.Warn
			}
			res.Recipients = append(res.Recipients, store.RecipientResult{
				Position: i, Status: s, Code: code, Text: text, Kind: kind, Level: level})
		}
		if err := h.st.Finish(ctx, res); err != nil {
			h.t.Fatal(err)
		}
	}
}

func testHasher(t *testing.T) *addrhash.Hasher {
	t.Helper()
	ring, err := keys.Parse([]byte(keys.Generate(time.Now())))
	if err != nil {
		t.Fatal(err)
	}
	return addrhash.New(ring)
}

var mailLinkRe = regexp.MustCompile(`href="/mails/(pw_[^"]+)"`)

func mailLinks(page string) []string {
	var out []string
	for _, m := range mailLinkRe.FindAllStringSubmatch(page, -1) {
		out = append(out, m[1])
	}
	return out
}

func (b *browser) search(by, q string) response {
	b.h.t.Helper()
	form := b.get("/mails")
	return b.do("POST", "/mails", url.Values{"csrf": {csrf(b.h.t, form.body)}, "by": {by}, "q": {q}})
}

// No page ever shows message content; every account sees addresses in
// full (U12, U14), whether it is from the tokens file or the database.
func TestPages(t *testing.T) {
	h := newHarness(t)
	now := h.clock.Now()
	h.seedMail("pw_01", now.Add(-3*time.Hour), []string{canary, utf8Canary},
		map[string][]store.Status{"mailjet": {store.HandedOff, store.Failed}, "capture": {store.HandedOff, store.HandedOff}})
	// pw_03 first: a claim takes the oldest queued delivery.
	h.seedMail("pw_03", now.Add(-time.Hour), []string{"lost@example.test"}, map[string][]store.Status{"mailjet": {store.Unknown}})
	h.seedMail("pw_02", now.Add(-2*time.Hour), []string{"queued@example.test"}, map[string][]store.Status{"mailjet": nil})

	for _, c := range []struct {
		b    *browser
		name string
	}{{h.loggedIn("ops", adminToken), "file account"}, {h.loggedIn("viewer", viewerPassword), "database account"}} {
		pages := map[string]response{
			"overview": c.b.get("/"),
			"mails":    c.b.get("/mails"),
			"filtered": c.b.get("/mails?state=partial&target=mailjet"),
			"detail":   c.b.get("/mails/pw_01"),
			"queued":   c.b.get("/mails/pw_02"),
			"results":  c.b.search("any", "order-pw_01"),
			"settings": c.b.get("/settings"),
			"account":  c.b.get("/settings/accounts/viewer"),
		}
		for name, r := range pages {
			if r.StatusCode != 200 {
				t.Fatalf("%s %s: %d\n%s", c.name, name, r.StatusCode, r.body)
			}
			for _, secret := range []string{bodyMarker, "Your code", "Subject"} {
				if strings.Contains(r.body, secret) {
					t.Errorf("%s %s shows message content %q", c.name, name, secret)
				}
			}
			if strings.Contains(r.body, "•••") {
				t.Errorf("%s %s shows a masked address", c.name, name)
			}
		}
		detail := pages["detail"].body
		if !strings.Contains(detail, canary) || !strings.Contains(detail, utf8Canary) ||
			!strings.Contains(detail, "noreply@example.test") {
			t.Errorf("%s does not see full addresses", c.name)
		}
		// Derived statuses are shown, with text.
		for _, want := range []string{">partial<", ">handed off<", ">failed<", "order-pw_01", "&lt;pw_01@shop.example.test&gt;",
			"mail.accepted", "delivery.failed", "5.1.1 &lt;redacted&gt;"} {
			if !strings.Contains(detail, want) {
				t.Errorf("%s detail lacks %q", c.name, want)
			}
		}
		if strings.Contains(detail, "ui.view") {
			t.Errorf("%s: the timeline shows ui.view", c.name)
		}
		if !slices.Equal(mailLinks(pages["filtered"].body), []string{"pw_01"}) {
			t.Errorf("filter partial at mailjet: %v", mailLinks(pages["filtered"].body))
		}
		if got := mailLinks(pages["results"].body); !slices.Equal(got, []string{"pw_01"}) {
			t.Errorf("search by ref: %v", got)
		}
		ov := pages["overview"].body
		for _, want := range []string{">mailjet<", ">capture<", "Recent problems", "/mails/pw_01", "/mails/pw_03", `href="/settings"`} {
			if !strings.Contains(ov, want) {
				t.Errorf("overview lacks %q", want)
			}
		}
	}
}

// Opening a mail is an event with the account and the mail id (U24);
// opening a mail that cannot be recorded shows nothing.
func TestViewEvents(t *testing.T) {
	ev := &switchEvents{}
	h := newHarness(t, func(o *Options) { ev.st = o.Events; o.Events = ev })
	h.seedMail("pw_01", h.clock.Now(), []string{canary}, map[string][]store.Status{"mailjet": {store.HandedOff}})
	b := h.loggedIn("viewer", viewerPassword)
	b.get("/mails/pw_01")
	b.get("/mails/pw_missing")
	var views []string
	for _, e := range h.events() {
		if e.Kind == store.KindUIView {
			views = append(views, e.Actor+"/"+e.MailID+"/"+e.Sender+"/"+string(e.Level))
		}
	}
	if want := []string{"viewer/pw_01/shop/info"}; !slices.Equal(views, want) {
		t.Fatalf("view events = %v, want %v", views, want)
	}
	ev.fail = true
	r := b.get("/mails/pw_01")
	if r.StatusCode != http.StatusServiceUnavailable || strings.Contains(r.body, canary) {
		t.Fatalf("unrecorded view: %d", r.StatusCode)
	}
}

// A search by address leaves no trace of the address in logs, events or
// exported events; the address appears in a page only before
// pseudonymization (U11, U12, U13).
func TestUISearchCanaryLeak(t *testing.T) {
	hs := testHasher(t)
	h := newHarnessWithHasher(t, hs)
	now := h.clock.Now()
	h.seedMail("pw_01", now.Add(-40*24*time.Hour), []string{canary, "other@example.test"},
		map[string][]store.Status{"mailjet": {store.HandedOff, store.HandedOff}})
	h.seedMail("pw_02", now.Add(-time.Hour), []string{utf8Canary}, map[string][]store.Status{"mailjet": {store.Failed}})
	admin := h.loggedIn("ops", adminToken)
	other := h.loggedIn("viewer", viewerPassword)

	for _, c := range []struct {
		b       *browser
		by, q   string
		want    []string
		comment string
	}{
		{admin, "address", strings.ToUpper(canary), []string{"pw_01"}, "by address, upper case"},
		{other, "address", canary, []string{"pw_01"}, "by address"},
		{other, "any", utf8Canary, []string{"pw_02"}, "any"},
		{admin, "address", utf8Canary, []string{"pw_02"}, "UTF-8"},
	} {
		r := c.b.search(c.by, c.q)
		if r.StatusCode != 200 || !slices.Equal(mailLinks(r.body), c.want) {
			t.Fatalf("%s: %d %v", c.comment, r.StatusCode, mailLinks(r.body))
		}
		// The query is not echoed back, not even to the person who typed it.
		if strings.Contains(r.body, c.q) || strings.Contains(strings.ToLower(r.body), "canary-leak") {
			t.Errorf("%s: the query is in the results page", c.comment)
		}
		if ref := r.Header.Get("Location"); ref != "" {
			t.Errorf("%s: search redirected to %s", c.comment, ref)
		}
		if !strings.Contains(r.body, "Results for your search") || !strings.Contains(r.body, `href="/mails">← Back to all mails`) {
			t.Errorf("%s: the results page lacks its header or the way back", c.comment)
		}
	}
	if d := admin.get("/mails/pw_01").body; !strings.Contains(d, canary) {
		t.Error("the address is not shown before pseudonymization")
	}

	// Pseudonymize pw_01 (40 days old); pw_02 stays plain.
	if _, err := h.st.Retain(context.Background(), store.RetainPolicy{
		RawMaxAge: 7 * 24 * time.Hour, PseudonymizeAfter: 30 * 24 * time.Hour, RecordRetention: 90 * 24 * time.Hour,
		EventRetention: 90 * 24 * time.Hour, Pseudonymize: hs.Hash,
	}, now); err != nil {
		t.Fatal(err)
	}
	for _, b := range []*browser{admin, other} {
		d := b.get("/mails/pw_01").body
		if strings.Contains(d, "canary-leak") || strings.Contains(d, "c•••@") || !strings.Contains(d, "pseudonymized") {
			t.Error("pseudonymized address shown")
		}
	}
	if r := admin.search("address", canary); !slices.Equal(mailLinks(r.body), []string{"pw_01"}) {
		t.Fatalf("search after pseudonymization: %v", mailLinks(r.body))
	}

	// Nowhere in logs, events or the exported event stream.
	var out bytes.Buffer
	rep := report.New(report.Options{Store: h.st, Sinks: []report.Sink{report.NewStdout(&out, "test")}})
	if err := rep.Flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	evs := h.events()
	var searches []string
	for _, e := range evs {
		all := fmt.Sprintf("%+v", e)
		for _, leak := range []string{"canary-leak", "kanarienvögel", "CANARY"} {
			if strings.Contains(all, leak) {
				t.Errorf("event leaks %q: %s", leak, all)
			}
		}
		if e.Kind == store.KindUISearch {
			searches = append(searches, e.Actor+"/"+e.Text)
		}
	}
	if want := []string{"ops/address", "viewer/address", "viewer/any", "ops/address", "ops/address"}; !slices.Equal(searches, want) {
		t.Errorf("search events = %v, want %v", searches, want)
	}
	for name, text := range map[string]string{"logs": h.logs.String(), "stdout": out.String()} {
		for _, leak := range []string{"canary-leak", "kanarienvögel", "CANARY"} {
			if strings.Contains(text, leak) {
				t.Errorf("%s leak %q", name, leak)
			}
		}
	}
	if !strings.Contains(out.String(), `"kind":"ui.search","actor":"ops","text":"address"`) {
		t.Errorf("stdout lacks the search event:\n%s", out.String())
	}
}

// Every link reachable from the overview: never the message content
// (U14).
func TestNoContentOnAnyReachablePage(t *testing.T) {
	h := newHarness(t)
	now := h.clock.Now()
	for i := range 60 {
		st := []store.Status{store.HandedOff}
		if i%7 == 0 {
			st = []store.Status{store.Failed}
		}
		h.seedMail(fmt.Sprintf("pw_%02d", i), now.Add(-time.Duration(60-i)*time.Hour), []string{"r@example.test"},
			map[string][]store.Status{"mailjet": st})
	}
	hrefRe := regexp.MustCompile(`href="(/[^"]*)"`)
	for _, b := range []*browser{h.loggedIn("ops", adminToken), h.loggedIn("viewer", viewerPassword)} {
		seen := map[string]bool{}
		queue := []string{"/"}
		for len(queue) > 0 && len(seen) < 400 {
			// A browser never sends the fragment.
			p, _, _ := strings.Cut(strings.ReplaceAll(queue[0], "&amp;", "&"), "#")
			queue = queue[1:]
			if seen[p] {
				continue
			}
			seen[p] = true
			r := b.get(p)
			if r.StatusCode != 200 {
				t.Fatalf("GET %s: %d", p, r.StatusCode)
			}
			if strings.Contains(r.body, bodyMarker) {
				t.Fatalf("%s shows message content", p)
			}
			for _, m := range hrefRe.FindAllStringSubmatch(r.body, -1) {
				queue = append(queue, m[1])
			}
		}
		if len(seen) < 60 {
			t.Fatalf("crawled only %d pages", len(seen))
		}
	}
}

func TestMailListPagination(t *testing.T) {
	h := newHarness(t)
	now := h.clock.Now()
	for i := range 120 {
		h.seedMail(fmt.Sprintf("pw_%03d", i), now.Add(-time.Duration(120-i)*time.Minute), []string{"r@example.test"},
			map[string][]store.Status{"mailjet": {store.HandedOff}})
	}
	b := h.loggedIn("viewer", viewerPassword)
	seen := map[string]bool{}
	next := "/mails?state=handed_off"
	for page := 1; next != ""; page++ {
		r := b.get(next)
		ids := mailLinks(r.body)
		want := 50
		if page == 3 {
			want = 20
		}
		if r.StatusCode != 200 || len(ids) != want {
			t.Fatalf("page %d: %d, %d rows", page, r.StatusCode, len(ids))
		}
		for _, id := range ids {
			if seen[id] {
				t.Fatalf("%s twice", id)
			}
			seen[id] = true
		}
		if page == 1 && (ids[0] != "pw_119" || strings.Contains(r.body, `rel="prev"`)) {
			t.Fatalf("page 1 starts with %s", ids[0])
		}
		if page > 1 && !strings.Contains(r.body, fmt.Sprintf(`href="/mails?page=%d&amp;state=handed_off" rel="prev"`, page-1)) &&
			!strings.Contains(r.body, `href="/mails?state=handed_off" rel="prev"`) {
			t.Fatalf("page %d: no link back", page)
		}
		next = ""
		if m := regexp.MustCompile(`href="([^"]+)" rel="next"`).FindStringSubmatch(r.body); m != nil {
			next = strings.ReplaceAll(m[1], "&amp;", "&")
			if !strings.Contains(next, "state=handed_off") || !strings.Contains(next, fmt.Sprintf("page=%d", page+1)) {
				t.Fatalf("next link %q loses the filter", next)
			}
		}
	}
	if len(seen) != 120 {
		t.Fatalf("pages held %d mails", len(seen))
	}
	for _, bad := range []string{"/mails?page=0", "/mails?page=x", "/mails?from=15.11.2026", "/mails?state=lost",
		"/mails?target=Bad-Name", "/mails?from=2026-11-15&to=2026-11-01"} {
		if r := b.get(bad); r.StatusCode != http.StatusBadRequest || !strings.Contains(r.body, "not valid") {
			t.Errorf("%s: %d", bad, r.StatusCode)
		}
	}
	if r := b.get("/mails?from=2026-11-15&to=2026-11-15"); r.StatusCode != 200 || len(mailLinks(r.body)) != 50 {
		t.Errorf("one day: %d rows", len(mailLinks(r.body)))
	}
}

func TestMailNotFound(t *testing.T) {
	h := newHarness(t)
	b := h.loggedIn("ops", adminToken)
	for _, p := range []string{"/mails/pw_missing", "/mails/not-an-id", "/mails/pw_%3Cscript%3E"} {
		if r := b.get(p); r.StatusCode != 404 || strings.Contains(r.body, "<script") {
			t.Errorf("%s: %d", p, r.StatusCode)
		}
	}
}

func TestSearchChecks(t *testing.T) {
	h := newHarness(t, func(o *Options) { o.SearchesPerMinute = 3 })
	b := h.loggedIn("ops", adminToken)
	token := csrf(t, b.get("/mails").body)

	if r := b.do("POST", "/mails", url.Values{"by": {"address"}, "q": {canary}}); r.StatusCode != 403 {
		t.Fatalf("search without csrf: %d", r.StatusCode)
	}
	if r := b.do("POST", "/mails", url.Values{"csrf": {token}, "by": {"subject"}, "q": {"x"}}); r.StatusCode != 400 {
		t.Fatalf("unknown kind: %d", r.StatusCode)
	}
	if r := b.do("POST", "/mails", url.Values{"csrf": {token}, "by": {"any"}, "q": {"  "}}); r.StatusCode != 400 ||
		!strings.Contains(r.body, "Enter something") {
		t.Fatalf("empty search: %d", r.StatusCode)
	}
	// The form posts; there is no GET search with a query. The old search
	// page sends the browser to the mails page and drops the query.
	if r := b.get("/search?q=" + url.QueryEscape(canary)); r.StatusCode != http.StatusMovedPermanently ||
		r.Header.Get("Location") != "/mails" {
		t.Fatalf("GET /search: %d %q", r.StatusCode, r.Header.Get("Location"))
	}
	if r := b.get("/mails?q=" + url.QueryEscape(canary)); r.StatusCode != 200 || strings.Contains(r.body, canary) ||
		!strings.Contains(r.body, `method="post" action="/mails"`) || strings.Contains(r.body, "Results for your search") {
		t.Fatalf("GET mails with a query: %d", r.StatusCode)
	}
	if r := b.do("POST", "/search", url.Values{"csrf": {token}, "by": {"any"}, "q": {"x"}}); r.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("POST /search: %d", r.StatusCode)
	}
	b.do("POST", "/mails", url.Values{"csrf": {token}, "by": {"any"}, "q": {"x"}})
	b.do("POST", "/mails", url.Values{"csrf": {token}, "by": {"any"}, "q": {"y"}})
	if r := b.do("POST", "/mails", url.Values{"csrf": {token}, "by": {"any"}, "q": {"z"}}); r.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("over the limit: %d", r.StatusCode)
	}
	// The limit is per token name, not per session.
	if r := h.loggedIn("ops", adminToken).search("any", "x"); r.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("second session of the same name: %d", r.StatusCode)
	}
	if r := h.loggedIn("viewer", viewerPassword).search("any", "x"); r.StatusCode != 200 {
		t.Fatalf("another name: %d", r.StatusCode)
	}
	h.clock.Add(time.Minute)
	if r := b.do("POST", "/mails", url.Values{"csrf": {token}, "by": {"any"}, "q": {"x"}}); r.StatusCode != 200 {
		t.Fatalf("after a minute: %d", r.StatusCode)
	}
}

// A search that cannot be recorded is not run.
func TestSearchNeedsTheAuditEvent(t *testing.T) {
	ev := &switchEvents{}
	h := newHarness(t, func(o *Options) { ev.st = o.Events; o.Events = ev })
	h.seedMail("pw_01", h.clock.Now(), []string{canary}, map[string][]store.Status{"mailjet": {store.HandedOff}})
	b := h.loggedIn("ops", adminToken)
	ev.fail = true
	r := b.search("address", canary)
	if r.StatusCode != http.StatusServiceUnavailable || len(mailLinks(r.body)) != 0 {
		t.Fatalf("unrecorded search: %d %v", r.StatusCode, mailLinks(r.body))
	}
}

type switchEvents struct {
	st   EventLog
	fail bool
}

func (s *switchEvents) Append(ctx context.Context, e store.Event) error {
	if s.fail {
		return fmt.Errorf("disk full")
	}
	return s.st.Append(ctx, e)
}

// The overview shows each target's state and which targets are optional
// (D45).
func TestOverviewTargetState(t *testing.T) {
	since := time.Date(2026, 11, 15, 11, 57, 0, 0, time.UTC)
	h := newHarness(t, func(o *Options) {
		o.Query = query.New(query.Options{Store: o.Events.(*store.Store), Targets: []string{"mailjet", "capture"},
			Optional: []string{"capture"}, Now: o.Now, Health: func() []query.TargetHealth {
				return []query.TargetHealth{{Target: "mailjet", Down: true, Since: since, Reason: "auth"},
					{Target: "capture", Since: since.Add(-time.Hour)}}
			}})
	})
	r := h.loggedIn("ops", adminToken).get("/")
	if r.StatusCode != 200 {
		t.Fatalf("overview: %d", r.StatusCode)
	}
	for _, want := range []string{">down (auth) since ", ">up since ", ">optional</span>"} {
		if !strings.Contains(r.body, want) {
			t.Fatalf("overview lacks %q:\n%s", want, r.body)
		}
	}
}
