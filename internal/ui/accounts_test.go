package ui

import (
	"context"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/eneuberg/postwerk/internal/store"
)

const newPassword = "a new password, long enough"

// post loads a page and posts form to path with that page's CSRF token.
func (b *browser) post(page, path string, form url.Values) response {
	b.h.t.Helper()
	form.Set("csrf", csrf(b.h.t, b.get(page).body))
	return b.do("POST", path, form)
}

func (h *harness) uiEvents(kinds ...string) []string {
	h.t.Helper()
	var out []string
	for _, e := range h.events() {
		if slices.Contains(kinds, e.Kind) {
			out = append(out, e.Kind+"/"+e.Actor+"/"+e.Text)
		}
	}
	return out
}

func TestAccountLifecycle(t *testing.T) {
	h := newHarness(t)
	ops := h.loggedIn("ops", adminToken)

	list := ops.get("/settings")
	for _, want := range []string{">ops<", ">tokens file<", `href="/settings/accounts/viewer"`, ">database<", "by ops"} {
		if !strings.Contains(list.body, want) {
			t.Errorf("account list lacks %q", want)
		}
	}

	create := func(b *browser, name, pw, again, current string) response {
		return b.post("/settings", "/settings/accounts", url.Values{"name": {name}, "password": {pw}, "confirm": {again}, "current": {current}})
	}
	for _, c := range []struct {
		name, pw, again, current string
		status                   int
		msg                      string
	}{
		{"alice", newPassword, newPassword, "wrong", 403, "Your current password is wrong."},
		{"alice", "too short", "too short", adminToken, 400, "at least 15 characters"},
		{"alice", strings.Repeat("x", 129), strings.Repeat("x", 129), adminToken, 400, "at most 128 characters"},
		{"alice.example.ops", "ALICE.EXAMPLE.OPS", "ALICE.EXAMPLE.OPS", adminToken, 400, "must not be the account name"},
		{"alice", newPassword, newPassword + "!", adminToken, 400, "not the same"},
		{"Alice", newPassword, newPassword, adminToken, 400, "Names have 1 to 64 characters"},
		{"ops", newPassword, newPassword, adminToken, 400, "in the tokens file"},
		{"viewer", newPassword, newPassword, adminToken, 409, "An account with this name exists."},
	} {
		r := create(ops, c.name, c.pw, c.again, c.current)
		if r.StatusCode != c.status || !strings.Contains(r.body, c.msg) {
			t.Errorf("create %s: %d, want %d %q", c.name, r.StatusCode, c.status, c.msg)
		}
		if strings.Contains(r.body, c.pw) && c.pw != c.name {
			t.Errorf("create %s: the password is echoed", c.name)
		}
	}
	r := create(ops, "alice", newPassword, newPassword, adminToken)
	if r.StatusCode != http.StatusSeeOther || r.Header.Get("Location") != "/settings" {
		t.Fatalf("create: %d %s", r.StatusCode, r.body)
	}
	if page := ops.get("/settings"); !strings.Contains(page.body, "Account alice created.") {
		t.Error("no notice after create")
	} else if again := ops.get("/settings"); strings.Contains(again.body, "Account alice created.") {
		t.Error("the notice stays")
	}

	// alice logs in with her password, twice; ops sets a new one, which
	// ends both sessions at once.
	alice1 := h.loggedIn("alice", newPassword)
	alice2 := h.loggedIn("alice", newPassword)
	viewer := h.loggedIn("viewer", viewerPassword)
	setPw := func(b *browser, name, pw, current string) response {
		return b.post("/settings/accounts/"+name, "/settings/accounts/"+name+"/password",
			url.Values{"password": {pw}, "confirm": {pw}, "current": {current}})
	}
	if r := setPw(ops, "alice", "another new password", "wrong"); r.StatusCode != 403 {
		t.Fatalf("set password with a wrong confirmation: %d", r.StatusCode)
	}
	if alice1.get("/").StatusCode != 200 {
		t.Fatal("a refused change ended the session")
	}
	if r := setPw(ops, "alice", "another new password", adminToken); r.StatusCode != http.StatusSeeOther {
		t.Fatalf("set password: %d %s", r.StatusCode, r.body)
	}
	for i, b := range []*browser{alice1, alice2} {
		if r := b.get("/"); r.StatusCode != http.StatusSeeOther {
			t.Errorf("alice session %d alive after the new password: %d", i, r.StatusCode)
		}
	}
	if viewer.get("/").StatusCode != 200 || ops.get("/").StatusCode != 200 {
		t.Fatal("other sessions ended")
	}
	if r := h.browser().login("alice", newPassword); r.StatusCode != http.StatusUnauthorized {
		t.Fatalf("old password still works: %d", r.StatusCode)
	}

	// alice changes her own password: her session ends, she logs in again.
	alice := h.loggedIn("alice", "another new password")
	if r := setPw(alice, "alice", "alice's own password!", "another new password"); r.StatusCode != http.StatusSeeOther ||
		r.Header.Get("Location") != "/login?done=password" {
		t.Fatalf("own password: %d %s", r.StatusCode, r.Header.Get("Location"))
	}
	if _, ok := alice.cookies[sessionCookie]; ok {
		t.Fatal("cookie kept after the own password changed")
	}
	if r := alice.get("/login?done=password"); !strings.Contains(r.body, "Your password was changed.") {
		t.Error("no notice on the login page")
	}
	alice = h.loggedIn("alice", "alice's own password!")

	// The tokens file's account cannot be changed here.
	if r := setPw(alice, "ops", newPassword, "alice's own password!"); r.StatusCode != 400 || !strings.Contains(r.body, "tokens file") {
		t.Fatalf("set password of a file account: %d", r.StatusCode)
	}
	if r := alice.post("/settings/accounts/ops", "/settings/accounts/ops/delete", url.Values{"current": {"alice's own password!"}}); r.StatusCode != 400 {
		t.Fatalf("delete a file account: %d", r.StatusCode)
	}
	if r := alice.get("/settings/accounts/nobody"); r.StatusCode != 404 {
		t.Fatalf("unknown account page: %d", r.StatusCode)
	}

	// Delete viewer: its session ends.
	if r := alice.post("/settings/accounts/viewer", "/settings/accounts/viewer/delete", url.Values{"current": {"wrong"}}); r.StatusCode != 403 {
		t.Fatalf("delete with a wrong confirmation: %d", r.StatusCode)
	}
	if r := alice.post("/settings/accounts/viewer", "/settings/accounts/viewer/delete", url.Values{"current": {"alice's own password!"}}); r.StatusCode != http.StatusSeeOther {
		t.Fatalf("delete: %d %s", r.StatusCode, r.body)
	}
	if r := viewer.get("/"); r.StatusCode != http.StatusSeeOther {
		t.Fatal("deleted account still logged in")
	}
	// alice is the last account in the database.
	if r := alice.post("/settings/accounts/alice", "/settings/accounts/alice/delete", url.Values{"current": {"alice's own password!"}}); r.StatusCode != http.StatusConflict ||
		!strings.Contains(r.body, "last account") {
		t.Fatalf("delete the last account: %d", r.StatusCode)
	}
	if alice.get("/").StatusCode != 200 {
		t.Fatal("refused delete ended the session")
	}

	want := []string{
		"ui.account_created/ops/viewer", // by the harness
		"ui.account_created/ops/alice",
		"ui.password_set/ops/alice",
		"ui.password_set/alice/alice",
		"ui.account_deleted/alice/viewer",
	}
	if got := h.uiEvents(store.KindUIAccountCreated, store.KindUIPasswordSet, store.KindUIAccountDeleted); !slices.Equal(got, want) {
		t.Fatalf("account events = %q\nwant %q", got, want)
	}
	if got := h.uiEvents(store.KindUILoginFailed); !slices.Contains(got, "ui.login_failed/ops/confirm") ||
		!slices.Contains(got, "ui.login_failed/alice/confirm") {
		t.Errorf("failed confirmations = %q", got)
	}
	// No password anywhere: not in events, logs or the accounts table.
	accounts, err := h.st.UIAccounts(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, pw := range []string{newPassword, "another new password", "alice's own password!", adminToken} {
		if strings.Contains(h.logs.String(), pw) {
			t.Errorf("log has a password")
		}
		for _, e := range h.events() {
			if strings.Contains(e.Text+e.Actor, pw) {
				t.Errorf("event %s has a password", e.Kind)
			}
		}
		for _, a := range accounts {
			if strings.Contains(a.Hash, pw) || !strings.HasPrefix(a.Hash, "pbkdf2-sha256$") {
				t.Errorf("stored hash of %s = %q", a.Name, a.Hash)
			}
		}
	}
}

// Every change is a POST with the session's CSRF token.
func TestAccountChangesNeedCSRF(t *testing.T) {
	h := newHarness(t)
	ops := h.loggedIn("ops", adminToken)
	for _, path := range []string{"/settings/accounts", "/settings/accounts/viewer/password", "/settings/accounts/viewer/delete"} {
		form := url.Values{"name": {"mallory"}, "password": {newPassword}, "confirm": {newPassword}, "current": {adminToken}}
		if r := ops.do("POST", path, form); r.StatusCode != 403 {
			t.Errorf("%s without csrf: %d", path, r.StatusCode)
		}
		form.Set("csrf", strings.Repeat("A", 43))
		if r := ops.do("POST", path, form); r.StatusCode != 403 {
			t.Errorf("%s with a wrong csrf: %d", path, r.StatusCode)
		}
		if r := ops.get(path + "?name=mallory"); path != "/settings/accounts" && r.StatusCode != http.StatusMethodNotAllowed {
			t.Errorf("GET %s: %d", path, r.StatusCode)
		}
		if r := h.browser().do("POST", path, form); r.StatusCode != http.StatusSeeOther {
			t.Errorf("%s without a session: %d", path, r.StatusCode)
		}
	}
	if n, _ := h.st.CountUIAccounts(context.Background()); n != 1 {
		t.Fatalf("%d accounts", n)
	}
	if h.ui.checks.Load() != 1 { // the login only
		t.Fatalf("%d password checks", h.ui.checks.Load())
	}
}

// The accounts are under Settings; the old addresses send the browser
// there, without the query, and the old forms are gone.
func TestSettingsPages(t *testing.T) {
	h := newHarness(t)
	ops := h.loggedIn("ops", adminToken)
	viewer := h.loggedIn("viewer", viewerPassword)

	page := viewer.get("/settings")
	nav := regexp.MustCompile(`(?s)<nav aria-label="Main">(.*?)</nav>`).FindStringSubmatch(page.body)
	if page.StatusCode != 200 || nav == nil {
		t.Fatalf("settings: %d", page.StatusCode)
	}
	links := regexp.MustCompile(`>([A-Za-z]+)</a>`).FindAllStringSubmatch(nav[1], -1)
	var names []string
	for _, l := range links {
		names = append(names, l[1])
	}
	if !slices.Equal(names, []string{"Overview", "Mails", "Settings"}) {
		t.Errorf("navigation: %v", names)
	}
	for _, want := range []string{`href="/settings" aria-current="page"`, "<h1>Settings</h1>", "Your account",
		`href="/settings/accounts/viewer">Change your password`, `action="/settings/accounts"`} {
		if !strings.Contains(page.body, want) {
			t.Errorf("settings page lacks %q", want)
		}
	}
	if p := ops.get("/settings").body; strings.Contains(p, "Change your password") || !strings.Contains(p, "cannot be changed here") {
		t.Error("the settings page offers a password change for a tokens file account")
	}
	if p := viewer.get("/settings/accounts/viewer").body; !strings.Contains(p, `aria-current="page">Settings`) ||
		!strings.Contains(p, `action="/settings/accounts/viewer/password"`) || !strings.Contains(p, `href="/settings">`) {
		t.Error("the account page is not under Settings")
	}

	for old, want := range map[string]string{
		"/accounts":                    "/settings",
		"/accounts?name=x":             "/settings",
		"/accounts/viewer":             "/settings/accounts/viewer",
		"/accounts/viewer?x=" + canary: "/settings/accounts/viewer",
		"/settings/accounts":           "/settings",
	} {
		if r := ops.get(old); r.StatusCode != http.StatusMovedPermanently || r.Header.Get("Location") != want {
			t.Errorf("GET %s: %d %q", old, r.StatusCode, r.Header.Get("Location"))
		}
	}
	if r := ops.get("/accounts/Not%20A%20Name"); r.StatusCode != 404 {
		t.Errorf("old address with a bad name: %d", r.StatusCode)
	}
	if r := h.browser().get("/accounts"); r.Header.Get("Location") != "/login" {
		t.Errorf("old address without a session: %d %q", r.StatusCode, r.Header.Get("Location"))
	}
	token := csrf(t, page.body)
	for _, path := range []string{"/accounts", "/accounts/viewer/password", "/accounts/viewer/delete"} {
		form := url.Values{"csrf": {token}, "name": {"mallory"}, "password": {newPassword}, "confirm": {newPassword}, "current": {viewerPassword}}
		if r := viewer.do("POST", path, form); r.StatusCode != http.StatusMethodNotAllowed && r.StatusCode != http.StatusNotFound {
			t.Errorf("POST %s: %d", path, r.StatusCode)
		}
	}
	if n, _ := h.st.CountUIAccounts(context.Background()); n != 1 {
		t.Fatalf("%d accounts", n)
	}
}

// Failed logins slow down per name and per source, the same for names
// that exist and names that do not (U22).
func TestLoginThrottle(t *testing.T) {
	h := newHarness(t)
	for _, name := range []string{"viewer", "nobody"} {
		b := h.browser()
		for i := range accountFreeFailures {
			if r := b.login(name, "wrong"); r.StatusCode != http.StatusUnauthorized {
				t.Fatalf("%s failure %d: %d", name, i, r.StatusCode)
			}
		}
		// The sixth failure starts a wait of one minute.
		b.login(name, "wrong")
		before := h.ui.checks.Load()
		r := b.login(name, viewerPassword)
		if r.StatusCode != http.StatusTooManyRequests || !strings.Contains(r.body, "try again in a minute") {
			t.Fatalf("%s while waiting: %d", name, r.StatusCode)
		}
		if h.ui.checks.Load() != before {
			t.Fatalf("%s: checked while waiting", name)
		}
	}
	// Another name from the same source is not affected yet.
	if r := h.browser().login("ops", adminToken); r.StatusCode != http.StatusSeeOther {
		t.Fatalf("ops: %d", r.StatusCode)
	}
	h.clock.Add(time.Minute)
	if r := h.browser().login("viewer", viewerPassword); r.StatusCode != http.StatusSeeOther {
		t.Fatalf("after the wait: %d", r.StatusCode)
	}
	// A good login cleared the name: five free failures again.
	for range accountFreeFailures {
		h.browser().login("viewer", "wrong")
	}
	if r := h.browser().login("viewer", viewerPassword); r.StatusCode != http.StatusSeeOther {
		t.Fatalf("after reset: %d", r.StatusCode)
	}

	// Waits double: 1, 2, 4 minutes.
	h2 := newHarness(t)
	for range accountFreeFailures {
		h2.browser().login("viewer", "wrong")
	}
	for _, want := range []time.Duration{time.Minute, 2 * time.Minute, 4 * time.Minute} {
		h2.browser().login("viewer", "wrong")
		h2.clock.Add(want - time.Second)
		if r := h2.browser().login("viewer", viewerPassword); r.StatusCode != http.StatusTooManyRequests {
			t.Fatalf("before %v: %d", want, r.StatusCode)
		}
		h2.clock.Add(time.Second)
	}

	got := h.uiEvents(store.KindUILoginThrottled)
	if want := []string{"ui.login_throttled/viewer/account", "ui.login_throttled//account"}; !slices.Equal(got, want) {
		t.Fatalf("throttle events = %q, want %q", got, want)
	}
}

func TestLoginThrottleBySource(t *testing.T) {
	h := newHarness(t)
	for i := range sourceFreeFailures {
		h.browser().login(strings.Repeat("n", i%60+1), "wrong") // a different name each time
	}
	h.browser().login("someone", "wrong")
	if r := h.browser().login("ops", adminToken); r.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("same source after %d failures: %d", sourceFreeFailures+1, r.StatusCode)
	}
	if got := h.uiEvents(store.KindUILoginThrottled); !slices.Equal(got, []string{"ui.login_throttled//source"}) {
		t.Fatalf("throttle events = %q", got)
	}
	h.clock.Add(time.Minute)
	if r := h.browser().login("ops", adminToken); r.StatusCode != http.StatusSeeOther {
		t.Fatalf("after the wait: %d", r.StatusCode)
	}
}

func TestThrottleMemoryIsBounded(t *testing.T) {
	c := &clock{t: time.Unix(0, 0)}
	th := newThrottle(c.Now, 1, time.Hour)
	for i := range maxThrottleKeys + 50 {
		c.Add(time.Millisecond)
		th.fail(strings.Repeat("k", i%40+1) + string(rune('a'+i%26)) + time.Duration(i).String())
	}
	if len(th.m) > maxThrottleKeys {
		t.Fatalf("%d keys", len(th.m))
	}
}
