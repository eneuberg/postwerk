package ui

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/eneuberg/postwerk/internal/addrhash"
	"github.com/eneuberg/postwerk/internal/auth"
	"github.com/eneuberg/postwerk/internal/query"
	"github.com/eneuberg/postwerk/internal/store"
	"github.com/eneuberg/postwerk/internal/testutil"
)

const (
	// adminToken is the token of "ops" in the tokens file.
	adminToken = "admin-token-for-tests"
	// viewerPassword is the password of "viewer", an account in the
	// database.
	viewerPassword = "viewer-password-for-tests"
)

var tokenFile = sync.OnceValue(func() string {
	a, err := auth.TokenLine("ops", auth.RoleAdmin, adminToken, auth.MinIterations)
	if err != nil {
		panic(err)
	}
	return a + "\n"
})

var viewerHash = sync.OnceValue(func() string {
	h, err := auth.Hash(viewerPassword, auth.MinIterations)
	if err != nil {
		panic(err)
	}
	return h
})

// clock is a settable time source.
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

// countingVerifier counts checks; block, if set, holds every check until
// it is closed.
type countingVerifier struct {
	Verifier
	calls atomic.Int32
	block chan struct{}
}

func (v *countingVerifier) Verify(name, token string) (auth.Role, bool) {
	v.calls.Add(1)
	if v.block != nil {
		<-v.block
	}
	return v.Verifier.Verify(name, token)
}

// failingEvents refuses every event.
type failingEvents struct{}

func (failingEvents) Append(context.Context, store.Event) error { return errors.New("disk full") }

type harness struct {
	t      *testing.T
	st     *store.Store
	ui     *UI
	clock  *clock
	logs   *testutil.SafeBuffer
	tokens *countingVerifier
	dbPath string
	sender string // of seeded mails; default "shop"
	// setupEvents is the number of events the harness wrote itself.
	setupEvents int
}

func newHarness(t *testing.T, opts ...func(*Options)) *harness {
	t.Helper()
	return newHarnessWithHasher(t, nil, opts...)
}

// newHarnessWithHasher lets the query layer find pseudonymized addresses.
func newHarnessWithHasher(t *testing.T, hs *addrhash.Hasher, opts ...func(*Options)) *harness {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "pw.db")
	st, err := store.Open(context.Background(), dbPath, store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	tokens, err := auth.ParseTokens(strings.NewReader(tokenFile()))
	if err != nil {
		t.Fatal(err)
	}
	h := &harness{t: t, st: st, clock: &clock{t: time.Date(2026, 11, 15, 12, 0, 0, 0, time.UTC)},
		logs: &testutil.SafeBuffer{}, tokens: &countingVerifier{Verifier: tokens}, dbPath: dbPath}
	if err := st.CreateUIAccount(context.Background(), "viewer", viewerHash(), "ops", h.clock.Now()); err != nil {
		t.Fatal(err)
	}
	h.setupEvents = len(h.events())
	o := Options{
		Query:      query.New(query.Options{Store: st, Hasher: hs, Targets: []string{"mailjet", "capture"}, Now: h.clock.Now}),
		Tokens:     h.tokens,
		Accounts:   st,
		Iterations: auth.MinIterations,
		Events:     st,
		Hosts:      []string{"relay.example.test"},
		Instance:   "test",
		Logger:     testutil.Logger(h.logs),
		Now:        h.clock.Now,
	}
	for _, f := range opts {
		f(&o)
	}
	h.ui, err = New(o)
	if err != nil {
		t.Fatal(err)
	}
	return h
}

// browser keeps cookies like a browser would for one origin.
type browser struct {
	h       *harness
	host    string
	cookies map[string]*http.Cookie
	header  http.Header // extra request headers
}

func (h *harness) browser() *browser {
	return &browser{h: h, host: "localhost:8081", cookies: map[string]*http.Cookie{}, header: http.Header{}}
}

type response struct {
	*http.Response
	body string
}

