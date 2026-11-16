package ui

import (
	"net/http"
	"net/url"
	"sync"
	"testing"
	"time"

	"github.com/eneuberg/postwerk/internal/testutil"
)

// Concurrent failed logins must not get past the throttle: no more checks
// run for one name than its free failures plus the one that starts the
// wait, however many requests arrive at once (U22).
func TestLoginThrottleHoldsUnderConcurrency(t *testing.T) {
	const n = 40
	h := newHarness(t, func(o *Options) { o.LoginSlots = n; o.LoginWait = 10 * time.Second })
	h.tokens.block = make(chan struct{})
	forms := make([]url.Values, n)
	browsers := make([]*browser, n)
	for i := range n {
		browsers[i] = h.browser()
		page := browsers[i].get("/login")
		forms[i] = url.Values{"csrf": {csrf(t, page.body)}, "name": {"ops"}, "password": {"wrong"}}
	}
	var wg sync.WaitGroup
	codes := make([]int, n)
	for i := range n {
		wg.Go(func() { codes[i] = browsers[i].do("POST", "/login", forms[i]).StatusCode })
	}
	// Hold the checks that start until every other request has been
	// answered.
	testutil.Eventually(t, 5*time.Second, "checks started", func() bool {
		return int(h.tokens.calls.Load()) >= accountFreeFailures
	})
	time.Sleep(200 * time.Millisecond) // more checks would start now
	close(h.tokens.block)
	wg.Wait()
	if got, limit := int(h.tokens.calls.Load()), accountFreeFailures+1; got > limit {
		t.Fatalf("%d concurrent requests ran %d password checks for one name; at most %d may run", n, got, limit)
	}
	wrong := 0
	for _, c := range codes {
		switch c {
		case http.StatusUnauthorized:
			wrong++
		case http.StatusServiceUnavailable, http.StatusTooManyRequests:
		default:
			t.Fatalf("status %d", c)
		}
	}
	if wrong != int(h.tokens.calls.Load()) {
		t.Fatalf("%d answered as wrong, %d checked", wrong, h.tokens.calls.Load())
	}
	// The next failure starts the wait, as without the burst.
	h.browser().login("ops", "wrong")
	if r := h.browser().login("ops", adminToken); r.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("after the burst: %d", r.StatusCode)
	}
}
