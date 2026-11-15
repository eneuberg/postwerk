package delivery

// End-to-end tests: Postwerk's ingress, workers and reporter run in-process
// against fake SMTP targets built with the same SMTP library. Every test
// sends to a canary address and checks at the end that it never appears in
// logs, exported events, the events table or delivery reply texts (leak
// test, privacy-and-security.md change 14).

import (
	"context"
	"crypto/tls"
	"errors"
	"io"
	"log"
	"net"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/emersion/go-sasl"
	"github.com/emersion/go-smtp"

	"github.com/eneuberg/postwerk/internal/auth"
	"github.com/eneuberg/postwerk/internal/config"
	"github.com/eneuberg/postwerk/internal/ingress"
	"github.com/eneuberg/postwerk/internal/keys"
	"github.com/eneuberg/postwerk/internal/report"
	"github.com/eneuberg/postwerk/internal/seal"
	"github.com/eneuberg/postwerk/internal/store"
	"github.com/eneuberg/postwerk/internal/testutil"
)

const canary = "canary-leak@example.test"

// utf8Canary is an internationalized canary address (RFC 6531). The leak
// check looks for it and for its parts.
const utf8Canary = "kanarienvögel-leck@exämple.test"

const timeout = 10 * time.Second

// ---- fake target ----

type received struct {
	from string
	to   []string
	opts smtp.MailOptions
	data []byte
}

// behavior scripts a fake target. It may change while a test runs.
type behavior struct {
	authFail bool  // reply 535 to every AUTH
	rcptErr  error // reply to every RCPT
	// rcptErrs scripts RCPT per address: one reply per RCPT, in order; a
	// nil entry or an empty list accepts.
	rcptErrs map[string][]error
	dataErrs []error // replies after DATA, one per message; nil entry or empty list: 250
	drop     int     // number of messages after which the connection is dropped without reply
	hold     chan struct{}
	holding  chan struct{} // closed when a message is being held
	hide     []string      // extensions removed from the EHLO reply, such as "SMTPUTF8"
}

type fakeTarget struct {
	t        *testing.T
	addr     string
	user     string
	pass     string
	mu       sync.Mutex
	b        behavior
	received []received
}

type scheme int

const (
	plain scheme = iota
	startTLS
	implicitTLS
)

func newTarget(t *testing.T, sc scheme, cert *testutil.Cert, user, pass string) *fakeTarget {
	t.Helper()
	f := &fakeTarget{t: t, user: user, pass: pass}
	srv := smtp.NewServer(smtp.BackendFunc(func(c *smtp.Conn) (smtp.Session, error) {
		return &fakeSession{f: f, conn: c}, nil
	}))
	srv.Domain = "target.test"
	srv.EnableSMTPUTF8 = true // hidden per test with behavior.hide
	srv.AllowInsecureAuth = sc == plain
	srv.ErrorLog = log.New(io.Discard, "", 0)
	var l net.Listener
	var err error
	l, err = net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	l = hidingListener{Listener: l, f: f}
	switch sc {
	case startTLS:
		srv.TLSConfig = &tls.Config{Certificates: []tls.Certificate{cert.TLS}}
	case implicitTLS:
		l = tls.NewListener(l, &tls.Config{Certificates: []tls.Certificate{cert.TLS}})
	}
	f.addr = l.Addr().String()
	go srv.Serve(l)
	t.Cleanup(func() { srv.Close() })
	return f
}

// hidingListener removes the extensions in behavior.hide from EHLO replies.
// The SMTP library always offers 8BITMIME, so this is the only way to fake a
// target without it.
type hidingListener struct {
	net.Listener
	f *fakeTarget
}

func (l hidingListener) Accept() (net.Conn, error) {
	c, err := l.Listener.Accept()
	if err != nil {
		return nil, err
	}
	return hidingConn{Conn: c, f: l.f}, nil
}

type hidingConn struct {
	net.Conn
	f *fakeTarget
}

func (c hidingConn) Write(b []byte) (int, error) {
	c.f.mu.Lock()
	hide := c.f.b.hide
	c.f.mu.Unlock()
	out := b
	for _, ext := range hide {
		out = []byte(strings.ReplaceAll(string(out), "250-"+ext+"\r\n", ""))
	}
	if _, err := c.Conn.Write(out); err != nil {
		return 0, err
	}
	return len(b), nil
}

func (f *fakeTarget) set(fn func(b *behavior)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	fn(&f.b)
}

func (f *fakeTarget) got() []received {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]received(nil), f.received...)
}

