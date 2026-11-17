package ui

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/eneuberg/postwerk/internal/auth"
	"github.com/eneuberg/postwerk/internal/query"
	"github.com/eneuberg/postwerk/internal/senders"
	"github.com/eneuberg/postwerk/internal/store"
)

// TestDemo serves the UI with made-up data for screenshots and design work:
//
//	UI_DEMO_ADDR=127.0.0.1:18081 go test ./internal/ui -run TestDemo -timeout 30m
//
// It listens on that address logged in as "ops" (an account from the
// tokens file), and on the next port as "alice" (an account in the
// database); /login shows the login page, where "viewer" logs in with
// "viewer-password-for-tests". It runs until the test times out or is
// stopped. Senders: "auth-service" in the senders file, "shop" (rotated)
// and "billing" in the database, "newsletter" removed but active.
func TestDemo(t *testing.T) {
	addr := os.Getenv("UI_DEMO_ADDR")
	if addr == "" {
		t.Skip("set UI_DEMO_ADDR to serve the demo UI")
	}
	hs := testHasher(t)
	ctx := context.Background()
	now := time.Now().Truncate(time.Second)
	fileKey, fileKeyID := senders.NewKey()
	line, _ := senders.KeyLine("auth-service", fileKey)
	sendersFile := filepath.Join(t.TempDir(), "senders")
	if err := os.WriteFile(sendersFile, []byte(line+"\nauth-service:policy from=noreply@example.com,<> max_per_minute=120\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	file, err := senders.NewFileSource(senders.FileOptions{Path: sendersFile, AllowEmpty: true})
	if err != nil {
		t.Fatal(err)
	}
	var db *senders.DBSource
	var reg *senders.Registry
	h := newHarnessWithHasher(t, hs, func(o *Options) {
		o.Now, o.Instance, o.SMTPPort = time.Now, "staging", "2525"
		st := o.Accounts.(*store.Store)
		if db, err = senders.NewDBSource(ctx, st, nil); err != nil {
			t.Fatal(err)
		}
		if reg, err = senders.NewRegistry(file, db); err != nil {
			t.Fatal(err)
		}
		o.Senders = db
	})
	h.clock.t = time.Now()
	// The statistics: 30 days of traffic, capture optional, the default
	// retention, and the worker's view of the targets.
	h.ui.opts.Query = query.New(query.Options{Store: h.st, Hasher: hs, Targets: []string{"mailjet", "capture"},
		Optional: []string{"capture"}, RecordRetention: 90 * 24 * time.Hour, EventRetention: 90 * 24 * time.Hour, Now: time.Now,
		Senders: reg, Health: func() []query.TargetHealth {
			return []query.TargetHealth{{Target: "mailjet", Since: now.Add(-20 * time.Hour)}, {Target: "capture", Since: now.Add(-30 * 24 * time.Hour)}}
		}})
	began := time.Now()
	seedTraffic(t, h.st, now)
	t.Logf("seeded in %s", time.Since(began).Round(time.Second))
	shopPolicy, _ := senders.ParsePolicy("from=*@shop.example.com,orders@example.com max_per_minute=60 max_rcpt_per_hour=2000")
	shopOld, err := db.Create(ctx, senders.CreateRequest{Name: "shop", Policy: shopPolicy, Actor: "alice"})
	if err != nil {
		t.Fatal(err)
	}
	shopNew, err := db.Rotate(ctx, "shop", time.Time{}, "ci")
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Revoke(ctx, shopOld.KeyID, "alice"); err != nil {
		t.Fatal(err)
	}
	billing, err := db.Create(ctx, senders.CreateRequest{Name: "billing", ExpiresAt: now.Add(5*24*time.Hour + 3*time.Hour), Actor: "ops"})
	if err != nil {
		t.Fatal(err)
	}
	keyOf := map[string]store.MailAudit{
		"auth-service": {KeyID: fileKeyID, KeySource: "file", ClientIP: "10.42.3.17", TLSVersion: "TLS 1.3",
			TLSCipher: "TLS_AES_128_GCM_SHA256", EHLO: "auth-service-7d9f8c6b5-x2k4p", AuthMech: "PLAIN"},
		"shop": {KeyID: shopNew.KeyID, KeySource: "db", ClientIP: "10.42.1.9", TLSVersion: "TLS 1.3",
			TLSCipher: "TLS_AES_256_GCM_SHA384", EHLO: "shop-web-0", AuthMech: "PLAIN"},
		"billing": {KeyID: billing.KeyID, KeySource: "db", ClientIP: "10.42.2.40", TLSVersion: "TLS 1.2",
			TLSCipher: "TLS_ECDHE_RSA_WITH_AES_128_GCM_SHA256", EHLO: "billing-worker", AuthMech: "PLAIN"},
		"newsletter": {KeySource: "file", ClientIP: "10.42.5.2", EHLO: "newsletter", AuthMech: "PLAIN"},
	}
	ok := []store.Status{store.HandedOff}
	names := []string{"auth-service", "shop", "billing", "shop", "auth-service"}
	for i := range 96 {
		at := now.Add(-time.Duration(96-i)*47*time.Minute - 13*time.Second*time.Duration(i%7))
		h.sender = names[i%len(names)]
		if i == 93 {
			h.sender = "newsletter"
		}
		h.audit = keyOf[h.sender]
		res := map[string][]store.Status{"mailjet": ok}
		if i%3 == 0 {
			res["capture"] = ok
		}
		rcpts := []string{fmt.Sprintf("customer%03d@example.com", i)}
		switch i {
		case 70:
			rcpts = []string{"jane.doe@example.com", "j.smith@example.org", "ops-archive@example.net"}
			res["mailjet"] = []store.Status{store.HandedOff, store.Failed, store.HandedOff}
		case 81:
			res["mailjet"] = []store.Status{store.Failed}
		case 88:
			res["mailjet"] = []store.Status{store.Unknown}
		}
		h.seedMail(fmt.Sprintf("pw_01KDEMO%05d%013d", i, i), at, rcpts, res)
	}
	h.sender, h.audit = "billing", keyOf["billing"]
	h.seedMail("pw_01KDEMOQUEUED00000000000001", now.Add(-14*time.Minute), []string{"late@example.com"},
		map[string][]store.Status{"mailjet": nil, "capture": ok})
	h.seedMail("pw_01KDEMOQUEUED00000000000002", now.Add(-3*time.Minute), []string{"later@example.com"},
		map[string][]store.Status{"mailjet": nil})
	// Admin actions (D53–D55): an unknown delivery that can be resent or
	// closed, one an admin closed already, and one whose message retention
	// removed (it turned failed; close only).
	h.sender, h.audit = "auth-service", keyOf["auth-service"]
	h.seedMail("pw_01KDEMOUNKNOWN000000000001", now.Add(-2*time.Hour), []string{"nina@example.com", "ole@example.org"},
		map[string][]store.Status{"mailjet": {store.Unknown, store.Unknown}, "capture": {store.HandedOff, store.HandedOff}})
	h.seedMail("pw_01KDEMOCLOSED0000000000001", now.Add(-5*time.Hour), []string{"paul@example.com"},
		map[string][]store.Status{"mailjet": {store.Unknown}})
	h.seedMail("pw_01KDEMOEXPIRED000000000001", now.Add(-4*24*time.Hour), []string{"rita@example.com"},
		map[string][]store.Status{"mailjet": {store.Unknown}})
	if _, err := h.st.Retain(context.Background(), store.RetainPolicy{
		RawMaxAge: 7 * 24 * time.Hour, RawKeepAfterFailure: 72 * time.Hour, PseudonymizeAfter: 48 * time.Hour, RecordRetention: 90 * 24 * time.Hour,
		EventRetention: 90 * 24 * time.Hour, Pseudonymize: hs.Hash,
	}, now); err != nil {
		t.Fatal(err)
	}

	// Logins and refusals of the last day.
	if err := h.st.TouchSenderUsage(ctx, []store.SenderUse{
		{Source: "file", Sender: "auth-service", KeyID: fileKeyID, At: now.Add(-2 * time.Minute)},
		{Source: "db", Sender: "shop", KeyID: shopOld.KeyID, At: now.Add(-50 * time.Minute)},
		{Source: "db", Sender: "shop", KeyID: shopNew.KeyID, At: now.Add(-4 * time.Minute)},
		{Source: "db", Sender: "billing", KeyID: billing.KeyID, At: now.Add(-3 * time.Hour)},
	}); err != nil {
		t.Fatal(err)
	}
	for _, e := range []store.Event{
		{Time: now.Add(-40 * time.Minute), Kind: "ingress.auth_failed", Level: store.Warn, Sender: "shop", Text: "failures=3"},
		{Time: now.Add(-30 * time.Minute), Kind: "ingress.rejected", Level: store.Warn, Sender: "shop", Text: "reason=from_not_allowed rejected=1"},
		{Time: now.Add(-20 * time.Minute), Kind: "ingress.rate_limited", Level: store.Warn, Sender: "billing", Text: "limit=max_per_minute limited=12"},
		{Time: now.Add(-10 * time.Minute), Kind: "ingress.auth_failed", Level: store.Warn, Sender: "newsletter", Text: "failures=6"},
	} {
		if err := h.st.Append(ctx, e); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := h.st.CloseDelivery(ctx, store.Close{MailID: "pw_01KDEMOCLOSED0000000000001", Target: "mailjet",
		Resolution: store.Confirmed, Note: "found as sent in the provider's message log", Actor: "alice", Via: "ui",
		At: now.Add(-4 * time.Hour)}); err != nil {
		t.Fatal(err)
	}
	for _, a := range []struct{ name, by string }{{"alice", "ops"}, {"bob.support", "alice"}} {
		hash, err := auth.Hash("demo-password-"+a.name, auth.MinIterations)
		if err != nil {
			t.Fatal(err)
		}
		if err := h.st.CreateUIAccount(ctx, a.name, hash, a.by, now.Add(-72*time.Hour)); err != nil {
			t.Fatal(err)
		}
	}
	hash, _ := auth.Hash("demo-password-bob-new", auth.MinIterations)
	if err := h.st.SetUIPassword(ctx, "bob.support", hash, "alice", now.Add(-26*time.Hour)); err != nil {
		t.Fatal(err)
	}

	passwords := map[string]string{"ops": adminToken, "alice": "demo-password-alice"}
	serve := func(addr string, name string) {
		sess := h.ui.sessions.create(name)
		srv := &http.Server{Addr: addr, Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if _, err := r.Cookie(sessionCookie); err != nil && r.URL.Path != "/login" {
				r.AddCookie(&http.Cookie{Name: sessionCookie, Value: sess.id})
			}
			// A screenshot tool cannot post a form: GET /mails?demo_by=…&demo_q=…
			// becomes the search POST the form would send.
			if q := r.URL.Query(); r.URL.Path == "/mails" && q.Has("demo_q") {
				form := url.Values{"csrf": {sess.csrf}, "by": {q.Get("demo_by")}, "q": {q.Get("demo_q")}}
				r2 := httptest.NewRequest("POST", "/mails", strings.NewReader(form.Encode()))
				r2.Host = r.Host
				r2.Header.Set("Content-Type", "application/x-www-form-urlencoded")
				r2.AddCookie(&http.Cookie{Name: sessionCookie, Value: sess.id})
				r = r2
			}
			// GET /demo-post?path=/settings/senders&name=… posts the other
			// parameters to path with the CSRF token and the account's
			// password, for screenshots of a form's answer.
			if q := r.URL.Query(); r.URL.Path == "/demo-post" {
				form := url.Values{"csrf": {sess.csrf}, "current": {passwords[name]}}
				for k, v := range q {
					if k != "path" {
						form[k] = v
					}
				}
				r2 := httptest.NewRequest("POST", q.Get("path"), strings.NewReader(form.Encode()))
				r2.Host = r.Host
				r2.Header.Set("Content-Type", "application/x-www-form-urlencoded")
				r2.AddCookie(&http.Cookie{Name: sessionCookie, Value: sess.id})
				r = r2
			}
			h.ui.ServeHTTP(w, r)
		})}
		go srv.ListenAndServe()
		t.Cleanup(func() { srv.Close() })
	}
	h.ui.opts.IdleTimeout, h.ui.sessions.idle = 24*time.Hour, 24*time.Hour
	host, port, _ := net.SplitHostPort(addr)
	var p int
	fmt.Sscan(port, &p)
	serve(addr, "ops")
	serve(net.JoinHostPort(host, fmt.Sprint(p+1)), "alice")
	t.Logf("demo UI: http://localhost:%d (ops, tokens file), http://localhost:%d (alice, database)", p, p+1)
	select {}
}
