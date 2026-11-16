package report

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/eneuberg/postwerk/internal/config"
	"github.com/eneuberg/postwerk/internal/store"
	"github.com/eneuberg/postwerk/internal/testutil"
)

const leakCanary = "canary-leak@example.test"

// fakeSentry records envelopes and answers as scripted.
type fakeSentry struct {
	t      *testing.T
	srv    *httptest.Server
	mu     sync.Mutex
	bodies [][]byte
	auth   []string
	reply  func(w http.ResponseWriter) // nil: 200
}

func newFakeSentry(t *testing.T) *fakeSentry {
	f := &fakeSentry{t: t}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		f.mu.Lock()
		defer f.mu.Unlock()
		if r.Method != http.MethodPost || r.URL.Path != "/api/9/envelope/" ||
			r.Header.Get("Content-Type") != "application/x-sentry-envelope" {
			t.Errorf("request %s %s %q", r.Method, r.URL.Path, r.Header.Get("Content-Type"))
		}
		f.bodies = append(f.bodies, b)
		f.auth = append(f.auth, r.Header.Get("X-Sentry-Auth"))
		if f.reply != nil {
			f.reply(w)
			return
		}
		w.Write([]byte(`{"id":"x"}`))
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeSentry) dsn(t *testing.T) config.SentryDSN {
	d, err := config.ParseSentryDSN("http://pubkey@"+strings.TrimPrefix(f.srv.URL, "http://")+"/9", true)
	if err != nil {
		t.Fatal(err)
	}
	return d
}

func (f *fakeSentry) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.bodies)
}

func (f *fakeSentry) last() []byte {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.bodies[len(f.bodies)-1]
}

func (f *fakeSentry) setReply(r func(w http.ResponseWriter)) {
	f.mu.Lock()
	f.reply = r
	f.mu.Unlock()
}