type fakeSession struct {
	f      *fakeTarget
	conn   *smtp.Conn
	authed bool
	from   string
	opts   smtp.MailOptions
	to     []string
}

func (s *fakeSession) AuthMechanisms() []string {
	if s.f.user == "" {
		return nil
	}
	return []string{sasl.Plain}
}

func (s *fakeSession) Auth(string) (sasl.Server, error) {
	return sasl.NewPlainServer(func(_, user, pass string) error {
		s.f.mu.Lock()
		fail := s.f.b.authFail
		s.f.mu.Unlock()
		if fail || user != s.f.user || pass != s.f.pass {
			return &smtp.SMTPError{Code: 535, EnhancedCode: smtp.EnhancedCode{5, 7, 8}, Message: "authentication failed"}
		}
		s.authed = true
		return nil
	}), nil
}

func (s *fakeSession) Mail(from string, opts *smtp.MailOptions) error {
	if s.f.user != "" && !s.authed {
		return &smtp.SMTPError{Code: 530, EnhancedCode: smtp.EnhancedCode{5, 7, 0}, Message: "auth required"}
	}
	s.from = from
	if opts != nil {
		s.opts = *opts
	}
	return nil
}

func (s *fakeSession) Rcpt(to string, _ *smtp.RcptOptions) error {
	s.f.mu.Lock()
	err := s.f.b.rcptErr
	if list := s.f.b.rcptErrs[to]; err == nil && len(list) > 0 {
		err, s.f.b.rcptErrs[to] = list[0], list[1:]
	}
	s.f.mu.Unlock()
	if err != nil {
		return err
	}
	s.to = append(s.to, to)
	return nil
}

func (s *fakeSession) Data(r io.Reader) error {
	data, err := io.ReadAll(r)
	if err != nil {
		return err
	}
	s.f.mu.Lock()
	s.f.received = append(s.f.received, received{from: s.from, to: s.to, opts: s.opts, data: data})
	b := &s.f.b
	hold, holding := b.hold, b.holding
	drop := b.drop > 0
	if drop {
		b.drop--
	}
	var reply error
	if len(b.dataErrs) > 0 {
		reply, b.dataErrs = b.dataErrs[0], b.dataErrs[1:]
	}
	s.f.mu.Unlock()

	if hold != nil {
		close(holding)
		<-hold
	}
	if drop {
		s.conn.Close()
		return nil
	}
	return reply
}

func (s *fakeSession) Reset()        { s.from, s.opts, s.to = "", smtp.MailOptions{}, nil }
func (s *fakeSession) Logout() error { return nil }

func target(name string, f *fakeTarget, sc scheme, mod ...func(*config.Target)) config.Target {
	host, _, _ := net.SplitHostPort(f.addr)
	t := config.Target{
		Name: name, Host: host, Addr: f.addr, RetryWindow: time.Hour, OnUnknown: config.Hold,
		Scheme: map[scheme]config.Scheme{plain: config.SchemePlain, startTLS: config.SchemeSTARTTLS, implicitTLS: config.SchemeSMTPS}[sc],
	}
	if f.user != "" {
		t.Username, t.Password = f.user, f.pass
	}
	for _, m := range mod {
		m(&t)
	}
	return t
}

// ---- Postwerk under test ----

var testSenders = sync.OnceValue(func() *auth.Senders {
	h, err := auth.Hash("secret", auth.MinIterations)
	if err != nil {
		panic(err)
	}
	s, err := auth.ParseSenders(strings.NewReader("app:" + h + "\n"))
	if err != nil {
		panic(err)
	}
	return s
})

type harness struct {
	t       *testing.T
	dbPath  string
	sealer  *seal.Sealer
	store   *store.Store
	mgr     *Manager
	ingress *ingress.Server
	addr    string
	logs    *testutil.SafeBuffer
	events  *testutil.SafeBuffer
	cancel  context.CancelFunc
	stopped bool
	stopFns []func()
}

type harnessOpts struct {
	dbPath   string
	rootCAs  *testutil.Cert
	maxBytes int64
	// sealer for ingress and workers; nil: encryption on with a new key.
	sealer *seal.Sealer
	// workerSealer, if set, replaces sealer for the workers only.
	workerSealer *seal.Sealer
	store        store.Options
}

