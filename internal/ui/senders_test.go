package ui

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/eneuberg/postwerk/internal/query"
	"github.com/eneuberg/postwerk/internal/senders"
	"github.com/eneuberg/postwerk/internal/store"
)

// senderHarness is a harness with senders: "app" in the senders file (a key
// and a policy) and, when managed, the database as the write side
// (POSTWERK_SENDER_KEYS=both); without it, file only.
type senderHarness struct {
	*harness
	db        *senders.DBSource
	reg       *senders.Registry
	fileKeyID string
}

func newSenderHarness(t *testing.T, managed bool) *senderHarness {
	t.Helper()
	sh := &senderHarness{}
	key, id := senders.NewKey()
	sh.fileKeyID = id
	line, err := senders.KeyLine("app", key)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "senders")
	if err := os.WriteFile(path, []byte(line+"\napp:policy from=*@app.example.test max_per_minute=60\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	file, err := senders.NewFileSource(senders.FileOptions{Path: path, AllowEmpty: true})
	if err != nil {
		t.Fatal(err)
	}
	sh.harness = newHarness(t, func(o *Options) {
		st := o.Accounts.(*store.Store)
		if managed {
			if sh.db, err = senders.NewDBSource(context.Background(), st, o.Now); err != nil {
				t.Fatal(err)
			}
			o.Senders = sh.db
		}
		if sh.reg, err = senders.NewRegistry(file, sh.db); err != nil {
			t.Fatal(err)
		}
		o.Query = query.New(query.Options{Store: st, Targets: []string{"mailjet", "capture"}, Now: o.Now, Senders: sh.reg})
		o.SMTPPort = "2525"
	})
	return sh
}

var keyRe = regexp.MustCompile(`pwk_[a-z2-7]{12}_[a-z2-7]{52}`)

// postForm loads page and posts form to path with the page's CSRF token
// and the acting account's password as "current".
func (b *browser) change(page, path, current string, form url.Values) response {
	b.h.t.Helper()
	if form == nil {
		form = url.Values{}
	}
	form.Set("current", current)
	return b.post(page, path, form)
}

func (h *harness) senderEvents() []string {
	h.t.Helper()
	var out []string
	for _, e := range h.events() {
		if strings.HasPrefix(e.Kind, "sender.") {
			out = append(out, e.Kind+"/"+e.Sender+"/"+e.Actor+"/"+e.Text)
		}
	}
	return out
}

// crawl gets every page reachable from start and returns their bodies.
func (b *browser) crawl(start ...string) map[string]string {
	b.h.t.Helper()
	hrefRe := regexp.MustCompile(`href="(/[^"]*)"`)
	pages := map[string]string{}
	queue := start
	for len(queue) > 0 && len(pages) < 300 {
		p, _, _ := strings.Cut(strings.ReplaceAll(queue[0], "&amp;", "&"), "#")
		queue = queue[1:]
		if _, ok := pages[p]; ok || strings.HasPrefix(p, "/static/") {
			continue
		}
		r := b.get(p)
		if r.StatusCode != 200 {
			b.h.t.Fatalf("GET %s: %d", p, r.StatusCode)
		}
		pages[p] = r.body
		for _, m := range hrefRe.FindAllStringSubmatch(r.body, -1) {
			queue = append(queue, m[1])
		}
	}
	return pages
}

func TestSenderKeysList(t *testing.T) {
	sh := newSenderHarness(t, true)
	ctx := context.Background()
	if _, err := sh.db.Create(ctx, senders.CreateRequest{Name: "billing", Actor: "ops"}); err != nil {
		t.Fatal(err)
	}
	// A sender in no source any more, but with a mail in the last 24 hours.
	sh.sender = "gone"
	sh.seedMail("pw_GONE1", sh.clock.Now().Add(-time.Hour), []string{"x@example.test"}, map[string][]store.Status{"mailjet": {store.Failed}})
	b := sh.loggedIn("ops", adminToken)
	page := b.get("/settings")
	for _, want := range []string{
		`id="senders"`, `href="/settings/senders/app"`, ">senders file<", `href="/settings/senders/billing"`, ">database<",
		`href="/settings/senders/gone"`, ">removed<", "*@app.example.test", "60 messages per minute",
		"any sender address", sh.fileKeyID, "1 failed recipient", `href="/mails?sender=gone"`, "New sender…",
	} {
		if !strings.Contains(page.body, want) {
			t.Errorf("settings lack %q", want)
		}
	}
	gone := b.get("/settings/senders/gone")
	if gone.StatusCode != 200 || !strings.Contains(gone.body, "in no source any more") || strings.Contains(gone.body, "<form method=\"post\" action=\"/settings/senders") {
		t.Fatalf("removed sender page: %d", gone.StatusCode)
	}
	if shop := b.get("/settings/senders/billing"); !strings.Contains(shop.body, `href="/stats?range=7d&amp;sender=billing"`) {
		t.Error("the sender page does not link to its statistics")
	}
	if r := b.get("/settings/senders/nobody"); r.StatusCode != 404 {
		t.Fatalf("unknown sender: %d", r.StatusCode)
	}
	if r := b.get("/settings/senders/Bad%20Name"); r.StatusCode != 404 {
		t.Fatalf("invalid name: %d", r.StatusCode)
	}

	// File only: no form, a hint how to add senders.
	fh := newSenderHarness(t, false)
	fp := fh.loggedIn("ops", adminToken).get("/settings")
	if strings.Contains(fp.body, "New sender…") || !strings.Contains(fp.body, "postwerk gen-sender NAME") {
		t.Fatalf("file-only settings:\n%s", fp.body)
	}
	if r := fh.loggedIn("ops", adminToken).change("/settings", "/settings/senders", adminToken,
		url.Values{"name": {"shop"}}); r.StatusCode != http.StatusConflict || !strings.Contains(r.body, "senders file only") {
		t.Fatalf("create in file mode: %d", r.StatusCode)
	}
}

// The key is shown once, in the response to the POST that made it, and
// nowhere else: not on a later page, not in an event, not in the log.
func TestCreateSenderShowsTheKeyOnce(t *testing.T) {
	sh := newSenderHarness(t, true)
	b := sh.loggedIn("viewer", viewerPassword)
	create := func(name, current string, extra url.Values) response {
		form := url.Values{"name": {name}, "from": {"*@shop.example.test\nnoreply@example.test"},
			"max_per_minute": {"30"}, "max_rcpt_per_hour": {""}, "expires": {"2026-12-31"}}
		for k, v := range extra {
			form[k] = v
		}
		return b.change("/settings", "/settings/senders", current, form)
	}
	for _, c := range []struct {
		name, current string
		extra         url.Values
		status        int
		msg           string
	}{
		{"shop", "wrong", nil, 403, "Your current password is wrong."},
		{"Shop", viewerPassword, nil, 400, "sender names have 1 to 64 characters"},
		{"app", viewerPassword, nil, 409, "A sender with this name exists"},
		{"shop", viewerPassword, url.Values{"from": {"no-at-sign"}}, 400, "From pattern 1"},
		{"shop", viewerPassword, url.Values{"max_per_minute": {"-1"}}, 400, "Messages per minute must be a whole number"},
		{"shop", viewerPassword, url.Values{"expires": {"2026-11-14"}}, 400, "The expiry must be today or later."},
		{"shop", viewerPassword, url.Values{"expires": {"soon"}}, 400, "not a date"},
	} {
		r := create(c.name, c.current, c.extra)
		if r.StatusCode != c.status || !strings.Contains(strings.ToLower(r.body), strings.ToLower(c.msg)) {
			t.Errorf("create %q %v: %d, want %d %q", c.name, c.extra, r.StatusCode, c.status, c.msg)
		}
		if keyRe.MatchString(r.body) {
			t.Errorf("a refused create shows a key")
		}
	}
	if evs := sh.senderEvents(); len(evs) != 0 {
		t.Fatalf("refused creates wrote %q", evs)
	}
	// Without the CSRF token.
	if r := b.do("POST", "/settings/senders", url.Values{"name": {"shop"}, "current": {viewerPassword}}); r.StatusCode != 403 {
		t.Fatalf("create without csrf: %d", r.StatusCode)
	}

	r := create("shop", viewerPassword, nil)
	if r.StatusCode != 200 {
		t.Fatalf("create: %d\n%s", r.StatusCode, r.body)
	}
	assertHeaders(t, "key page", r)
	key := keyRe.FindString(r.body)
	id, secret, ok := senders.ParseKey(key)
	if !ok {
		t.Fatalf("no key on the result page:\n%s", r.body)
	}
	for _, want := range []string{"shown on this page only and never again", "readonly", ">shop<", "2525", "STARTTLS", id,
		"2027-01-01 00:00:00 UTC"} {
		if !strings.Contains(r.body, want) {
			t.Errorf("key page lacks %q", want)
		}
	}
	// The key logs in, as "shop" only.
	authn := senders.NewAuthenticator(sh.reg, senders.AuthOptions{})
	if p, err := authn.Authenticate("shop", key); err != nil || p.KeyID != id || p.Source != senders.KindDB {
		t.Fatalf("new key: %+v %v", p, err)
	}
	if _, err := authn.Authenticate("app", key); err == nil {
		t.Fatal("the key logs in as another sender")
	}
	snd, _ := sh.reg.ByName("shop")
	if got := snd.Policy.String(); got != "from=*@shop.example.test,noreply@example.test max_per_minute=30" {
		t.Fatalf("policy %q", got)
	}
	if evs := sh.senderEvents(); len(evs) != 1 || evs[0] != "sender.created/shop/viewer/key="+id {
		t.Fatalf("events %q", evs)
	}
	// A second create of the same name (a reload that posts again) gets no key.
	if r := create("shop", viewerPassword, nil); r.StatusCode != 409 || keyRe.MatchString(r.body) {
		t.Fatalf("second create: %d", r.StatusCode)
	}

	// Never again: every page an admin can reach, the log and every event.
	pages := b.crawl("/", "/settings", "/settings/senders/shop", "/mails?sender=shop", "/mails?key="+id)
	if _, ok := pages["/settings/senders/shop"]; !ok || len(pages) < 5 {
		t.Fatalf("crawled %d pages", len(pages))
	}
	for p, body := range pages {
		if strings.Contains(body, secret) || keyRe.MatchString(body) {
			t.Fatalf("%s shows the key", p)
		}
	}
	if strings.Contains(sh.logs.String(), secret) {
		t.Fatal("the key is in the log")
	}
	for _, e := range sh.events() {
		if strings.Contains(fmt.Sprintf("%+v", e), secret) {
			t.Fatalf("the key is in event %s", e.Kind)
		}
	}
	raw, err := os.ReadFile(sh.dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), secret) || strings.Contains(string(raw), key) {
		t.Fatal("the key is in the database file")
	}
}

