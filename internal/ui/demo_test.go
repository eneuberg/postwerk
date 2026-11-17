package ui

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/eneuberg/postwerk/internal/auth"
	"github.com/eneuberg/postwerk/internal/query"
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
// stopped.
func TestDemo(t *testing.T) {
	addr := os.Getenv("UI_DEMO_ADDR")
	if addr == "" {
		t.Skip("set UI_DEMO_ADDR to serve the demo UI")
	}
	hs := testHasher(t)
	h := newHarnessWithHasher(t, hs, func(o *Options) { o.Now = time.Now; o.Instance = "staging" })
	h.clock.t = time.Now()
	now := time.Now().Truncate(time.Second)
	// The statistics: 30 days of traffic, capture optional, the default
	// retention, and the worker's view of the targets.
	h.ui.opts.Query = query.New(query.Options{Store: h.st, Hasher: hs, Targets: []string{"mailjet", "capture"},
		Optional: []string{"capture"}, RecordRetention: 90 * 24 * time.Hour, EventRetention: 90 * 24 * time.Hour, Now: time.Now,
		Health: func() []query.TargetHealth {
			return []query.TargetHealth{{Target: "mailjet", Since: now.Add(-20 * time.Hour)}, {Target: "capture", Since: now.Add(-30 * 24 * time.Hour)}}
		}})
	began := time.Now()
	seedTraffic(t, h.st, now)
	t.Logf("seeded in %s", time.Since(began).Round(time.Second))
	ok := []store.Status{store.HandedOff}
	senders := []string{"auth-service", "shop", "billing", "shop", "auth-service"}
	for i := range 96 {
		at := now.Add(-time.Duration(96-i)*47*time.Minute - 13*time.Second*time.Duration(i%7))
		h.sender = senders[i%len(senders)]
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
	h.sender = "billing"
	h.seedMail("pw_01KDEMOQUEUED00000000000001", now.Add(-14*time.Minute), []string{"late@example.com"},
		map[string][]store.Status{"mailjet": nil, "capture": ok})
	h.seedMail("pw_01KDEMOQUEUED00000000000002", now.Add(-3*time.Minute), []string{"later@example.com"},
		map[string][]store.Status{"mailjet": nil})
	// Admin actions (D53–D55): an unknown delivery that can be resent or
	// closed, one an admin closed already, and one whose message retention
	// removed (it turned failed; close only).
	h.sender = "auth-service"
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

	ctx := context.Background()
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