// newSealer returns a Sealer with one new key.
func newSealer(t *testing.T) *seal.Sealer {
	t.Helper()
	ring, err := keys.Parse([]byte(keys.Generate(time.Now()) + "\n"))
	if err != nil {
		t.Fatal(err)
	}
	s, err := seal.New(ring)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func startPostwerk(t *testing.T, targets []config.Target, ho harnessOpts) *harness {
	t.Helper()
	if ho.dbPath == "" {
		ho.dbPath = filepath.Join(t.TempDir(), "pw.db")
	}
	if ho.maxBytes == 0 {
		ho.maxBytes = 1 << 20
	}
	if ho.sealer == nil {
		ho.sealer = newSealer(t)
	}
	if ho.workerSealer == nil {
		ho.workerSealer = ho.sealer
	}
	h := &harness{t: t, dbPath: ho.dbPath, sealer: ho.sealer, logs: &testutil.SafeBuffer{}, events: &testutil.SafeBuffer{}}
	logger := testutil.Logger(h.logs)

	var rep *report.Reporter
	sopts := ho.store
	sopts.OnEvent = func() { rep.Wake() }
	st, err := store.Open(context.Background(), ho.dbPath, sopts)
	if err != nil {
		t.Fatal(err)
	}
	h.store = st
	rep = report.New(report.Options{Store: st, Sinks: []report.Sink{report.NewStdout(h.events, "test")},
		Interval: 20 * time.Millisecond, Logger: logger})

	opts := Options{
		Store: st, Sealer: ho.workerSealer, Targets: targets, Logger: logger, HeloName: "postwerk.test",
		Backoff:      func(int) time.Duration { return 20 * time.Millisecond },
		PollInterval: 50 * time.Millisecond,
		DialTimeout:  2 * time.Second, CommandTimeout: 2 * time.Second, SubmissionTimeout: 3 * time.Second,
	}
	if ho.rootCAs != nil {
		opts.rootCAs = ho.rootCAs.Pool
	}
	h.mgr = New(opts)

	h.ingress = ingress.New(ingress.Options{
		Store: st, Sealer: ho.sealer, Senders: testSenders(), Targets: targets, Hostname: "postwerk.test",
		AllowInsecure: true, MaxMessageBytes: ho.maxBytes, MaxRecipients: 10,
		Wake: h.mgr.Wake, Logger: logger,
	})
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	h.addr = l.Addr().String()

	ctx, cancel := context.WithCancel(context.Background())
	h.cancel = cancel
	repDone := make(chan struct{})
	go func() { rep.Run(ctx); close(repDone) }()
	go h.ingress.Serve(l)
	h.mgr.Start(ctx)

	h.stopFns = append(h.stopFns, func() {
		sctx, scancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer scancel()
		_ = h.ingress.Shutdown(sctx)
		cancel()
		_ = h.mgr.Stop(sctx)
		<-repDone
		_ = rep.Flush(sctx)
	})
	t.Cleanup(func() {
		h.stop()
		h.checkNoLeak()
		st.Close()
	})
	return h
}

func (h *harness) stop() {
	if h.stopped {
		return
	}
	h.stopped = true
	for _, f := range h.stopFns {
		f()
	}
}

var queuedRe = regexp.MustCompile(`^2\.0\.0 queued as (pw_[0-9A-Z]{26})$`)

// send submits one mail as sender "app" and returns its id.
func (h *harness) send(to []string, msg string) string {
	h.t.Helper()
	id, err := h.trySend(to, msg)
	if err != nil {
		h.t.Fatal(err)
	}
	return id
}

func (h *harness) trySend(to []string, msg string) (string, error) {
	return h.trySendWith(nil, to, msg)
}

// sendWith submits one mail with MAIL parameters.
func (h *harness) sendWith(opts *smtp.MailOptions, to []string, msg string) string {
	h.t.Helper()
	id, err := h.trySendWith(opts, to, msg)
	if err != nil {
		h.t.Fatal(err)
	}
	return id
}

func (h *harness) trySendWith(opts *smtp.MailOptions, to []string, msg string) (string, error) {
	c, err := smtp.Dial(h.addr)
	if err != nil {
		return "", err
	}
	defer c.Close()
	if err := c.Auth(sasl.NewPlainClient("", "app", "secret")); err != nil {
		return "", err
	}
	if err := c.Mail("noreply@example.test", opts); err != nil {
		return "", err
	}
	for _, r := range to {
		if err := c.Rcpt(r, nil); err != nil {
			return "", err
		}
	}
	w, err := c.Data()
	if err != nil {
		return "", err
	}
	if _, err := io.WriteString(w, msg); err != nil {
		return "", err
	}
	resp, err := w.CloseWithResponse()
	if err != nil {
		return "", err
	}
	m := queuedRe.FindStringSubmatch(resp.StatusText)
	if m == nil {
		return "", errors.New("unexpected reply: " + resp.StatusText)
	}
	_ = c.Quit()
	return m[1], nil
}

func (h *harness) delivery(mailID, target string) store.Delivery {
	h.t.Helper()
	ds, err := h.store.Deliveries(context.Background(), mailID)
	if err != nil {
		h.t.Fatal(err)
	}
	for _, d := range ds {
		if d.Target == target {
			return d
		}
	}
	h.t.Fatalf("no delivery %s/%s", mailID, target)
	return store.Delivery{}
}

func (h *harness) waitStatus(mailID, target string, want store.Status) store.Delivery {
	h.t.Helper()
	var d store.Delivery
	testutil.Eventually(h.t, timeout, mailID+"/"+target+" "+string(want), func() bool {
		d = h.delivery(mailID, target)
		return d.Status == want
	})
	return d
}

func (h *harness) eventsOf(mailID string) []store.Event {
	h.t.Helper()
	evs, err := h.store.EventsAfter(context.Background(), 0, 10000)
	if err != nil {
		h.t.Fatal(err)
	}
	var out []store.Event
	for _, e := range evs {
		if e.MailID == mailID {
			out = append(out, e)
		}
	}
	return out
}

func kinds(evs []store.Event, target string) []string {
	var out []string
	for _, e := range evs {
		if target == "" || e.Target == target || e.Target == "" {
			out = append(out, e.Kind)
		}
	}
	return out
}

// checkNoLeak asserts that the canary address never left the recipients
// table and the stored message.
func (h *harness) checkNoLeak() {
	t := h.t
	needles := []string{canary, "canary-leak", utf8Canary, "kanarienvögel", "exämple"}
	check := func(where, s string) {
		for _, n := range needles {
			if strings.Contains(s, n) {
				t.Errorf("canary address found in %s: %q", where, s)
				return
			}
		}
	}
	check("logs", h.logs.String())
	check("exported events", h.events.String())
	if h.store.Ping(context.Background()) != nil {
		return // the store was closed on purpose
	}
	evs, err := h.store.EventsAfter(context.Background(), 0, 100000)
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for _, e := range evs {
		check("events table", e.Text+" "+e.Kind+" "+e.Target+" "+e.Sender)
		if !seen[e.MailID] {
			seen[e.MailID] = true
			ds, _ := h.store.Deliveries(context.Background(), e.MailID)
			for _, d := range ds {
				check("deliveries.last_text", d.LastText)
			}
		}
	}
}

const testMsg = "From: Sender <noreply@example.test>\r\n" +
	"To: <" + canary + ">\r\n" +
	"X-Postwerk-Ref: order-42\r\n" +
	"Subject: Hello\r\n" +
	"Message-ID: <m1@example.test>\r\n" +
	"\r\n" +
	"Line one\r\n" +
	".leading dot\r\n" +
	"\r\n" +
	"last line\r\n"

// testMsg as the target must receive it after the X-Postwerk-Id header.
var deliveredBody = strings.Replace(testMsg, "X-Postwerk-Ref: order-42\r\n", "", 1)

// ---- tests ----

func TestHappyPath(t *testing.T) {
	cert := testutil.NewCert(t)
	mj := newTarget(t, startTLS, &cert, "apikey", "apisecret")
	cap := newTarget(t, implicitTLS, &cert, "", "")
	h := startPostwerk(t, []config.Target{
		target("mailjet", mj, startTLS, func(t *config.Target) { t.Status = config.StatusMailjet; t.OnUnknown = config.Check }),
		target("capture", cap, implicitTLS, func(t *config.Target) { t.Optional = true }),
	}, harnessOpts{rootCAs: &cert})

	mailID := h.send([]string{canary, "second@example.test"}, testMsg)
	h.waitStatus(mailID, "mailjet", store.HandedOff)
	d := h.waitStatus(mailID, "capture", store.HandedOff)
	if d.Attempts != 1 || d.LastCode != 250 || d.DataSent.IsZero() || d.LastText != "2.0.0 OK: queued" {
		t.Fatalf("delivery = %+v", d)
	}

	for name, f := range map[string]*fakeTarget{"mailjet": mj, "capture": cap} {
		got := f.got()
		if len(got) != 1 {
			t.Fatalf("%s received %d messages", name, len(got))
		}
		r := got[0]
		if r.from != "noreply@example.test" || len(r.to) != 2 || r.to[0] != canary {
			t.Fatalf("%s envelope = %q %q", name, r.from, r.to)
		}
		want := "X-Postwerk-Id: " + mailID + "\r\n"
		if name == "mailjet" {
			want += "X-MJ-CustomID: " + mailID + "\r\n"
		}
		want += deliveredBody
		if string(r.data) != want {
			t.Fatalf("%s received:\n%q\nwant\n%q", name, r.data, want)
		}
	}

	// One result event per recipient.
	want := []string{"mail.accepted", "delivery.attempt", "delivery.handed_off", "delivery.handed_off"}
	for _, tg := range []string{"mailjet", "capture"} {
		if got := kinds(h.eventsOf(mailID), tg); strings.Join(got, " ") != strings.Join(want, " ") {
			t.Fatalf("%s events = %q", tg, got)
		}
	}
	testutil.Eventually(t, timeout, "events on stdout", func() bool {
		return strings.Count(h.events.String(), `"kind":"delivery.handed_off"`) == 4
	})
	if !strings.Contains(h.events.String(), `"sender":"app","instance":"test"`) {
		t.Fatalf("stdout events: %s", h.events.String())
	}
}

func TestTargetAuthFailedThenFixed(t *testing.T) {
	cert := testutil.NewCert(t)
	mj := newTarget(t, startTLS, &cert, "apikey", "apisecret")
	mj.set(func(b *behavior) { b.authFail = true })
	h := startPostwerk(t, []config.Target{target("mailjet", mj, startTLS)}, harnessOpts{rootCAs: &cert})

	mailID := h.send([]string{canary}, testMsg)
	testutil.Eventually(t, timeout, "two failed attempts", func() bool {
		return h.delivery(mailID, "mailjet").Attempts >= 2
	})
	d := h.delivery(mailID, "mailjet")
	if d.Status != store.Queued && d.Status != store.Sending {
		t.Fatalf("status = %s, want queued", d.Status)
	}
	evs := h.eventsOf(mailID)
	var authEv *store.Event
	for i := range evs {
		if evs[i].Kind == store.KindTargetAuthFailed {
			authEv = &evs[i]
			break
		}
	}
	if authEv == nil || authEv.Level != store.Error || authEv.Code != 535 || authEv.Text != "5.7.8 authentication failed" {
		t.Fatalf("auth event = %+v", authEv)
	}

	mj.set(func(b *behavior) { b.authFail = false })
	d = h.waitStatus(mailID, "mailjet", store.HandedOff)
	if d.Attempts < 2 || len(mj.got()) != 1 {
		t.Fatalf("delivery = %+v, received %d", d, len(mj.got()))
	}
}

func TestTemporaryRetries(t *testing.T) {
	f := newTarget(t, plain, nil, "", "")
	busy := &smtp.SMTPError{Code: 451, EnhancedCode: smtp.EnhancedCode{4, 3, 0}, Message: "try again later"}
	f.set(func(b *behavior) { b.dataErrs = []error{busy, busy} })
	h := startPostwerk(t, []config.Target{target("mailpit", f, plain)}, harnessOpts{})

	mailID := h.send([]string{canary}, testMsg)
	d := h.waitStatus(mailID, "mailpit", store.HandedOff)
	if d.Attempts != 3 {
		t.Fatalf("attempts = %d", d.Attempts)
	}
	want := "mail.accepted delivery.attempt delivery.retry delivery.attempt delivery.retry delivery.attempt delivery.handed_off"
	if got := strings.Join(kinds(h.eventsOf(mailID), ""), " "); got != want {
		t.Fatalf("events = %s", got)
	}
}

func TestPermanentFailure(t *testing.T) {
	f := newTarget(t, plain, nil, "", "")
	f.set(func(b *behavior) {
		b.rcptErr = &smtp.SMTPError{Code: 550, EnhancedCode: smtp.EnhancedCode{5, 1, 1}, Message: "<" + canary + ">: Recipient address rejected"}
	})
	h := startPostwerk(t, []config.Target{
		target("mailpit", f, plain),
		target("optional", f, plain, func(t *config.Target) { t.Optional = true }),
	}, harnessOpts{})

	mailID := h.send([]string{canary}, testMsg)
	d := h.waitStatus(mailID, "mailpit", store.Failed)
	h.waitStatus(mailID, "optional", store.Failed)
	if d.Attempts != 1 || d.LastCode != 550 || d.LastText != "5.1.1 <redacted>: Recipient address rejected" {
		t.Fatalf("delivery = %+v", d)
	}
	levels := map[string]store.Level{}
	for _, e := range h.eventsOf(mailID) {
		if e.Kind == store.KindDeliveryFailed {
			levels[e.Target] = e.Level
		}
	}
	if levels["mailpit"] != store.Error || levels["optional"] != store.Warn {
		t.Fatalf("levels = %v", levels)
	}
}

func TestBrokenTargetDoesNotDelayOther(t *testing.T) {
	good := newTarget(t, plain, nil, "", "")
	// A port with nothing listening.
	l, _ := net.Listen("tcp", "127.0.0.1:0")
	deadAddr := l.Addr().String()
	l.Close()
	dead := config.Target{Name: "dead", Scheme: config.SchemePlain, Host: "127.0.0.1", Addr: deadAddr,
		RetryWindow: time.Hour, OnUnknown: config.Hold}
	h := startPostwerk(t, []config.Target{dead, target("good", good, plain)}, harnessOpts{})

	start := time.Now()
	ids := []string{h.send([]string{canary}, testMsg), h.send([]string{canary}, testMsg)}
	for _, id := range ids {
		h.waitStatus(id, "good", store.HandedOff)
	}
	if time.Since(start) > 3*time.Second {
		t.Fatalf("good target took %v", time.Since(start))
	}
	testutil.Eventually(t, timeout, "dead target retrying", func() bool {
		return h.delivery(ids[0], "dead").Attempts >= 2
	})
	if s := h.delivery(ids[0], "dead").Status; s != store.Queued && s != store.Sending {
		t.Fatalf("dead status = %s", s)
	}
	var unreachable bool
	for _, e := range h.eventsOf(ids[0]) {
		if e.Kind == store.KindTargetUnreachable && e.Target == "dead" && e.Code == 0 && e.Text != "" {
			unreachable = true
		}
	}
	if !unreachable {
		t.Fatal("no target.unreachable event")
	}
}

func TestDroppedAfterData(t *testing.T) {
	held := newTarget(t, plain, nil, "", "")
	retried := newTarget(t, plain, nil, "", "")
	held.set(func(b *behavior) { b.drop = 1 })
	retried.set(func(b *behavior) { b.drop = 1 })
	h := startPostwerk(t, []config.Target{
		target("held", held, plain),
		target("retried", retried, plain, func(t *config.Target) { t.OnUnknown = config.Retry }),
	}, harnessOpts{})

	mailID := h.send([]string{canary}, testMsg)
	d := h.waitStatus(mailID, "held", store.Unknown)
	if d.Attempts != 1 || d.DataSent.IsZero() {
		t.Fatalf("held = %+v", d)
	}
	d = h.waitStatus(mailID, "retried", store.HandedOff)
	if d.Attempts != 2 {
		t.Fatalf("retried = %+v", d)
	}
	if n := len(retried.got()); n != 2 {
		t.Fatalf("retried target got %d copies, want 2 (policy retry resends)", n)
	}
	if n := len(held.got()); n != 1 {
		t.Fatalf("held target got %d copies", n)
	}
	var unknownEv *store.Event
	for _, e := range h.eventsOf(mailID) {
		if e.Kind == store.KindDeliveryUnknown {
			unknownEv = &e
		}
	}
	if unknownEv == nil || unknownEv.Target != "held" || unknownEv.Level != store.Error ||
		!strings.Contains(unknownEv.Text, "policy hold") {
		t.Fatalf("unknown event = %+v", unknownEv)
	}
	// The held delivery stays unknown.
	time.Sleep(150 * time.Millisecond)
	if s := h.delivery(mailID, "held").Status; s != store.Unknown {
		t.Fatalf("held status changed to %s", s)
	}
}

func TestSkipHeader(t *testing.T) {
	a := newTarget(t, plain, nil, "", "")
	b := newTarget(t, plain, nil, "", "")
	h := startPostwerk(t, []config.Target{target("main", a, plain), target("capture", b, plain)}, harnessOpts{})

	msg := "X-Postwerk-Skip: Capture, nosuch\r\n" + testMsg
	mailID := h.send([]string{canary}, msg)
	h.waitStatus(mailID, "main", store.HandedOff)
	d := h.delivery(mailID, "capture")
	if d.Status != store.Skipped || d.Attempts != 0 {
		t.Fatalf("capture = %+v", d)
	}
	if got := a.got(); len(got) != 1 || strings.Contains(string(got[0].data), "X-Postwerk-Skip") {
		t.Fatalf("main received %+v", got)
	}
	time.Sleep(100 * time.Millisecond)
	if len(b.got()) != 0 {
		t.Fatal("skipped target received the mail")
	}
	if !strings.Contains(h.logs.String(), `"name":"nosuch"`) {
		t.Fatal("unknown skip name not logged")
	}
}

func TestAllowList(t *testing.T) {
	a := newTarget(t, plain, nil, "", "")
	b := newTarget(t, plain, nil, "", "")
	h := startPostwerk(t, []config.Target{
		target("prod", a, plain),
		target("staging", b, plain, func(t *config.Target) { t.AllowRecipients = []string{"qa-*@example.test"} }),
	}, harnessOpts{})

	blocked := h.send([]string{"qa-1@example.test", canary}, testMsg)
	allowed := h.send([]string{"QA-2@example.test"}, testMsg)
	h.waitStatus(blocked, "prod", store.HandedOff)
	h.waitStatus(allowed, "staging", store.HandedOff)
	if d := h.delivery(blocked, "staging"); d.Status != store.Skipped {
		t.Fatalf("staging = %+v", d)
	}
	var ev *store.Event
	for _, e := range h.eventsOf(blocked) {
		if e.Kind == store.KindDeliveryNotAllowed {
			ev = &e
		}
	}
	if ev == nil || ev.Level != store.Warn || ev.Target != "staging" {
		t.Fatalf("not_allowed event = %+v", ev)
	}
	if n := len(b.got()); n != 1 {
		t.Fatalf("staging received %d mails, want 1", n)
	}
}

func TestRecovery(t *testing.T) {
	for _, policy := range []config.OnUnknown{config.Hold, config.Retry} {
		t.Run(string(policy), func(t *testing.T) {
			dbPath := filepath.Join(t.TempDir(), "pw.db")
			ctx := context.Background()
			st, err := store.Open(ctx, dbPath, store.Options{})
			if err != nil {
				t.Fatal(err)
			}
			now := time.Now()
			sealer := newSealer(t)
			for _, id := range []string{"pw_before", "pw_after"} {
				sealed, err := sealer.Seal(id, []byte(testMsg))
				if err != nil {
					t.Fatal(err)
				}
				err = st.Accept(ctx, store.NewMail{
					ID: id, ReceivedAt: now, Sender: "app", EnvelopeFrom: "noreply@example.test",
					Recipients: []string{canary}, Raw: sealed, Size: len(testMsg),
					Deliveries: []store.NewDelivery{{Target: "t", Status: store.Queued}},
				})
				if err != nil {
					t.Fatal(err)
				}
			}
			// Simulate a crash: both claimed, one past data_sent_at.
			c1, _ := st.Claim(ctx, "t", now)
			c2, _ := st.Claim(ctx, "t", now)
			if c1 == nil || c2 == nil {
				t.Fatal("claim failed")
			}
			if err := st.MarkDataSent(ctx, "pw_before", "t", now, 1, nil); err != nil {
				t.Fatal(err)
			}
			afterID, beforeID := "pw_before", "pw_after" // pw_before is claimed first and got data_sent_at
			st.Close()

			f := newTarget(t, plain, nil, "", "")
			h := startPostwerk(t, []config.Target{target("t", f, plain, func(t *config.Target) { t.OnUnknown = policy })},
				harnessOpts{dbPath: dbPath, sealer: sealer})

			d := h.waitStatus(beforeID, "t", store.HandedOff)
			if d.Attempts != 2 {
				t.Fatalf("before data: %+v", d)
			}
			switch policy {
			case config.Hold:
				h.waitStatus(afterID, "t", store.Unknown)
				if n := len(f.got()); n != 1 {
					t.Fatalf("target got %d mails, want 1", n)
				}
			case config.Retry:
				h.waitStatus(afterID, "t", store.HandedOff)
				if n := len(f.got()); n != 2 {
					t.Fatalf("target got %d mails, want 2", n)
				}
			}
		})
	}
}

func TestStorageFailure451(t *testing.T) {
	f := newTarget(t, plain, nil, "", "")
	h := startPostwerk(t, []config.Target{target("t", f, plain)}, harnessOpts{})
	h.store.Close()
	_, err := h.trySend([]string{canary}, testMsg)
	var se *smtp.SMTPError
	if !errors.As(err, &se) || se.Code != 451 || se.Message != "Temporary local problem, try again later" {
		t.Fatalf("err = %v", err)
	}
}

func TestOversized552(t *testing.T) {
	f := newTarget(t, plain, nil, "", "")
	h := startPostwerk(t, []config.Target{target("t", f, plain)}, harnessOpts{maxBytes: 1024})
	_, err := h.trySend([]string{canary}, testMsg+strings.Repeat(strings.Repeat("x", 70)+"\r\n", 30))
	var se *smtp.SMTPError
	if !errors.As(err, &se) || se.Code != 552 {
		t.Fatalf("err = %v", err)
	}
	evs, _ := h.store.EventsAfter(context.Background(), 0, 10)
	if len(evs) != 0 {
		t.Fatalf("events after a rejected mail: %+v", evs)
	}
}

func TestShutdownFinishesSend(t *testing.T) {
	f := newTarget(t, plain, nil, "", "")
	release := make(chan struct{})
	holding := make(chan struct{})
	f.set(func(b *behavior) { b.hold, b.holding = release, holding })
	h := startPostwerk(t, []config.Target{target("t", f, plain)}, harnessOpts{})

	mailID := h.send([]string{canary}, testMsg)
	select {
	case <-holding:
	case <-time.After(timeout):
		t.Fatal("target never received the message")
	}

	stopped := make(chan struct{})
	go func() { h.stop(); close(stopped) }()
	select {
	case <-stopped:
		t.Fatal("shutdown did not wait for the send in progress")
	case <-time.After(200 * time.Millisecond):
	}
	close(release)
	select {
	case <-stopped:
	case <-time.After(timeout):
		t.Fatal("shutdown did not finish")
	}
	if d := h.delivery(mailID, "t"); d.Status != store.HandedOff {
		t.Fatalf("delivery after shutdown = %+v", d)
	}
}

func TestNoAddressLeaks(t *testing.T) {
	// All error paths at once; checkNoLeak runs at cleanup and fails the test
	// if the canary shows up anywhere but recipients and the raw message.
	cert := testutil.NewCert(t)
	rejecting := newTarget(t, plain, nil, "", "")
	rejecting.set(func(b *behavior) {
		b.dataErrs = []error{&smtp.SMTPError{Code: 554, EnhancedCode: smtp.EnhancedCode{5, 7, 1},
			Message: "Message to " + canary + " rejected; RCPT TO:<" + canary + "> blocked"}}
	})
	badLogin := newTarget(t, startTLS, &cert, "u", "p")
	badLogin.set(func(b *behavior) { b.authFail = true })
	dropping := newTarget(t, plain, nil, "", "")
	dropping.set(func(b *behavior) { b.drop = 100 })
	h := startPostwerk(t, []config.Target{
		target("rejecting", rejecting, plain),
		target("badlogin", badLogin, startTLS),
		target("dropping", dropping, plain),
		target("allowlist", rejecting, plain, func(t *config.Target) { t.AllowRecipients = []string{"*@allowed.test"} }),
	}, harnessOpts{rootCAs: &cert})

	mailID := h.send([]string{canary}, testMsg)
	h.waitStatus(mailID, "rejecting", store.Failed)
	h.waitStatus(mailID, "dropping", store.Unknown)
	testutil.Eventually(t, timeout, "auth failure", func() bool { return h.delivery(mailID, "badlogin").Attempts >= 2 })
	if d := h.delivery(mailID, "allowlist"); d.Status != store.Skipped {
		t.Fatalf("allowlist = %+v", d)
	}
	if got := rejecting.got(); len(got) != 1 || got[0].to[0] != canary {
		t.Fatal("canary was not used as recipient")
	}
	h.stop()
	if !strings.Contains(h.events.String(), "<redacted>") {
		t.Fatalf("expected a redacted reply in the exported events: %s", h.events.String())
	}
}

func TestTLSIsEnforced(t *testing.T) {
	cert := testutil.NewCert(t)
	untrusted := newTarget(t, implicitTLS, &cert, "", "")
	noStartTLS := newTarget(t, plain, nil, "", "") // offers no STARTTLS
	// No rootCAs: the self-signed certificate is not trusted.
	h := startPostwerk(t, []config.Target{
		target("untrusted", untrusted, implicitTLS),
		target("nostarttls", noStartTLS, startTLS),
	}, harnessOpts{})

	mailID := h.send([]string{canary}, testMsg)
	for _, name := range []string{"untrusted", "nostarttls"} {
		testutil.Eventually(t, timeout, name+" retrying", func() bool { return h.delivery(mailID, name).Attempts >= 2 })
	}
	if len(untrusted.got()) != 0 || len(noStartTLS.got()) != 0 {
		t.Fatal("a message went out without verified TLS")
	}
	texts := map[string]string{}
	for _, e := range h.eventsOf(mailID) {
		if e.Kind == store.KindTargetUnreachable {
			texts[e.Target] = e.Text
		}
	}
	if !strings.Contains(texts["untrusted"], "certificate") || !strings.Contains(texts["nostarttls"], "STARTTLS") {
		t.Fatalf("unreachable texts = %q", texts)
	}
}