func (b *browser) do(method, path string, form url.Values) response {
	b.h.t.Helper()
	var body io.Reader
	if form != nil {
		body = strings.NewReader(form.Encode())
	}
	req := httptest.NewRequest(method, path, body)
	req.Host = b.host
	if form != nil {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	for k, v := range b.header {
		req.Header[k] = v
	}
	for _, c := range b.cookies {
		if strings.HasPrefix(req.URL.Path, c.Path) {
			req.AddCookie(&http.Cookie{Name: c.Name, Value: c.Value})
		}
	}
	rec := httptest.NewRecorder()
	b.h.ui.ServeHTTP(rec, req)
	resp := rec.Result()
	for _, c := range resp.Cookies() {
		if c.MaxAge < 0 {
			delete(b.cookies, c.Name)
		} else {
			b.cookies[c.Name] = c
		}
	}
	return response{Response: resp, body: rec.Body.String()}
}

func (b *browser) get(path string) response { return b.do("GET", path, nil) }

var csrfRe = regexp.MustCompile(`name="csrf" value="([^"]+)"`)

// csrf returns the CSRF token of the first form in a page.
func csrf(t *testing.T, page string) string {
	t.Helper()
	m := csrfRe.FindStringSubmatch(page)
	if m == nil {
		t.Fatalf("no csrf field in page:\n%s", page)
	}
	return m[1]
}

// login loads the form and posts name and token.
func (b *browser) login(name, token string) response {
	b.h.t.Helper()
	form := b.get("/login")
	return b.do("POST", "/login", url.Values{"csrf": {csrf(b.h.t, form.body)}, "name": {name}, "password": {token}})
}

func (h *harness) loggedIn(name, token string) *browser {
	h.t.Helper()
	b := h.browser()
	if r := b.login(name, token); r.StatusCode != http.StatusSeeOther || r.Header.Get("Location") != "/" {
		h.t.Fatalf("login as %s: %d %s", name, r.StatusCode, r.body)
	}
	return b
}

// events returns the UI events in the store.
func (h *harness) events() []store.Event {
	h.t.Helper()
	evs, err := h.st.EventsAfter(context.Background(), 0, 10000)
	if err != nil {
		h.t.Fatal(err)
	}
	return evs
}

func assertHeaders(t *testing.T, what string, r response) {
	t.Helper()
	want := map[string]string{
		"Content-Security-Policy": "default-src 'none'; style-src 'self'; img-src 'self'; form-action 'self'; frame-ancestors 'none'; base-uri 'none'",
		"X-Content-Type-Options":  "nosniff",
		"Referrer-Policy":         "no-referrer",
		"Cache-Control":           "no-store",
		"X-Frame-Options":         "DENY",
	}
	for k, v := range want {
		if got := r.Header.Get(k); got != v {
			t.Errorf("%s: %s = %q, want %q", what, k, got, v)
		}
	}
}

func TestHostCheck(t *testing.T) {
	h := newHarness(t)
	for host, ok := range map[string]bool{
		"localhost:8081":         true,
		"localhost":              true,
		"LocalHost:9000":         true, // kubectl port-forward 9000:8081
		"127.0.0.1:8081":         true,
		"[::1]:8081":             true,
		"[::1]":                  true,
		"relay.example.test":     true, // POSTWERK_ADMIN_HOSTS
		"relay.example.test:443": true,
		"":                       false,
		"evil.example:8081":      false, // DNS rebinding: the attacker's name
		"localhost.evil.example": false,
		"127.0.0.1.nip.io:8081":  false,
		"127.0.0.2:8081":         false,
		"[::ffff:127.0.0.1]":     false,
		"localhost.:8081":        false,
	} {
		b := h.browser()
		b.host = host
		r := b.get("/login")
		if ok && r.StatusCode != 200 || !ok && r.StatusCode != 403 {
			t.Errorf("Host %q: %d", host, r.StatusCode)
		}
		assertHeaders(t, "Host "+host, r)
		if !ok && strings.Contains(r.body, "<form") {
			t.Errorf("Host %q got a form", host)
		}
	}
}

func TestSecurityHeadersOnEveryResponse(t *testing.T) {
	h := newHarness(t)
	anon := h.browser()
	b := h.loggedIn("ops", adminToken)
	checks := []struct {
		what string
		r    response
	}{
		{"login form", anon.get("/login")},
		{"redirect to login", anon.get("/")},
		{"login without csrf", anon.do("POST", "/login", url.Values{"name": {"ops"}})},
		{"stylesheet", anon.get("/static/ui.css")},
		{"logo", anon.get("/static/logo.svg")},
		{"unknown static", anon.get("/static/../ui.go")},
		{"not found", anon.get("/nope")},
		{"wrong method", anon.do("PUT", "/login", url.Values{})},
		{"overview", b.get("/")},
		{"logout without csrf", b.do("POST", "/logout", url.Values{})},
	}
	for _, c := range checks {
		assertHeaders(t, c.what, c.r)
	}
	if r := anon.get("/static/ui.css"); r.StatusCode != 200 || r.Header.Get("Content-Type") != "text/css; charset=utf-8" {
		t.Fatalf("stylesheet: %d %s", r.StatusCode, r.Header.Get("Content-Type"))
	}
	if r := anon.get("/static/"); r.StatusCode != 404 {
		t.Fatalf("static directory listing: %d", r.StatusCode)
	}
	// No JavaScript anywhere.
	for _, c := range checks {
		if strings.Contains(strings.ToLower(c.r.body), "<script") {
			t.Errorf("%s: script tag", c.what)
		}
	}
}

func TestLoginCSRF(t *testing.T) {
	h := newHarness(t)
	b := h.browser()
	form := b.get("/login")
	token := csrf(t, form.body)
	c := b.cookies[loginCookie]
	if c == nil || c.Value != token || !c.HttpOnly || c.SameSite != http.SameSiteStrictMode || c.Path != "/login" {
		t.Fatalf("login cookie = %+v", c)
	}
	for name, f := range map[string]url.Values{
		"missing field": {"name": {"ops"}, "password": {adminToken}},
		"wrong field":   {"csrf": {strings.Repeat("A", 43)}, "name": {"ops"}, "password": {adminToken}},
	} {
		if r := b.do("POST", "/login", f); r.StatusCode != 403 {
			t.Errorf("%s: %d", name, r.StatusCode)
		}
	}
	// A form without the cookie (posted from another browser, or by a
	// page that cannot read the cookie).
	other := h.browser()
	if r := other.do("POST", "/login", url.Values{"csrf": {token}, "name": {"ops"}, "password": {adminToken}}); r.StatusCode != 403 {
		t.Errorf("no cookie: %d", r.StatusCode)
	}
	if n := h.ui.checks.Load(); n != 0 {
		t.Fatalf("token checked %d times before the CSRF check passed", n)
	}
	if _, ok := other.cookies[sessionCookie]; ok {
		t.Fatal("session without a valid form")
	}
}

func TestLoginSuccessAndFailure(t *testing.T) {
	h := newHarness(t)
	b := h.browser()

	r := b.login("ops", "wrong")
	if r.StatusCode != http.StatusUnauthorized || !strings.Contains(r.body, "Name or password is wrong.") {
		t.Fatalf("wrong token: %d", r.StatusCode)
	}
	r2 := b.login("nobody", "wrong")
	if r2.StatusCode != http.StatusUnauthorized || r2.body != strings.Replace(r.body, csrf(t, r.body), csrf(t, r2.body), 1) {
		t.Fatal("an unknown name is answered differently from a wrong token")
	}
	// A token pasted into the name field must not end up anywhere.
	b.login(adminToken, "")
	if _, ok := b.cookies[sessionCookie]; ok {
		t.Fatal("session after failed logins")
	}

	r = b.login("ops", adminToken)
	if r.StatusCode != http.StatusSeeOther || r.Header.Get("Location") != "/" {
		t.Fatalf("good login: %d", r.StatusCode)
	}
	c := b.cookies[sessionCookie]
	if c == nil || !c.HttpOnly || c.SameSite != http.SameSiteStrictMode || c.Path != "/" || c.Secure || c.MaxAge != 0 ||
		!wellFormedToken(c.Value) {
		t.Fatalf("session cookie = %+v", c)
	}
	if _, ok := b.cookies[loginCookie]; ok {
		t.Fatal("login cookie kept after login")
	}
	if page := b.get("/"); page.StatusCode != 200 || !strings.Contains(page.body, ">ops<") || !strings.Contains(page.body, ">file<") {
		t.Fatalf("overview after login: %d", page.StatusCode)
	}
	// Logged in, the login page sends you on.
	if r := b.get("/login"); r.StatusCode != http.StatusSeeOther {
		t.Fatalf("login page while logged in: %d", r.StatusCode)
	}

	evs := h.events()[h.setupEvents:]
	var kinds []string
	for _, e := range evs {
		kinds = append(kinds, e.Kind+"/"+e.Actor+"/"+e.Text+"/"+string(e.Level))
	}
	want := []string{
		"ui.login_failed/ops//warn", "ui.login_failed///warn", "ui.login_failed///warn", "ui.login/ops/file/info",
	}
	if strings.Join(kinds, " ") != strings.Join(want, " ") {
		t.Fatalf("events = %q, want %q", kinds, want)
	}
	for _, leak := range []string{adminToken, "wrong"} {
		if strings.Contains(h.logs.String(), leak) {
			t.Errorf("log contains %q", leak)
		}
		for _, e := range evs {
			if strings.Contains(e.Actor+e.Text, leak) {
				t.Errorf("event contains %q", leak)
			}
		}
	}
}

// Every login attempt costs exactly one password check, whatever was
// typed, so its time does not depend on the name (U5). The auth package
// proves that each check costs the same number of PBKDF2 rounds, for the
// tokens file (padded by the checker), database hashes and unknown names.
func TestLoginTimingPath(t *testing.T) {
	h := newHarness(t)
	for _, c := range [][2]string{
		{"ops", "wrong"}, {"nobody", "wrong"}, {"", ""}, {"ops", ""}, {"ops", strings.Repeat("x", 5000)},
		{strings.Repeat("n", 300), "x"}, {"ops", adminToken}, {"viewer", "wrong"}, {"viewer", viewerPassword},
	} {
		before := h.ui.checks.Load()
		h.browser().login(c[0], c[1])
		if n := h.ui.checks.Load() - before; n != 1 {
			t.Errorf("login %.20q cost %d checks, want 1", c[0], n)
		}
	}
}

func TestLoginConcurrencyLimit(t *testing.T) {
	h := newHarness(t, func(o *Options) { o.LoginSlots = 1; o.LoginWait = 50 * time.Millisecond })
	h.tokens.block = make(chan struct{})
	first := h.browser()
	form := first.get("/login")
	done := make(chan response)
	go func() {
		done <- first.do("POST", "/login", url.Values{"csrf": {csrf(t, form.body)}, "name": {"ops"}, "password": {adminToken}})
	}()
	testutil.Eventually(t, 5*time.Second, "first check running", func() bool { return h.tokens.calls.Load() == 1 })

	start := time.Now()
	r := h.browser().login("ops", adminToken)
	if r.StatusCode != http.StatusServiceUnavailable || !strings.Contains(r.body, "Too many logins") {
		t.Fatalf("second login: %d", r.StatusCode)
	}
	if time.Since(start) > 3*time.Second || h.tokens.calls.Load() != 1 {
		t.Fatal("second login was not refused promptly without a check")
	}
	close(h.tokens.block)
	if r := <-done; r.StatusCode != http.StatusSeeOther {
		t.Fatalf("first login: %d", r.StatusCode)
	}
}

func TestLoginNeedsTheAuditEvent(t *testing.T) {
	h := newHarness(t, func(o *Options) { o.Events = failingEvents{} })
	b := h.browser()
	if r := b.login("ops", adminToken); r.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("login without event: %d", r.StatusCode)
	}
	if _, ok := b.cookies[sessionCookie]; ok || h.ui.sessions.len() != 0 {
		t.Fatal("session created although the login event was not written")
	}
}

