package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/eneuberg/postwerk/internal/addrhash"
	"github.com/eneuberg/postwerk/internal/auth"
	"github.com/eneuberg/postwerk/internal/keys"
	"github.com/eneuberg/postwerk/internal/query"
	"github.com/eneuberg/postwerk/internal/report"
	"github.com/eneuberg/postwerk/internal/store"
	"github.com/eneuberg/postwerk/internal/testutil"
)

const (
	adminSecret = "admin-secret-for-tests"
	readSecret  = "read-secret-for-tests"
	canary      = "canary-leak@example.test"
	// bodyMarker and subjectMarker are only in the stored message; no
	// response may ever hold them.
	bodyMarker    = "BODY-MARKER-7f3a9c"
	subjectMarker = "SUBJECT-MARKER-c41d"
)

var (
	adminBearer = "Bearer " + BearerToken("support.admin", adminSecret)
	readBearer  = "Bearer " + BearerToken("support", readSecret)
)

var tokenFile = sync.OnceValue(func() string {
	a, err := auth.TokenLine("support.admin", auth.RoleAdmin, adminSecret, auth.MinIterations)
	if err != nil {
		panic(err)
	}
	r, err := auth.TokenLine("support", auth.RoleRead, readSecret, auth.MinIterations)
	if err != nil {
		panic(err)
	}
	return a + "\n" + r + "\n"
})

type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *clock) Add(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

// countingVerifier counts full token checks.
type countingVerifier struct {
	Verifier
	calls atomic.Int32
}

func (v *countingVerifier) Verify(name, token string) (auth.Role, bool) {
	v.calls.Add(1)
	return v.Verifier.Verify(name, token)
}

type failingEvents struct{}

func (failingEvents) Append(context.Context, store.Event) error { return errors.New("disk full") }

type harness struct {
	t      *testing.T
	st     *store.Store
	api    *API
	clock  *clock
	logs   *testutil.SafeBuffer
	tokens *countingVerifier
	hasher *addrhash.Hasher
}

func newHarness(t *testing.T, opts ...func(*Options)) *harness {
	t.Helper()
	st, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "pw.db"), store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	tokens, err := auth.ParseTokens(strings.NewReader(tokenFile()))
	if err != nil {
		t.Fatal(err)
	}
	ring, err := keys.Parse([]byte(keys.Generate(time.Now())))
	if err != nil {
		t.Fatal(err)
	}
	h := &harness{t: t, st: st, clock: &clock{t: time.Date(2026, 11, 15, 12, 0, 0, 0, time.UTC)},
		logs: &testutil.SafeBuffer{}, tokens: &countingVerifier{Verifier: tokens}, hasher: addrhash.New(ring)}
	o := Options{
		Query:  query.New(query.Options{Store: st, Hasher: h.hasher, Targets: []string{"mailjet"}, Now: h.clock.Now}),
		Tokens: h.tokens,
		Events: st,
		Logger: testutil.Logger(h.logs),
		Now:    h.clock.Now,
	}
	for _, f := range opts {
		f(&o)
	}
	h.api, err = New(o)
	if err != nil {
		t.Fatal(err)
	}
	return h
}