func TestRotateRevokeAndPolicy(t *testing.T) {
	sh := newSenderHarness(t, true)
	ctx := context.Background()
	first, err := sh.db.Create(ctx, senders.CreateRequest{Name: "shop", Actor: "ops"})
	if err != nil {
		t.Fatal(err)
	}
	b := sh.loggedIn("ops", adminToken)
	page := "/settings/senders/shop"

	r := b.change(page, page+"/rotate", "wrong", nil)
	if r.StatusCode != 403 || !strings.Contains(r.body, "Your current password is wrong.") {
		t.Fatalf("rotate with a wrong password: %d", r.StatusCode)
	}
	r = b.change(page, page+"/rotate", adminToken, nil)
	second := keyRe.FindString(r.body)
	secondID, _, _ := senders.ParseKey(second)
	if r.StatusCode != 200 || second == "" || !strings.Contains(r.body, "Finish the rotation") ||
		!strings.Contains(r.body, "Revoke the old key <span class=\"mono\">"+first.KeyID+"</span>") {
		t.Fatalf("rotate: %d\n%s", r.StatusCode, r.body)
	}
	// Two active keys: no third.
	p := b.get(page)
	if !strings.Contains(p.body, "The sender has two active keys.") || strings.Contains(p.body, `action="/settings/senders/shop/rotate"`) {
		t.Fatal("the page offers a third key")
	}
	if r := b.change(page, page+"/rotate", adminToken, nil); r.StatusCode != 409 || keyRe.MatchString(r.body) {
		t.Fatalf("third key: %d", r.StatusCode)
	}

	// Revoke the old key; a second time, and a key of another sender, fail.
	if r := b.change(page, page+"/keys/"+first.KeyID+"/revoke", adminToken, nil); r.StatusCode != 303 {
		t.Fatalf("revoke: %d %s", r.StatusCode, r.body)
	}
	p = b.get(page)
	if !strings.Contains(p.body, "Key "+first.KeyID+" revoked.") || !strings.Contains(p.body, "by ops") {
		t.Fatalf("after revoke:\n%s", p.body)
	}
	if r := b.change(page, page+"/keys/"+first.KeyID+"/revoke", adminToken, nil); r.StatusCode != 409 {
		t.Fatalf("revoke twice: %d", r.StatusCode)
	}
	if r := b.change(page, page+"/keys/"+sh.fileKeyID+"/revoke", adminToken, nil); r.StatusCode != 404 {
		t.Fatalf("revoke another sender's key: %d", r.StatusCode)
	}

	// The policy: refused input changes nothing; then a valid one.
	if r := b.change(page, page+"/policy", adminToken, url.Values{"from": {"a@b@c"}}); r.StatusCode != 400 ||
		!strings.Contains(r.body, "needs exactly one") || !strings.Contains(r.body, "a@b@c") {
		t.Fatalf("bad policy: %d", r.StatusCode)
	}
	if r := b.change(page, page+"/policy", adminToken, url.Values{"from": {"*@shop.example.test, <>"},
		"max_rcpt_per_hour": {"500"}}); r.StatusCode != 303 {
		t.Fatalf("policy: %d %s", r.StatusCode, r.body)
	}
	snd, _ := sh.reg.ByName("shop")
	if got := snd.Policy.String(); got != "from=*@shop.example.test,<> max_rcpt_per_hour=500" {
		t.Fatalf("policy %q", got)
	}

	// Revoke all: the sender can no longer log in, and stays.
	if r := b.change(page, page+"/revoke-all", "wrong", nil); r.StatusCode != 403 {
		t.Fatalf("revoke all with a wrong password: %d", r.StatusCode)
	}
	if r := b.change(page, page+"/revoke-all", adminToken, nil); r.StatusCode != 303 {
		t.Fatalf("revoke all: %d %s", r.StatusCode, r.body)
	}
	p = b.get(page)
	if !strings.Contains(p.body, "1 key of shop revoked.") || !strings.Contains(p.body, "no active key") ||
		!strings.Contains(p.body, "Every key is revoked.") {
		t.Fatalf("after revoke all:\n%s", p.body)
	}
	if _, err := senders.NewAuthenticator(sh.reg, senders.AuthOptions{}).Authenticate("shop", second); err == nil {
		t.Fatal("a revoked key logs in")
	}
	if r := b.change(page, page+"/revoke-all", adminToken, nil); r.StatusCode != 409 {
		t.Fatalf("revoke all twice: %d", r.StatusCode)
	}
	want := []string{
		"sender.created/shop/ops/key=" + first.KeyID,
		"sender.rotated/shop/ops/key=" + secondID,
		"sender.revoked/shop/ops/key=" + first.KeyID,
		"sender.policy_changed/shop/ops/from_patterns=2 max_per_minute=0 max_rcpt_per_hour=500",
		"sender.revoked/shop/ops/key=" + secondID,
	}
	if got := sh.senderEvents(); strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("events\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	// A rotation switches it on again.
	if r := b.change(page, page+"/rotate", adminToken, url.Values{"expires": {"2026-11-20"}}); r.StatusCode != 200 ||
		!keyRe.MatchString(r.body) || !strings.Contains(r.body, "nothing to revoke") {
		t.Fatalf("rotate after revoke all: %d", r.StatusCode)
	}
	if p := b.get(page); !strings.Contains(p.body, "expires in 5 d 12 h") {
		t.Fatalf("a key that expires soon is not marked:\n%s", p.body)
	}

	// Every change needs the CSRF token.
	for _, path := range []string{page + "/rotate", page + "/policy", page + "/revoke-all", page + "/keys/" + secondID + "/revoke"} {
		if r := b.do("POST", path, url.Values{"current": {adminToken}}); r.StatusCode != 403 {
			t.Errorf("%s without csrf: %d", path, r.StatusCode)
		}
		if r := sh.browser().do("POST", path, url.Values{}); r.StatusCode != 303 || r.Header.Get("Location") != "/login" {
			t.Errorf("%s without a session: %d", path, r.StatusCode)
		}
	}
}

// A sender from the file is read-only: no forms, every change refused,
// nothing written.
func TestFileSenderIsReadOnly(t *testing.T) {
	sh := newSenderHarness(t, true)
	b := sh.loggedIn("ops", adminToken)
	page := "/settings/senders/app"
	p := b.get(page)
	if p.StatusCode != 200 || !strings.Contains(p.body, "Kept in the senders file") ||
		!strings.Contains(p.body, "postwerk gen-sender app") || !strings.Contains(p.body, "app:policy from=*@app.example.test max_per_minute=60") {
		t.Fatalf("file sender page:\n%s", p.body)
	}
	if strings.Contains(p.body, `<form method="post" action="/settings/senders/app`) {
		t.Fatal("the file sender's page has a change form")
	}
	before, _ := sh.reg.ByName("app")
	csrfToken := csrf(t, p.body)
	for _, path := range []string{page + "/rotate", page + "/policy", page + "/revoke-all", page + "/keys/" + sh.fileKeyID + "/revoke"} {
		r := b.do("POST", path, url.Values{"csrf": {csrfToken}, "current": {adminToken}, "from": {"*@x.example.test"}})
		if r.StatusCode != 409 || !strings.Contains(r.body, "is in the senders file and cannot be changed here") || keyRe.MatchString(r.body) {
			t.Errorf("%s: %d", path, r.StatusCode)
		}
	}
	after, _ := sh.reg.ByName("app")
	if !after.Policy.Equal(before.Policy) || len(after.Keys) != 1 || !after.Keys[0].RevokedAt.IsZero() {
		t.Fatalf("the file sender changed: %+v", after)
	}
	if evs := sh.senderEvents(); len(evs) != 0 {
		t.Fatalf("events %q", evs)
	}
}

func TestMailsBySenderAndKey(t *testing.T) {
	sh := newSenderHarness(t, true)
	ctx := context.Background()
	iss, err := sh.db.Create(ctx, senders.CreateRequest{Name: "shop", Actor: "ops"})
	if err != nil {
		t.Fatal(err)
	}
	now := sh.clock.Now()
	for i, m := range []struct{ sender, key string }{{"shop", iss.KeyID}, {"shop", ""}, {"app", sh.fileKeyID}} {
		if err := sh.st.Accept(ctx, store.NewMail{ID: fmt.Sprintf("pw_F%d", i), ReceivedAt: now.Add(-time.Duration(i) * time.Minute),
			Sender: m.sender, Recipients: []string{"r@example.test"}, Raw: []byte{0},
			Deliveries: []store.NewDelivery{{Target: "mailjet", Status: store.Queued}},
			Audit:      store.MailAudit{KeyID: m.key, KeySource: "db", ClientIP: "192.0.2.7"}}); err != nil {
			t.Fatal(err)
		}
	}
	b := sh.loggedIn("ops", adminToken)
	for q, want := range map[string][]string{
		"sender=shop":                        {"pw_F0", "pw_F1"},
		"sender=app":                         {"pw_F2"},
		"key=" + iss.KeyID:                   {"pw_F0"},
		"sender=app&key=" + iss.KeyID:        nil,
		"sender=nobody":                      nil,
		"key=" + sh.fileKeyID + "&state=any": nil,
	} {
		r := b.get("/mails?" + q)
		if q == "key="+sh.fileKeyID+"&state=any" {
			if r.StatusCode != 400 {
				t.Errorf("%s: %d", q, r.StatusCode)
			}
			continue
		}
		if got := mailLinks(r.body); r.StatusCode != 200 || strings.Join(got, ",") != strings.Join(want, ",") {
			t.Errorf("%s: %d %v, want %v", q, r.StatusCode, got, want)
		}
	}
	page := b.get("/mails?sender=shop")
	if !strings.Contains(page.body, `<option value="shop" selected>`) || !strings.Contains(page.body, `<option value="app">`) {
		t.Fatal("the sender filter lacks its choices")
	}
	for _, bad := range []string{"key=short", "key=" + strings.ToUpper(iss.KeyID), "sender=Bad%20Name"} {
		if r := b.get("/mails?" + bad); r.StatusCode != 400 || !strings.Contains(r.body, "The filter is not valid") {
			t.Errorf("%s: %d", bad, r.StatusCode)
		}
	}
}

func TestMailAuditBlock(t *testing.T) {
	sh := newSenderHarness(t, true)
	ctx := context.Background()
	now := sh.clock.Now()
	mails := []store.NewMail{
		{ID: "pw_KEY", Sender: "app", Audit: store.MailAudit{KeyID: sh.fileKeyID, KeySource: "file", ClientIP: "10.1.2.3",
			TLSVersion: "TLS 1.3", TLSCipher: "TLS_AES_128_GCM_SHA256", EHLO: "app-7d9f.example.test", AuthMech: "PLAIN"}},
		{ID: "pw_PW", Sender: "legacy", Audit: store.MailAudit{KeySource: "file", ClientIP: "2001:db8::5", EHLO: "legacy", AuthMech: "PLAIN"}},
		{ID: "pw_OLD", Sender: "app"},
	}
	for _, m := range mails {
		m.ReceivedAt, m.Recipients, m.Raw = now, []string{"r@example.test"}, []byte{0}
		m.Deliveries = []store.NewDelivery{{Target: "mailjet", Status: store.Queued}}
		if err := sh.st.Accept(ctx, m); err != nil {
			t.Fatal(err)
		}
	}
	b := sh.loggedIn("ops", adminToken)
	p := b.get("/mails/pw_KEY").body
	for _, want := range []string{"Sent by", `href="/mails?key=` + sh.fileKeyID + `"`, ">senders file<", "10.1.2.3", "TLS 1.3",
		"TLS_AES_128_GCM_SHA256", "app-7d9f.example.test", "PLAIN", `href="/settings/senders/app"`} {
		if !strings.Contains(p, want) {
			t.Errorf("mail with a key lacks %q", want)
		}
	}
	p = b.get("/mails/pw_PW").body
	for _, want := range []string{"<dd>password", "2001:db8::5", "<dt>TLS</dt><dd><span class=\"muted\">none</span>"} {
		if !strings.Contains(p, want) {
			t.Errorf("mail with a password lacks %q", want)
		}
	}
	if strings.Contains(p, `href="/settings/senders/legacy"`) {
		t.Error("a sender in no source is linked")
	}
	if p := b.get("/mails/pw_OLD").body; !strings.Contains(p, "Not recorded") {
		t.Error("an old mail does not say that its origin was not recorded")
	}
}