// clock is a settable time source.
type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) Now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *clock) add(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

type envelope struct {
	header map[string]any
	item   map[string]any
	event  map[string]any
}

func parseEnvelope(t *testing.T, b []byte) envelope {
	t.Helper()
	lines := bytes.SplitN(b, []byte("\n"), 3)
	if len(lines) != 3 {
		t.Fatalf("envelope %q", b)
	}
	var env envelope
	if err := json.Unmarshal(lines[0], &env.header); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(lines[1], &env.item); err != nil {
		t.Fatal(err)
	}
	n := int(env.item["length"].(float64))
	if len(lines[2]) != n+1 || lines[2][n] != '\n' {
		t.Fatalf("payload length %d, header says %d", len(lines[2]), n)
	}
	if err := json.Unmarshal(lines[2][:n], &env.event); err != nil {
		t.Fatal(err)
	}
	return env
}

func failedEvent() store.Event {
	return store.Event{
		Seq: 1042, Time: time.UnixMilli(1_763_026_867_312), Level: store.Error, Kind: "delivery.failed",
		MailID: "pw_01JCX4ABCDEFGHJKMNPQRSTVWX", Target: "mailjet", Recipient: store.Position(1), Attempt: 3,
		Code: 550, Text: "5.1.1 <" + leakCanary + ">: unknown user " + leakCanary, Sender: "auth-service",
	}
}

func TestSentryEnvelope(t *testing.T) {
	f := newFakeSentry(t)
	clk := &clock{t: time.UnixMilli(1_763_026_900_000)}
	s := NewSentry(SentryOptions{DSN: f.dsn(t), Instance: "prod", Version: "1.2.3", Now: clk.Now})
	e := failedEvent()
	if err := s.Export(ctx, e); err != nil {
		t.Fatal(err)
	}
	body := f.last()
	// Leak test: the address in the reply never leaves the system.
	for _, n := range []string{leakCanary, "canary-leak", "@example.test"} {
		if bytes.Contains(body, []byte(n)) {
			t.Fatalf("%q in body: %s", n, body)
		}
	}
	if bytes.Contains(body, []byte("pubkey")) {
		t.Fatal("key in body")
	}
	if got := f.auth[0]; got != "Sentry sentry_version=7, sentry_client=postwerk/1.2.3, sentry_key=pubkey" {
		t.Fatalf("auth = %q", got)
	}
	env := parseEnvelope(t, body)
	id := EventID("prod", e)
	if len(id) != 32 || id[12] != '8' {
		t.Fatalf("event id %q", id)
	}
	if env.header["event_id"] != id || env.event["event_id"] != id || env.header["sent_at"] != "2025-11-13T09:41:40.000Z" {
		t.Fatalf("header %v", env.header)
	}
	if env.item["type"] != "event" || env.item["content_type"] != "application/json" {
		t.Fatalf("item %v", env.item)
	}
	ev := env.event
	want := map[string]any{
		"timestamp": "2025-11-13T09:41:07.312Z", "platform": "other", "level": "error", "logger": "postwerk",
		"environment": "prod",
	}
	for k, v := range want {
		if ev[k] != v {
			t.Fatalf("%s = %v, want %v", k, ev[k], v)
		}
	}
	if m := ev["logentry"].(map[string]any)["formatted"]; m != "delivery.failed at mailjet: 550 5.1.1 <redacted>: unknown user <redacted>" {
		t.Fatalf("message %q", m)
	}
	if fp, _ := json.Marshal(ev["fingerprint"]); string(fp) != `["postwerk","delivery.failed","mailjet"]` {
		t.Fatalf("fingerprint %s", fp)
	}
	tags, _ := json.Marshal(ev["tags"])
	if string(tags) != `{"instance":"prod","kind":"delivery.failed","level":"error","mail_id":"pw_01JCX4ABCDEFGHJKMNPQRSTVWX","sender":"auth-service","target":"mailjet"}` {
		t.Fatalf("tags %s", tags)
	}
	extra, _ := marshal(ev["extra"])
	if string(extra) != `{"attempt":3,"recipient":1,"seq":1042,"smtp_code":550,"smtp_text":"5.1.1 <redacted>: unknown user <redacted>","ui_path":"/mails/pw_01JCX4ABCDEFGHJKMNPQRSTVWX"}` {
		t.Fatalf("extra %s", extra)
	}
	for _, k := range []string{"user", "request", "breadcrumbs", "server_name", "contexts", "exception"} {
		if _, ok := ev[k]; ok {
			t.Fatalf("event has %s", k)
		}
	}

	// The same event again gets the same id, so the receiver drops it.
	if err := s.Export(ctx, e); err != nil {
		t.Fatal(err)
	}
	if parseEnvelope(t, f.last()).event["event_id"] != id {
		t.Fatal("id not deterministic")
	}
	e2 := e
	e2.Seq++
	if EventID("prod", e2) == id || EventID("staging", e) == id {
		t.Fatal("ids collide")
	}
	e3 := e
	e3.Time = e.Time.Add(time.Millisecond) // a new database starting at the same seq
	if EventID("prod", e3) == id {
		t.Fatal("ids collide across databases")
	}

	// A text without a reply code is no SMTP reply: "text", no smtp_*.
	tlsEvent := store.Event{Seq: 7, Time: e.Time, Kind: "tls.expiring", Level: store.Error,
		Text: "smtp: the served certificate expires in 6 days"}
	if err := s.Export(ctx, tlsEvent); err != nil {
		t.Fatal(err)
	}
	extra, _ = marshal(parseEnvelope(t, f.last()).event["extra"])
	if string(extra) != `{"seq":7,"text":"smtp: the served certificate expires in 6 days"}` {
		t.Fatalf("extra of a non-SMTP event %s", extra)
	}
}

func TestSentryLevels(t *testing.T) {
	f := newFakeSentry(t)
	errOnly := NewSentry(SentryOptions{DSN: f.dsn(t)})
	warn := NewSentry(SentryOptions{DSN: f.dsn(t), MinLevel: store.Warn})
	ev := func(l store.Level) store.Event {
		return store.Event{Seq: 1, Time: time.Now(), Level: l, Kind: "delivery.retry", Target: "t"}
	}
	for _, l := range []store.Level{store.Info, store.Warn} {
		if err := errOnly.Export(ctx, ev(l)); err != nil {
			t.Fatal(err)
		}
	}
	if err := warn.Export(ctx, ev(store.Info)); err != nil {
		t.Fatal(err)
	}
	if f.count() != 0 {
		t.Fatalf("%d requests for events below the level", f.count())
	}
	if err := warn.Export(ctx, ev(store.Warn)); err != nil {
		t.Fatal(err)
	}
	env := parseEnvelope(t, f.last())
	if env.event["level"] != "warning" || env.event["tags"].(map[string]any)["level"] != "warn" {
		t.Fatalf("event %v", env.event)
	}
	if _, ok := env.event["environment"]; ok {
		t.Fatal("environment without an instance")
	}

	// A UI event names its actor, not an SMTP reply.
	ui := store.Event{Seq: 2, Time: time.Now(), Level: store.Warn, Kind: "ui.login_failed", Actor: "ops"}
	if err := warn.Export(ctx, ui); err != nil {
		t.Fatal(err)
	}
	env = parseEnvelope(t, f.last())
	if m := env.event["logentry"].(map[string]any)["formatted"]; m != "ui.login_failed by ops" {
		t.Fatalf("message %q", m)
	}
	if fp, _ := json.Marshal(env.event["fingerprint"]); string(fp) != `["postwerk","ui.login_failed"]` {
		t.Fatalf("fingerprint %s", fp)
	}
}

func TestSentryRateLimit(t *testing.T) {
	f := newFakeSentry(t)
	clk := &clock{t: time.Unix(1_800_000_000, 0)}
	var logs testutil.SafeBuffer
	s := NewSentry(SentryOptions{DSN: f.dsn(t), Now: clk.Now, Logger: testutil.Logger(&logs)})
	e := failedEvent()

	f.setReply(func(w http.ResponseWriter) {
		w.Header().Set("Retry-After", "120")
		w.WriteHeader(http.StatusTooManyRequests)
	})
	if err := s.Export(ctx, e); !errors.Is(err, ErrDeferred) {
		t.Fatalf("429: %v", err)
	}
	f.setReply(nil)
	clk.add(119 * time.Second)
	if err := s.Export(ctx, e); !errors.Is(err, ErrDeferred) || f.count() != 1 {
		t.Fatalf("during Retry-After: %v, %d requests", err, f.count())
	}
	clk.add(time.Second)
	if err := s.Export(ctx, e); err != nil || f.count() != 2 {
		t.Fatalf("after Retry-After: %v, %d requests", err, f.count())
	}

	// X-Sentry-Rate-Limits wins over Retry-After and counts on a 200 too.
	f.setReply(func(w http.ResponseWriter) {
		w.Header().Set("X-Sentry-Rate-Limits", "600:transaction:org, 30:error;default:project")
		w.WriteHeader(http.StatusOK)
	})
	if err := s.Export(ctx, e); err != nil {
		t.Fatal(err)
	}
	f.setReply(nil)
	clk.add(29 * time.Second)
	if err := s.Export(ctx, e); !errors.Is(err, ErrDeferred) || f.count() != 3 {
		t.Fatalf("during rate limit: %v, %d requests", err, f.count())
	}
	clk.add(time.Second)
	if err := s.Export(ctx, e); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(logs.String(), "pubkey") {
		t.Fatal("key in logs")
	}
}

func TestRateLimitHeader(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	h := func(kv ...string) http.Header {
		x := http.Header{}
		for i := 0; i < len(kv); i += 2 {
			x.Set(kv[i], kv[i+1])
		}
		return x
	}
	cases := []struct {
		h      http.Header
		status int
		wait   time.Duration
		found  bool
	}{
		{h(), 200, 0, false},
		{h(), 429, 60 * time.Second, true},
		{h("Retry-After", "7"), 429, 7 * time.Second, true},
		{h("Retry-After", now.Add(90*time.Second).UTC().Format(http.TimeFormat)), 429, 90 * time.Second, true},
		{h("Retry-After", "7"), 503, 0, false},
		{h("X-Sentry-Rate-Limits", "60:transaction:key"), 429, 0, false},
		{h("X-Sentry-Rate-Limits", "60::organization"), 200, 60 * time.Second, true},
		{h("X-Sentry-Rate-Limits", " 10 : error ; transaction : key , 20:default:org"), 200, 20 * time.Second, true},
		{h("X-Sentry-Rate-Limits", "99999:error"), 200, time.Hour, true},
		{h("X-Sentry-Rate-Limits", "x:error, -1:error"), 429, 0, false},
		{h("X-Sentry-Rate-Limits", "2.5:error:key:reason:ns"), 200, 2500 * time.Millisecond, true},
	}
	for i, c := range cases {
		wait, found := rateLimit(c.h, c.status, now)
		if wait != c.wait || found != c.found {
			t.Errorf("case %d: %s, %v; want %s, %v", i, wait, found, c.wait, c.found)
		}
	}
}

func TestSentryFailures(t *testing.T) {
	f := newFakeSentry(t)
	clk := &clock{t: time.Unix(1_800_000_000, 0)}
	var logs testutil.SafeBuffer
	s := NewSentry(SentryOptions{DSN: f.dsn(t), Now: clk.Now, Logger: testutil.Logger(&logs)})
	e := failedEvent()

	// 5xx: back off, doubling.
	f.setReply(func(w http.ResponseWriter) { w.WriteHeader(http.StatusBadGateway) })
	for i, wait := range []time.Duration{5 * time.Second, 10 * time.Second, 20 * time.Second} {
		if err := s.Export(ctx, e); !errors.Is(err, ErrDeferred) || f.count() != i+1 {
			t.Fatalf("5xx %d: %v, %d requests", i, err, f.count())
		}
		clk.add(wait - time.Millisecond)
		if err := s.Export(ctx, e); !errors.Is(err, ErrDeferred) || f.count() != i+1 {
			t.Fatalf("backoff %d: request too early", i)
		}
		clk.add(time.Millisecond)
	}
	if n := strings.Count(logs.String(), "sentry export failing"); n != 1 {
		t.Fatalf("failure logged %d times:\n%s", n, logs.String())
	}

	// A wrong key: kept and retried, logged as an error.
	f.setReply(func(w http.ResponseWriter) { w.WriteHeader(http.StatusForbidden) })
	s2 := NewSentry(SentryOptions{DSN: f.dsn(t), Now: clk.Now, Logger: testutil.Logger(&logs)})
	if err := s2.Export(ctx, e); !errors.Is(err, ErrDeferred) || !strings.Contains(logs.String(), "refused the DSN") {
		t.Fatalf("403: %v", err)
	}

	// Recovery is logged once and the backoff is reset.
	f.setReply(nil)
	clk.add(time.Hour)
	if err := s.Export(ctx, e); err != nil || !strings.Contains(logs.String(), "works again") {
		t.Fatalf("recovery: %v", err)
	}

	// 400: this event is refused for good; skip it.
	f.setReply(func(w http.ResponseWriter) {
		w.Header().Set("X-Sentry-Error", "invalid event for "+leakCanary)
		w.WriteHeader(http.StatusBadRequest)
	})
	if err := s.Export(ctx, e); err != nil {
		t.Fatalf("400: %v", err)
	}
	if !strings.Contains(logs.String(), "refused an event") || strings.Contains(logs.String(), leakCanary) {
		t.Fatalf("logs:\n%s", logs.String())
	}

	// A redirect is not followed: it would carry the key elsewhere.
	other := newFakeSentry(t)
	f.setReply(func(w http.ResponseWriter) {
		w.Header().Set("Location", other.srv.URL+"/api/9/envelope/")
		w.WriteHeader(http.StatusTemporaryRedirect)
	})
	clk.add(time.Hour)
	if err := s.Export(ctx, e); !errors.Is(err, ErrDeferred) || other.count() != 0 {
		t.Fatalf("redirect: %v, %d", err, other.count())
	}

	// Unreachable: deferred, never blocks for long.
	f.srv.Close()
	clk.add(time.Hour)
	start := time.Now()
	if err := s.Export(ctx, e); !errors.Is(err, ErrDeferred) {
		t.Fatalf("down: %v", err)
	}
	if time.Since(start) > 5*time.Second {
		t.Fatal("export to a closed port took long")
	}
}

func TestSentryTimeout(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { <-release }))
	defer srv.Close()
	defer close(release)
	d, _ := config.ParseSentryDSN("http://k@"+strings.TrimPrefix(srv.URL, "http://")+"/9", true)
	s := NewSentry(SentryOptions{DSN: d, Client: &http.Client{Timeout: 100 * time.Millisecond}})
	err := s.Export(ctx, failedEvent())
	if !errors.Is(err, ErrDeferred) || !strings.Contains(err.Error(), "timeout") {
		t.Fatalf("err = %v", err)
	}
}