// seed stores a mail whose message holds bodyMarker and subjectMarker; the
// mailjet recipients end in the given statuses (nil: queued).
func (h *harness) seed(id string, at time.Time, rcpts []string, results []store.Status) {
	h.t.Helper()
	ctx := context.Background()
	m := store.NewMail{
		ID: id, ReceivedAt: at, Sender: "shop", EnvelopeFrom: "noreply@example.test", Recipients: rcpts,
		MessageID: "<" + id + "@shop.example.test>", Ref: "order-" + id,
		Raw: []byte("Subject: " + subjectMarker + "\r\nTo: <" + rcpts[0] + ">\r\nX-Secret-Header: yes\r\n\r\n" +
			bodyMarker + "\r\n"),
		Size:       2345,
		Deliveries: []store.NewDelivery{{Target: "mailjet", Status: store.Queued}},
	}
	if err := h.st.Accept(ctx, m); err != nil {
		h.t.Fatal(err)
	}
	if results == nil {
		return
	}
	c, err := h.st.Claim(ctx, "mailjet", at.Add(time.Second))
	if err != nil || c == nil || c.MailID != id {
		h.t.Fatalf("claim %s: %v %v", id, c, err)
	}
	res := store.Result{MailID: id, Target: "mailjet", At: at.Add(2 * time.Second), Attempt: c.Attempt, Code: 250,
		Text: "2.0.0 OK", NextAttempt: at.Add(time.Hour)}
	for i, s := range results {
		code, text, kind, level := 250, "2.0.0 OK", store.KindDeliveryHandedOff, store.Info
		if s == store.Failed {
			code, text, kind, level = 550, "5.1.1 <"+rcpts[i]+">: mailbox unavailable", store.KindDeliveryFailed, store.Error
		}
		res.Recipients = append(res.Recipients, store.RecipientResult{
			Position: i, Status: s, Code: code, Text: text, Kind: kind, Level: level})
	}
	if err := h.st.Finish(ctx, res); err != nil {
		h.t.Fatal(err)
	}
}

type response struct {
	*http.Response
	body string
}

func (r response) json(t *testing.T, v any) {
	t.Helper()
	if err := json.Unmarshal([]byte(r.body), v); err != nil {
		t.Fatalf("decode %q: %v", r.body, err)
	}
}

// do sends a request; body, if not empty, goes as application/json.
func (h *harness) do(method, path, bearer, body string, hdr ...string) response {
	h.t.Helper()
	var rd io.Reader
	if body != "" {
		rd = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, path, rd)
	req.Host = "postwerk.mail.svc:8082"
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	if bearer != "" {
		req.Header.Set("Authorization", bearer)
	}
	for i := 0; i+1 < len(hdr); i += 2 {
		req.Header.Set(hdr[i], hdr[i+1])
	}
	rec := httptest.NewRecorder()
	h.api.ServeHTTP(rec, req)
	return response{Response: rec.Result(), body: rec.Body.String()}
}

func (h *harness) search(bearer, by, q string) response {
	h.t.Helper()
	b, _ := json.Marshal(map[string]string{"by": by, "query": q})
	return h.do("POST", "/v1/search", bearer, string(b))
}

func (h *harness) events() []store.Event {
	h.t.Helper()
	evs, err := h.st.EventsAfter(context.Background(), 0, 10000)
	if err != nil {
		h.t.Fatal(err)
	}
	return evs
}

// apiEvents returns "kind actor text" for every API event.
func (h *harness) apiEvents() []string {
	h.t.Helper()
	var out []string
	for _, e := range h.events() {
		if strings.HasPrefix(e.Kind, "api.") {
			out = append(out, strings.TrimSpace(e.Kind+" "+e.Actor+" "+e.Text))
		}
	}
	return out
}

func ids(rows []mailSummary) []string {
	var out []string
	for _, r := range rows {
		out = append(out, r.ID)
	}
	return out
}