func TestSessionExpiry(t *testing.T) {
	h := newHarness(t)

	// Idle timeout: 30 minutes without a request.
	b := h.loggedIn("ops", adminToken)
	h.clock.Add(29 * time.Minute)
	if r := b.get("/"); r.StatusCode != 200 {
		t.Fatalf("after 29 min: %d", r.StatusCode)
	}
	h.clock.Add(30 * time.Minute)
	if r := b.get("/"); r.StatusCode != http.StatusSeeOther || r.Header.Get("Location") != "/login" {
		t.Fatalf("after 30 idle min: %d", r.StatusCode)
	}

	// Absolute limit: 8 hours, however active.
	b = h.loggedIn("ops", adminToken)
	for range 23 {
		h.clock.Add(20 * time.Minute)
		if r := b.get("/"); r.StatusCode != 200 {
			t.Fatalf("active session ended early: %d", r.StatusCode)
		}
	}
	h.clock.Add(20 * time.Minute) // 8 h
	if r := b.get("/"); r.StatusCode != http.StatusSeeOther {
		t.Fatalf("after 8 h: %d", r.StatusCode)
	}
	if h.ui.sessions.len() != 0 {
		t.Fatalf("%d sessions left", h.ui.sessions.len())
	}
}

func TestSessionIDIsNew(t *testing.T) {
	h := newHarness(t)
	b := h.browser()
	planted := strings.Repeat("a", 43)
	b.cookies[sessionCookie] = &http.Cookie{Name: sessionCookie, Value: planted, Path: "/"}
	if r := b.get("/"); r.StatusCode != http.StatusSeeOther {
		t.Fatalf("planted cookie accepted: %d", r.StatusCode)
	}
	b.login("ops", adminToken)
	if c := b.cookies[sessionCookie]; c == nil || c.Value == planted {
		t.Fatal("login kept the planted session id")
	}
	// Two logins, two sessions.
	other := h.loggedIn("ops", adminToken)
	if other.cookies[sessionCookie].Value == b.cookies[sessionCookie].Value {
		t.Fatal("same session id twice")
	}
}