func TestSentryWithReporter(t *testing.T) {
	s := openStore(t)
	f := newFakeSentry(t)
	clk := &clock{t: time.Unix(1_800_000_000, 0)}
	sentry := NewSentry(SentryOptions{DSN: f.dsn(t), Now: clk.Now})
	var out bytes.Buffer
	r := New(Options{Store: s, Sinks: []Sink{sentry, NewStdout(&out, "")}})
	at := time.UnixMilli(1_763_000_000_000)
	appendEv := func(l store.Level, kind string) {
		t.Helper()
		if err := s.Append(ctx, store.Event{Time: at, Kind: kind, Level: l, Target: "t", Code: 550, Text: "5.1.1 no"}); err != nil {
			t.Fatal(err)
		}
	}
	appendEv(store.Info, "delivery.attempt")
	appendEv(store.Warn, "delivery.retry")
	appendEv(store.Error, "delivery.failed")
	appendEv(store.Info, "delivery.attempt")

	f.setReply(func(w http.ResponseWriter) { w.WriteHeader(http.StatusServiceUnavailable) })
	if err := r.Flush(ctx); !errors.Is(err, ErrDeferred) {
		t.Fatalf("flush: %v", err)
	}
	// Events below the level advanced the cursor; the error event waits.
	if c, _ := s.Cursor(ctx, "sentry"); c != 2 {
		t.Fatalf("sentry cursor = %d, want 2", c)
	}
	if c, _ := s.Cursor(ctx, "stdout"); c != 4 {
		t.Fatalf("stdout cursor = %d", c)
	}

	f.setReply(nil)
	clk.add(time.Hour)
	if err := r.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	if c, _ := s.Cursor(ctx, "sentry"); c != 4 {
		t.Fatalf("sentry cursor = %d, want 4", c)
	}
	if f.count() != 2 { // one refused, one sent
		t.Fatalf("%d requests", f.count())
	}
	if k := parseEnvelope(t, f.last()).event["tags"].(map[string]any)["kind"]; k != "delivery.failed" {
		t.Fatalf("kind %v", k)
	}
}