func TestRoles(t *testing.T) {
	h := newHarness(t)
	now := h.clock.Now()
	h.seed("pw_01", now.Add(-time.Hour), []string{canary, "other@example.test"}, []store.Status{store.HandedOff, store.Failed})
	h.seed("pw_02", now.Add(-time.Minute), []string{"x@example.test"}, nil)

	// Both roles look mails up.
	for _, bearer := range []string{readBearer, adminBearer} {
		r := h.search(bearer, "address", canary)
		var res searchResponse
		r.json(t, &res)
		if r.StatusCode != 200 || !slices.Equal(ids(res.Mails), []string{"pw_01"}) || res.Mails[0].Status != store.Partial {
			t.Fatalf("search: %d %s", r.StatusCode, r.body)
		}
	}
	// read sees addresses masked, admin in full.
	var d mailDetail
	r := h.do("GET", "/v1/mails/pw_01", readBearer, "")
	r.json(t, &d)
	if r.StatusCode != 200 || d.Recipients[0].State != "masked" || d.Recipients[0].Address != query.Mask(canary) ||
		d.EnvelopeFrom.Address != "n•••@e•••.test" || strings.Contains(r.body, "canary-leak") || strings.Contains(r.body, "other@") {
		t.Fatalf("read detail: %d %s", r.StatusCode, r.body)
	}
	if d.Status != store.Partial || len(d.Deliveries) != 1 || d.Deliveries[0].Recipients[1].LastCode != 550 ||
		!strings.Contains(d.Deliveries[0].Recipients[1].LastText, "<redacted>") || len(d.Events) == 0 {
		t.Fatalf("read detail content: %s", r.body)
	}
	r = h.do("GET", "/v1/mails/pw_01", adminBearer, "")
	d = mailDetail{}
	r.json(t, &d)
	if r.StatusCode != 200 || d.Recipients[0].State != "full" || d.Recipients[0].Address != canary ||
		d.EnvelopeFrom.Address != "noreply@example.test" {
		t.Fatalf("admin detail: %d %s", r.StatusCode, r.body)
	}

	// read can neither list nor see the overview.
	for _, p := range []string{"/v1/mails", "/v1/mails?state=failed", "/v1/overview"} {
		if r := h.do("GET", p, readBearer, ""); r.StatusCode != 403 || !strings.Contains(r.body, `"forbidden"`) {
			t.Errorf("read GET %s: %d %s", p, r.StatusCode, r.body)
		}
	}
	// admin can.
	var list listResponse
	r = h.do("GET", "/v1/mails", adminBearer, "")
	r.json(t, &list)
	if r.StatusCode != 200 || !slices.Equal(ids(list.Mails), []string{"pw_02", "pw_01"}) || list.Page != 1 || list.HasNext {
		t.Fatalf("admin list: %d %s", r.StatusCode, r.body)
	}
	r = h.do("GET", "/v1/mails?state=queued&from=2026-11-15&to=2026-11-15", adminBearer, "")
	list = listResponse{}
	r.json(t, &list)
	if r.StatusCode != 200 || !slices.Equal(ids(list.Mails), []string{"pw_02"}) {
		t.Fatalf("admin list filtered: %d %s", r.StatusCode, r.body)
	}
	for _, p := range []string{"/v1/mails?state=nope", "/v1/mails?from=15.11.2026", "/v1/mails?page=0",
		"/v1/mails?to=user@example.test", "/v1/mails?state=a&state=b"} {
		if r := h.do("GET", p, adminBearer, ""); r.StatusCode != 400 {
			t.Errorf("GET %s: %d %s", p, r.StatusCode, r.body)
		}
	}
	var o overviewResponse
	r = h.do("GET", "/v1/overview", adminBearer, "")
	r.json(t, &o)
	if r.StatusCode != 200 || len(o.Targets) != 1 || o.Targets[0].Queued != 1 || o.Targets[0].Recent["handed_off"] != 1 ||
		o.Targets[0].RecentTotal != 3 || !slices.Equal(ids(o.Problems), []string{"pw_01"}) {
		t.Fatalf("overview: %d %s", r.StatusCode, r.body)
	}
	if r := h.do("GET", "/v1/mails/pw_nope", readBearer, ""); r.StatusCode != 404 {
		t.Fatalf("unknown mail: %d", r.StatusCode)
	}

	want := []string{
		"api.request support search by=address status=200 results=1",
		"api.request support.admin search by=address status=200 results=1",
		"api.request support mail status=200 results=1",
		"api.request support.admin mail status=200 results=1",
		"api.forbidden support mails",
		"api.forbidden support mails",
		"api.forbidden support overview",
		"api.request support.admin mails status=200 results=2",
		"api.request support.admin mails status=200 results=1",
		"api.request support.admin mails status=400 results=0",
		"api.request support.admin mails status=400 results=0",
		"api.request support.admin mails status=400 results=0",
		"api.request support.admin mails status=400 results=0",
		"api.request support.admin mails status=400 results=0",
		"api.request support.admin overview status=200 results=1",
		"api.request support mail status=404 results=0",
	}
	if got := h.apiEvents(); !slices.Equal(got, want) {
		t.Fatalf("events:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

// A read token gets at most ReadMaxResults mails per search.
func TestReadSearchIsBounded(t *testing.T) {
	h := newHarness(t, func(o *Options) { o.PerToken = 1000 })
	now := h.clock.Now()
	for i := range ReadMaxResults + 5 {
		h.seed(fmt.Sprintf("pw_%02d", i), now.Add(-time.Duration(i)*time.Minute), []string{canary}, nil)
	}
	for _, c := range []struct {
		bearer    string
		n         int
		truncated bool
	}{{readBearer, ReadMaxResults, true}, {adminBearer, ReadMaxResults + 5, false}} {
		var res searchResponse
		r := h.search(c.bearer, "address", canary)
		r.json(t, &res)
		if r.StatusCode != 200 || len(res.Mails) != c.n || res.Truncated != c.truncated {
			t.Fatalf("%d mails, truncated %v: %s", len(res.Mails), res.Truncated, r.body)
		}
	}
}

func TestAuthFailures(t *testing.T) {
	h := newHarness(t)
	for _, c := range []struct{ name, header string }{
		{"missing", ""},
		{"basic", "Basic c3VwcG9ydDp4"},
		{"no dot", "Bearer " + readSecret},
		{"empty token", "Bearer support."},
		{"unknown name", "Bearer nobody." + readSecret},
		{"token in the name part", "Bearer " + adminSecret + "." + readSecret},
		{"wrong token", "Bearer support.wrong"},
		{"other token", "Bearer support." + adminSecret},
		{"too long", "Bearer support." + strings.Repeat("x", 2000)},
	} {
		r := h.do("POST", "/v1/search", c.header, `{"by":"id","query":"pw_01"}`)
		if r.StatusCode != 401 || r.Header.Get("WWW-Authenticate") != `Bearer realm="postwerk"` ||
			!strings.Contains(r.body, `"unauthorized"`) {
			t.Errorf("%s: %d %s", c.name, r.StatusCode, r.body)
		}
	}
	// Every failure is an event; the name only when it is in the file.
	want := slices.Repeat([]string{"api.auth_failed"}, 6)
	want = append(want, "api.auth_failed support", "api.auth_failed support", "api.auth_failed")
	if got := h.apiEvents(); !slices.Equal(got, want) {
		t.Fatalf("events = %q, want %q", got, want)
	}
	for _, e := range h.events() {
		if s := fmt.Sprintf("%+v", e); strings.Contains(s, "secret-for-tests") || strings.Contains(s, "wrong") {
			t.Errorf("event holds the token: %s", s)
		}
	}
	if s := h.logs.String(); strings.Contains(s, "secret-for-tests") {
		t.Errorf("log holds the token: %s", s)
	}
	// A token that was right before costs no second full check.
	n := h.tokens.calls.Load()
	for range 3 {
		if r := h.search(readBearer, "id", "pw_01"); r.StatusCode != 200 {
			t.Fatalf("good token: %d %s", r.StatusCode, r.body)
		}
	}
	if got := h.tokens.calls.Load() - n; got != 1 {
		t.Fatalf("full checks for three good requests = %d, want 1", got)
	}
	// A wrong token for a known name still pays the full check.
	n = h.tokens.calls.Load()
	h.search("Bearer support.wrong", "id", "pw_01")
	if h.tokens.calls.Load() != n+1 {
		t.Fatal("wrong token skipped the full check")
	}
}

func TestRateLimits(t *testing.T) {
	h := newHarness(t, func(o *Options) { o.PerToken, o.Global = 3, 6 })
	for i := range 3 {
		if r := h.search(readBearer, "id", "pw_01"); r.StatusCode != 200 {
			t.Fatalf("request %d: %d", i, r.StatusCode)
		}
	}
	r := h.search(readBearer, "id", "pw_01")
	if r.StatusCode != 429 || r.Header.Get("Retry-After") != "60" || !strings.Contains(r.body, `"rate_limited"`) {
		t.Fatalf("over the per-token limit: %d %q %s", r.StatusCode, r.Header.Get("Retry-After"), r.body)
	}
	// Another token has its own budget, until the global cap is reached;
	// that counts unauthenticated requests as well.
	h.clock.Add(15 * time.Second)
	if r := h.search(adminBearer, "id", "pw_01"); r.StatusCode != 200 {
		t.Fatalf("admin: %d", r.StatusCode)
	}
	if r := h.search("", "id", "pw_01"); r.StatusCode != 401 {
		t.Fatalf("no token: %d", r.StatusCode)
	}
	r = h.search(adminBearer, "id", "pw_01")
	if r.StatusCode != 429 || r.Header.Get("Retry-After") != "45" {
		t.Fatalf("over the global cap: %d %q", r.StatusCode, r.Header.Get("Retry-After"))
	}
	if r := h.search("", "id", "pw_01"); r.StatusCode != 429 {
		t.Fatalf("unauthenticated over the global cap: %d", r.StatusCode)
	}
	h.clock.Add(time.Minute)
	if r := h.search(readBearer, "id", "pw_01"); r.StatusCode != 200 {
		t.Fatalf("after the window: %d", r.StatusCode)
	}
}

// No response holds message content, the subject or another header, for
// either role; read never sees an address; the query is never in an event,
// log or the exported stream (P3, H5).
func TestNoContentOrQueryLeak(t *testing.T) {
	h := newHarness(t)
	now := h.clock.Now()
	h.seed("pw_01", now.Add(-40*24*time.Hour), []string{canary}, []store.Status{store.Failed})
	h.seed("pw_02", now.Add(-time.Hour), []string{"kanarienvögel-leck@exämple.test"}, []store.Status{store.HandedOff})

	var all []string
	for _, bearer := range []string{readBearer, adminBearer} {
		for _, r := range []response{
			h.search(bearer, "address", strings.ToUpper(canary)),
			h.search(bearer, "any", "kanarienvögel-leck@exämple.test"),
			h.search(bearer, "ref", "order-pw_01"),
			h.search(bearer, "message_id", "pw_02@shop.example.test"),
			h.do("GET", "/v1/mails/pw_01", bearer, ""),
			h.do("GET", "/v1/mails/pw_02", bearer, ""),
		} {
			if r.StatusCode != 200 || !strings.Contains(r.body, `"pw_0`) {
				t.Fatalf("%d %s", r.StatusCode, r.body)
			}
			all = append(all, r.body)
			if bearer == readBearer && (strings.Contains(strings.ToLower(r.body), "canary-leak") || strings.Contains(r.body, "kanarienvögel")) {
				t.Errorf("read sees an address: %s", r.body)
			}
		}
	}
	for _, p := range []string{"/v1/mails", "/v1/overview"} {
		all = append(all, h.do("GET", p, adminBearer, "").body)
	}
	for _, b := range all {
		for _, leak := range []string{bodyMarker, subjectMarker, "X-Secret-Header", "Subject"} {
			if strings.Contains(b, leak) {
				t.Errorf("response holds %q: %s", leak, b)
			}
		}
	}

	// After pseudonymization nobody sees the address; it is still found.
	if _, err := h.st.Retain(context.Background(), store.RetainPolicy{
		RawMaxAge: 7 * 24 * time.Hour, PseudonymizeAfter: 30 * 24 * time.Hour, RecordRetention: 90 * 24 * time.Hour,
		EventRetention: 90 * 24 * time.Hour, Pseudonymize: h.hasher.Hash,
	}, now); err != nil {
		t.Fatal(err)
	}
	r := h.do("GET", "/v1/mails/pw_01", adminBearer, "")
	if strings.Contains(r.body, "canary-leak") || !strings.Contains(r.body, `"state":"pseudonymized"`) {
		t.Errorf("pseudonymized: %s", r.body)
	}
	var res searchResponse
	h.search(readBearer, "address", canary).json(t, &res)
	if !slices.Equal(ids(res.Mails), []string{"pw_01"}) {
		t.Errorf("search after pseudonymization: %v", ids(res.Mails))
	}

	var out bytes.Buffer
	rep := report.New(report.Options{Store: h.st, Sinks: []report.Sink{report.NewStdout(&out, "test")}})
	if err := rep.Flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	for _, e := range h.events() {
		if !strings.HasPrefix(e.Kind, "api.") {
			continue
		}
		s := fmt.Sprintf("%+v", e)
		for _, leak := range []string{"canary", "CANARY", "kanarien", "order-", "shop.example", "pw_0"} {
			if strings.Contains(s, leak) {
				t.Errorf("event holds %q: %s", leak, s)
			}
		}
	}
	for name, text := range map[string]string{"logs": h.logs.String(), "stdout": out.String()} {
		for _, leak := range []string{"canary-leak", "CANARY", "kanarienvögel"} {
			if strings.Contains(text, leak) {
				t.Errorf("%s holds %q", name, leak)
			}
		}
	}
	if !strings.Contains(out.String(), `"kind":"api.request","actor":"support","text":"search by=address status=200 results=1"`) {
		t.Errorf("stdout lacks the request event:\n%s", out.String())
	}
}

func TestRequestChecks(t *testing.T) {
	h := newHarness(t, func(o *Options) { o.Hosts = []string{"postwerk.mail.svc"} })
	big := `{"by":"any","query":"` + strings.Repeat("a", maxBody) + `"}`
	for _, c := range []struct {
		name, method, path, body string
		hdr                      []string
		want                     int
		code                     string
	}{
		{"GET search", "GET", "/v1/search", "", nil, 405, "method_not_allowed"},
		{"POST mail", "POST", "/v1/mails/pw_01", `{}`, nil, 405, "method_not_allowed"},
		{"DELETE list", "DELETE", "/v1/mails", "", nil, 405, "method_not_allowed"},
		{"OPTIONS preflight", "OPTIONS", "/v1/search", "", nil, 405, "method_not_allowed"},
		{"text/plain", "POST", "/v1/search", `{"query":"x"}`, []string{"Content-Type", "text/plain"}, 415, "unsupported_media_type"},
		{"form", "POST", "/v1/search", `query=x`, []string{"Content-Type", "application/x-www-form-urlencoded"}, 415, "unsupported_media_type"},
		{"latin1", "POST", "/v1/search", `{"query":"x"}`, []string{"Content-Type", "application/json; charset=latin1"}, 415, "unsupported_media_type"},
		{"query in URL", "POST", "/v1/search?query=user@example.test", `{"query":"x"}`, nil, 400, "bad_request"},
		{"too large", "POST", "/v1/search", big, nil, 413, "too_large"},
		{"unknown field", "POST", "/v1/search", `{"q":"x"}`, nil, 400, "bad_request"},
		{"two values", "POST", "/v1/search", `{"query":"x"} {"query":"y"}`, nil, 400, "bad_request"},
		{"not an object", "POST", "/v1/search", `"x"`, nil, 400, "bad_request"},
		{"bad kind", "POST", "/v1/search", `{"by":"subject","query":"x"}`, nil, 400, "bad_request"},
		{"empty query", "POST", "/v1/search", `{"by":"any","query":"  "}`, nil, 400, "bad_request"},
		{"GET with body", "GET", "/v1/mails/pw_01", `{}`, nil, 400, "bad_request"},
		{"params on detail", "GET", "/v1/mails/pw_01?x=1", "", nil, 400, "bad_request"},
		{"unknown path", "GET", "/v1/nope", "", nil, 404, "not_found"},
		{"root", "GET", "/", "", nil, 404, "not_found"},
		{"browser origin", "POST", "/v1/search", `{"query":"x"}`, []string{"Origin", "https://evil.example.test"}, 403, "browser_refused"},
		{"browser fetch", "GET", "/v1/mails/pw_01", "", []string{"Sec-Fetch-Site", "same-origin"}, 403, "browser_refused"},
	} {
		req := httptest.NewRequest(c.method, c.path, strings.NewReader(c.body))
		req.Host = "postwerk.mail.svc:8082"
		req.Header.Set("Authorization", readBearer)
		if c.body != "" {
			req.Header.Set("Content-Type", "application/json")
		}
		for i := 0; i+1 < len(c.hdr); i += 2 {
			req.Header.Set(c.hdr[i], c.hdr[i+1])
		}
		rec := httptest.NewRecorder()
		h.api.ServeHTTP(rec, req)
		var e apiError
		_ = json.Unmarshal(rec.Body.Bytes(), &e)
		if rec.Code != c.want || e.Error != c.code || rec.Header().Get("Content-Type") != "application/json" {
			t.Errorf("%s: %d %s", c.name, rec.Code, rec.Body.String())
		}
		if rec.Header().Get("Access-Control-Allow-Origin") != "" {
			t.Errorf("%s: CORS header", c.name)
		}
	}
	// The Host allow-list, with any port.
	for host, want := range map[string]int{"postwerk.mail.svc": 200, "POSTWERK.mail.svc:443": 200, "evil.example.test": 403, "": 403} {
		req := httptest.NewRequest("GET", "/v1/mails/pw_nope", nil)
		req.Host = host
		req.Header.Set("Authorization", readBearer)
		rec := httptest.NewRecorder()
		h.api.ServeHTTP(rec, req)
		if (want == 200 && rec.Code != 404) || (want == 403 && rec.Code != 403) {
			t.Errorf("host %q: %d", host, rec.Code)
		}
	}
	// A JSON body without a charset and with charset=utf-8 is fine.
	r := h.do("POST", "/v1/search", readBearer, `{"by":"id","query":"pw_01"}`, "Content-Type", "application/json; charset=UTF-8")
	if r.StatusCode != 200 || !strings.Contains(r.body, `"mails":[]`) {
		t.Fatalf("charset utf-8: %d %s", r.StatusCode, r.body)
	}
}

func TestSecurityHeaders(t *testing.T) {
	h := newHarness(t)
	for _, r := range []response{
		h.search(readBearer, "id", "pw_01"),
		h.search("", "id", "pw_01"),
		h.do("GET", "/nope", "", ""),
		h.do("GET", "/v1/overview", readBearer, ""),
	} {
		for k, v := range map[string]string{
			"Content-Security-Policy": ContentSecurityPolicy, "X-Content-Type-Options": "nosniff",
			"Cache-Control": "no-store", "Referrer-Policy": "no-referrer", "X-Frame-Options": "DENY",
			"Content-Type": "application/json",
		} {
			if got := r.Header.Get(k); got != v {
				t.Errorf("%d: %s = %q", r.StatusCode, k, got)
			}
		}
	}
}

// A request whose event cannot be written returns nothing.
func TestRequestNeedsTheAuditEvent(t *testing.T) {
	h := newHarness(t, func(o *Options) { o.Events = failingEvents{} })
	h.seed("pw_01", h.clock.Now(), []string{canary}, nil)
	r := h.search(adminBearer, "id", "pw_01")
	if r.StatusCode != 503 || strings.Contains(r.body, "pw_01") || r.Header.Get("Retry-After") == "" {
		t.Fatalf("%d %s", r.StatusCode, r.body)
	}
}

func TestParseBearer(t *testing.T) {
	for in, want := range map[string]string{
		"Bearer a.b":          "a/b",
		"bearer a.b.c":        "a.b/c",
		"Bearer  a-1_x.tok":   "a-1_x/tok",
		"Bearer .tok":         "",
		"Bearer A.tok":        "",
		"Bearer a.":           "",
		"Bearer":              "",
		"Token a.b":           "",
		"Bearer a:b.c":        "",
		"Bearer " + "a.b c d": "a/b c d",
	} {
		n, tok, ok := parseBearer(in)
		got := ""
		if ok {
			got = n + "/" + tok
		}
		if got != want {
			t.Errorf("parseBearer(%q) = %q, want %q", in, got, want)
		}
	}
}

// A body without Content-Length is cut at the limit as well.
func TestStreamedBodyLimit(t *testing.T) {
	h := newHarness(t)
	req := httptest.NewRequest("POST", "/v1/search", io.MultiReader(strings.NewReader(`{"query":"`),
		strings.NewReader(strings.Repeat("a", 2*maxBody)), strings.NewReader(`"}`)))
	req.ContentLength = -1
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", readBearer)
	rec := httptest.NewRecorder()
	h.api.ServeHTTP(rec, req)
	if rec.Code != 413 || !strings.Contains(rec.Body.String(), `"too_large"`) {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
}