func TestLogout(t *testing.T) {
	h := newHarness(t)
	b := h.loggedIn("ops", adminToken)
	page := b.get("/")
	token := csrf(t, page.body)

	if r := b.do("POST", "/logout", url.Values{}); r.StatusCode != 403 {
		t.Fatalf("logout without csrf: %d", r.StatusCode)
	}
	if r := b.do("POST", "/logout", url.Values{"csrf": {otherFirst(token)}}); r.StatusCode != 403 {
		t.Fatalf("logout with a wrong csrf: %d", r.StatusCode)
	}
	if r := b.get("/"); r.StatusCode != 200 {
		t.Fatal("refused logouts ended the session")
	}
	// The token of another session does not work either.
	other := h.loggedIn("viewer", viewerPassword)
	if r := b.do("POST", "/logout", url.Values{"csrf": {csrf(t, other.get("/").body)}}); r.StatusCode != 403 {
		t.Fatalf("logout with another session's csrf: %d", r.StatusCode)
	}

	stolen := b.cookies[sessionCookie].Value
	r := b.do("POST", "/logout", url.Values{"csrf": {token}})
	if r.StatusCode != http.StatusSeeOther || r.Header.Get("Location") != "/login" {
		t.Fatalf("logout: %d", r.StatusCode)
	}
	if _, ok := b.cookies[sessionCookie]; ok {
		t.Fatal("cookie not cleared")
	}
	// The old id is dead on the server too.
	b.cookies[sessionCookie] = &http.Cookie{Name: sessionCookie, Value: stolen, Path: "/"}
	if r := b.get("/"); r.StatusCode != http.StatusSeeOther {
		t.Fatalf("session alive after logout: %d", r.StatusCode)
	}
	if r := other.get("/"); r.StatusCode != 200 {
		t.Fatal("logout ended another session")
	}
}

func TestCrossSitePOSTRefused(t *testing.T) {
	h := newHarness(t)
	b := h.loggedIn("ops", adminToken)
	token := csrf(t, b.get("/").body)
	b.header.Set("Sec-Fetch-Site", "cross-site")
	if r := b.do("POST", "/logout", url.Values{"csrf": {token}}); r.StatusCode != 403 {
		t.Fatalf("cross-site POST with a valid token: %d", r.StatusCode)
	}
	b.header = http.Header{"Origin": {"http://evil.example"}}
	if r := b.do("POST", "/logout", url.Values{"csrf": {token}}); r.StatusCode != 403 {
		t.Fatalf("POST from another origin: %d", r.StatusCode)
	}
	b.header = http.Header{"Origin": {"http://localhost:8081"}, "Sec-Fetch-Site": {"same-origin"}}
	if r := b.do("POST", "/logout", url.Values{"csrf": {token}}); r.StatusCode != http.StatusSeeOther {
		t.Fatalf("same-origin logout: %d", r.StatusCode)
	}
}

// otherFirst returns s with its first character replaced by a different
// one, so the result never equals s (a random token can start with any
// character, "x" too).
func otherFirst(s string) string {
	if s[0] == 'x' {
		return "y" + s[1:]
	}
	return "x" + s[1:]
}
