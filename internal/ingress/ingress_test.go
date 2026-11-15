package ingress

import (
	"context"
	"crypto/tls"
	"errors"
	"net"
	"net/textproto"
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
	"github.com/eneuberg/postwerk/internal/id"
	"github.com/eneuberg/postwerk/internal/keys"
	"github.com/eneuberg/postwerk/internal/seal"
	"github.com/eneuberg/postwerk/internal/store"
	"github.com/eneuberg/postwerk/internal/testutil"
)

var senders = sync.OnceValue(func() *auth.Senders {
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

type env struct {
	srv   *Server
	store *store.Store
	addr  string
	logs  *testutil.SafeBuffer
	woken chan struct{}
}

func start(t *testing.T, mod func(*Options)) *env {
	t.Helper()
	st, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "pw.db"), store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	e := &env{store: st, logs: &testutil.SafeBuffer{}, woken: make(chan struct{}, 100)}
	opts := Options{
		Store: st, Sealer: seal.Off(), Senders: senders(), Hostname: "postwerk.test",
		Targets:         []config.Target{{Name: "a"}, {Name: "b", AllowRecipients: []string{"*@example.test"}}},
		AllowInsecure:   true,
		MaxMessageBytes: 4096, MaxRecipients: 3,
		Wake:   func() { e.woken <- struct{}{} },
		Logger: testutil.Logger(e.logs),
	}
	if mod != nil {
		mod(&opts)
	}
	srv := New(opts)
	e.srv = srv
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	e.addr = l.Addr().String()
	go srv.Serve(l)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		srv.Shutdown(ctx)
	})
	return e
}

func dial(t *testing.T, addr string) *smtp.Client {
	t.Helper()
	c, err := smtp.Dial(addr)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	return c
}

func code(err error) int {
	var se *smtp.SMTPError
	if errors.As(err, &se) {
		return se.Code
	}
	return -1
}

const msg = "From: a@example.test\r\nX-Postwerk-Ref: ref-1\r\nSubject: hi\r\n\r\nbody\r\n"

func sendMsg(c *smtp.Client, from string, to []string, body string) (string, error) {
	if err := c.Mail(from, nil); err != nil {
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
	if _, err := w.Write([]byte(body)); err != nil {
		return "", err
	}
	resp, err := w.CloseWithResponse()
	if err != nil {
		return "", err
	}
	return resp.StatusText, nil
}

var queuedRe = regexp.MustCompile(`^2\.0\.0 queued as (pw_[0-9A-Z]{26})$`)

func TestQueuedReply(t *testing.T) {
	e := start(t, nil)
	c := dial(t, e.addr)
	if err := c.Auth(sasl.NewPlainClient("", "app", "secret")); err != nil {
		t.Fatal(err)
	}
	text, err := sendMsg(c, "", []string{"x@example.test", "y@other.test"}, msg)
	if err != nil {
		t.Fatal(err)
	}
	m := queuedRe.FindStringSubmatch(text)
	if m == nil || !id.Valid(m[1]) {
		t.Fatalf("reply = %q", text)
	}
	select {
	case <-e.woken:
	case <-time.After(time.Second):
		t.Fatal("workers not woken")
	}
	ds, err := e.store.Deliveries(context.Background(), m[1])
	if err != nil {
		t.Fatal(err)
	}
	if len(ds) != 2 || ds[0].Status != store.Queued || ds[1].Status != store.Skipped {
		t.Fatalf("deliveries = %+v", ds)
	}

	// A second mail on the same connection works without a new AUTH.
	if _, err := sendMsg(c, "b@example.test", []string{"x@example.test"}, msg); err != nil {
		t.Fatal(err)
	}
}

func TestAuthRequired(t *testing.T) {
	e := start(t, nil)
	c := dial(t, e.addr)
	if err := c.Mail("a@example.test", nil); code(err) != 530 {
		t.Fatalf("MAIL before AUTH: %v", err)
	}
	if err := c.Rcpt("a@example.test", nil); code(err) == 250 || err == nil {
		t.Fatalf("RCPT before AUTH accepted")
	}
}

func TestAuthFailuresClose(t *testing.T) {
	e := start(t, nil)
	c := dial(t, e.addr)
	for i := 1; i <= 3; i++ {
		err := c.Auth(sasl.NewPlainClient("", "app", "wrong"))
		want := 535
		if i == 3 {
			want = 421
		}
		if code(err) != want {
			t.Fatalf("attempt %d: %v", i, err)
		}
	}
	if err := c.Noop(); err == nil || code(err) != -1 {
		t.Fatalf("connection still open after 3 failures: %v", err)
	}
	if strings.Contains(e.logs.String(), "wrong") {
		t.Fatal("password in logs")
	}
}

func TestAuthIdentityAndUnknown(t *testing.T) {
	e := start(t, nil)
	c := dial(t, e.addr)
	if err := c.Auth(sasl.NewPlainClient("other", "app", "secret")); code(err) != 535 {
		t.Fatalf("identity mismatch: %v", err)
	}
	if err := c.Auth(sasl.NewPlainClient("", "nobody", "secret")); code(err) != 535 {
		t.Fatalf("unknown user: %v", err)
	}
	if err := c.Auth(sasl.NewPlainClient("app", "app", "secret")); err != nil {
		t.Fatalf("identity equal to user: %v", err)
	}
}

func TestMalformedAuthIsFixedReply(t *testing.T) {
	e := start(t, nil)
	conn, err := textproto.Dial("tcp", e.addr)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if _, _, err := conn.ReadResponse(220); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Cmd("EHLO x"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := conn.ReadResponse(250); err != nil {
		t.Fatal(err)
	}
	conn.Cmd("AUTH PLAIN bm9udWxscw==") // "nonulls": no NUL separators
	code, text, _ := conn.ReadResponse(0)
	if code != 535 || text != "5.7.8 Authentication failed" {
		t.Fatalf("reply = %d %q", code, text)
	}
}

func TestTLSRequiredForAuth(t *testing.T) {
	cert := testutil.NewCert(t)
	e := start(t, func(o *Options) {
		o.TLSCertificate = &cert.TLS
		o.AllowInsecure = false
	})
	c := dial(t, e.addr)
	if ok, _ := c.Extension("AUTH"); ok {
		t.Fatal("AUTH offered before STARTTLS")
	}
	if err := c.Auth(sasl.NewPlainClient("", "app", "secret")); err == nil {
		t.Fatal("AUTH accepted before STARTTLS")
	}

	tc, err := smtp.DialStartTLS(e.addr, &tls.Config{RootCAs: cert.Pool, ServerName: "127.0.0.1"})
	if err != nil {
		t.Fatal(err)
	}
	defer tc.Close()
	if err := tc.Auth(sasl.NewPlainClient("", "app", "secret")); err != nil {
		t.Fatalf("AUTH after STARTTLS: %v", err)
	}
	if _, err := sendMsg(tc, "a@example.test", []string{"b@example.test"}, msg); err != nil {
		t.Fatal(err)
	}
}

func TestLimitsAndAddresses(t *testing.T) {
	e := start(t, nil)
	c := dial(t, e.addr)
	if err := c.Auth(sasl.NewPlainClient("", "app", "secret")); err != nil {
		t.Fatal(err)
	}
	long := strings.Repeat("a", 320) + "@example.test"
	if err := c.Mail(long, nil); code(err) != 553 {
		t.Fatalf("long sender: %v", err)
	}
	if err := c.Mail("a@example.test", nil); err != nil {
		t.Fatal(err)
	}
	if err := c.Rcpt(long, nil); code(err) != 553 {
		t.Fatalf("long recipient: %v", err)
	}
	for i := range 3 {
		if err := c.Rcpt(string(rune('a'+i))+"@example.test", nil); err != nil {
			t.Fatal(err)
		}
	}
	if err := c.Rcpt("d@example.test", nil); code(err) != 452 {
		t.Fatalf("fourth recipient: %v", err)
	}
	if err := c.Reset(); err != nil {
		t.Fatal(err)
	}

	// Oversized message: 552 and nothing stored.
	big := "Subject: big\r\n\r\n" + strings.Repeat(strings.Repeat("x", 76)+"\r\n", 100)
	if _, err := sendMsg(c, "a@example.test", []string{"b@example.test"}, big); code(err) != 552 {
		t.Fatalf("oversized: %v", err)
	}
	if evs, _ := e.store.EventsAfter(context.Background(), 0, 10); len(evs) != 0 {
		t.Fatalf("oversized mail left events: %+v", evs)
	}
	// The connection is still usable afterwards.
	if _, err := sendMsg(c, "a@example.test", []string{"b@example.test"}, msg); err != nil {
		t.Fatalf("after oversized: %v", err)
	}

	// A line longer than the limit is refused with a fixed reply.
	long2 := "Subject: x\r\n\r\n" + strings.Repeat("y", 2500) + "\r\n"
	e2 := start(t, func(o *Options) { o.MaxMessageBytes = 1 << 20 })
	c2 := dial(t, e2.addr)
	if err := c2.Auth(sasl.NewPlainClient("", "app", "secret")); err != nil {
		t.Fatal(err)
	}
	if _, err := sendMsg(c2, "a@example.test", []string{"b@example.test"}, long2); code(err) != 500 {
		t.Fatalf("long line: %v", err)
	}
}

func TestStorageFailure(t *testing.T) {
	e := start(t, nil)
	c := dial(t, e.addr)
	if err := c.Auth(sasl.NewPlainClient("", "app", "secret")); err != nil {
		t.Fatal(err)
	}
	e.store.Close()
	_, err := sendMsg(c, "a@example.test", []string{"b@example.test"}, msg)
	var se *smtp.SMTPError
	if !errors.As(err, &se) || se.Code != 451 || se.EnhancedCode != (smtp.EnhancedCode{4, 3, 0}) ||
		se.Message != "Temporary local problem, try again later" {
		t.Fatalf("reply = %#v", err)
	}
	if !strings.Contains(e.logs.String(), "storing mail failed") {
		t.Fatal("storage failure not logged")
	}
}

func TestSkipUnknownTargetLogged(t *testing.T) {
	e := start(t, nil)
	c := dial(t, e.addr)
	if err := c.Auth(sasl.NewPlainClient("", "app", "secret")); err != nil {
		t.Fatal(err)
	}
	body := "X-Postwerk-Skip: a, capture, Bad-Name!\r\nSubject: x\r\n\r\nbody\r\n"
	text, err := sendMsg(c, "a@example.test", []string{"x@example.test"}, body)
	if err != nil {
		t.Fatal(err)
	}
	mailID := queuedRe.FindStringSubmatch(text)[1]
	ds, _ := e.store.Deliveries(context.Background(), mailID)
	if ds[0].Target != "a" || ds[0].Status != store.Skipped || ds[1].Status != store.Queued {
		t.Fatalf("deliveries = %+v", ds)
	}
	logs := e.logs.String()
	if !strings.Contains(logs, `"name":"capture"`) || !strings.Contains(logs, `"name":"<invalid>"`) {
		t.Fatalf("unknown skip names not logged: %s", logs)
	}
}

func TestValidAddress(t *testing.T) {
	for _, a := range []string{"", "a@b", strings.Repeat("a", 320)} {
		if !validAddress(a) {
			t.Errorf("validAddress(%q) = false", a)
		}
	}
	for _, a := range []string{"jörg@exämple.test", "用户@例子.广告"} {
		if !validAddress(a) {
			t.Errorf("validAddress(%q) = false", a)
		}
	}
	for _, a := range []string{"a\r\n@b", "a\n@b", "a\x00@b", strings.Repeat("a", 321),
		"a\xff@b", "a\u0085@b", "a\u2028@b", "a\u00a0b@c\u2029"} {
		if validAddress(a) {
			t.Errorf("validAddress(%q) = true", a)
		}
	}
}

func TestShutdownClosesIdleSessions(t *testing.T) {
	st, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "pw.db"), store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	srv := New(Options{Store: st, Sealer: seal.Off(), Senders: senders(), AllowInsecure: true, MaxMessageBytes: 4096, MaxRecipients: 3})
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	served := make(chan error, 1)
	go func() { served <- srv.Serve(l) }()

	c := dial(t, l.Addr().String())
	if err := c.Noop(); err != nil { // an idle, open session
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	start := time.Now()
	if err := srv.Shutdown(ctx); err == nil {
		t.Fatal("shutdown reported success with an open session")
	}
	if time.Since(start) > 2*time.Second {
		t.Fatal("shutdown overran its deadline")
	}
	if err := c.Noop(); err == nil {
		t.Fatal("idle session still open after shutdown")
	}
	if err := <-served; err != nil {
		t.Fatalf("Serve returned %v", err)
	}
	if _, err := net.DialTimeout("tcp", l.Addr().String(), time.Second); err == nil {
		t.Fatal("listener still accepting")
	}
}

func TestMessageIsSealed(t *testing.T) {
	ring, err := keys.Parse([]byte(keys.Generate(time.Now()) + "\n"))
	if err != nil {
		t.Fatal(err)
	}
	sealer, err := seal.New(ring)
	if err != nil {
		t.Fatal(err)
	}
	e := start(t, func(o *Options) { o.Sealer = sealer })
	c := dial(t, e.addr)
	if err := c.Auth(sasl.NewPlainClient("", "app", "secret")); err != nil {
		t.Fatal(err)
	}
	text, err := sendMsg(c, "a@example.test", []string{"x@example.test"}, msg)
	if err != nil {
		t.Fatal(err)
	}
	mailID := queuedRe.FindStringSubmatch(text)[1]
	cl, err := e.store.Claim(context.Background(), "a", time.Now())
	if err != nil || cl == nil || cl.MailID != mailID {
		t.Fatalf("claim = %+v, %v", cl, err)
	}
	if strings.Contains(string(cl.Raw), "Subject: hi") {
		t.Fatal("stored message is not encrypted")
	}
	plain, err := sealer.Open(mailID, cl.Raw)
	if err != nil {
		t.Fatal(err)
	}
	// X-Postwerk-Ref is removed before sealing; everything else is kept.
	if string(plain) != "From: a@example.test\r\nSubject: hi\r\n\r\nbody\r\n" {
		t.Fatalf("opened = %q", plain)
	}
	rec, err := e.store.Mail(context.Background(), mailID)
	if err != nil || rec.Size != len(plain) || rec.Ref != "ref-1" {
		t.Fatalf("record = %+v, %v", rec, err)
	}
}

func TestMaxConnections(t *testing.T) {
	e := start(t, func(o *Options) { o.MaxConnections = 2 })
	// Two sessions fill the limit.
	var open []*smtp.Client
	for range 2 {
		c := dial(t, e.addr)
		if err := c.Noop(); err != nil {
			t.Fatal(err)
		}
		open = append(open, c)
	}
	conn, err := textproto.Dial("tcp", e.addr)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	code, text, err := conn.ReadResponse(0)
	if code != 421 || text != "4.7.0 Too many connections, try again later" {
		t.Fatalf("over the limit: %d %q %v", code, text, err)
	}
	if _, err := conn.ReadLine(); err == nil {
		t.Fatal("connection over the limit stays open")
	}
	if !strings.Contains(e.logs.String(), "too many connections") {
		t.Fatal("refusal not logged")
	}

	// When a session ends, a new one is accepted again.
	if err := open[0].Quit(); err != nil {
		t.Fatal(err)
	}
	testutil.Eventually(t, 5*time.Second, "slot freed", func() bool {
		c, err := smtp.Dial(e.addr)
		if err != nil {
			return false
		}
		defer c.Close()
		return c.Noop() == nil
	})
}

// The SMTP library unquotes a quoted local part, so `"x>"@example.test`
// would reach Rcpt as `x>@example.test` and later be written unquoted into
// the RCPT command towards a target, where the `>` ends the path early. Such
// addresses are refused at once; plain addresses are accepted as before.
func TestEnvelopeAddressSyntax(t *testing.T) {
	e := start(t, func(o *Options) { o.MaxRecipients = 50 })
	c := dial(t, e.addr)
	if err := c.Auth(sasl.NewPlainClient("", "app", "secret")); err != nil {
		t.Fatal(err)
	}
	if err := c.Mail(`"a> RET=FULL"@example.test`, nil); code(err) != 553 {
		t.Fatalf("sender with '>': %v", err)
	}
	if err := c.Mail("a@example.test", nil); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{
		`"evil@evil.test>"@example.test`, // passes *@example.test, but RCPT TO:<evil@evil.test>@example.test>
		`"evil@evil.test"@example.test`,
		`"x y"@example.test`,
		`"a<b"@example.test`,
		`"a\"b"@example.test`,
		`"a,b"@example.test`,
	} {
		if err := c.Rcpt(bad, nil); code(err) != 553 {
			t.Errorf("Rcpt(%s): %v, want 553", bad, err)
		}
	}
	for _, good := range []string{"a.b+tag@example.test", "x_y-z@sub.example.test", "user@[192.0.2.1]", "postmaster"} {
		if err := c.Rcpt(good, nil); err != nil {
			t.Errorf("Rcpt(%s): %v", good, err)
		}
	}
}

// rawSession opens a connection, says EHLO, logs in and returns the
// connection with the EHLO lines.
func rawSession(t *testing.T, addr string) (*textproto.Conn, string) {
	t.Helper()
	conn, err := textproto.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	if _, _, err := conn.ReadResponse(220); err != nil {
		t.Fatal(err)
	}
	conn.Cmd("EHLO x")
	_, ehlo, err := conn.ReadResponse(250)
	if err != nil {
		t.Fatal(err)
	}
	conn.Cmd("AUTH PLAIN AGFwcABzZWNyZXQ=") // \0app\0secret
	if _, _, err := conn.ReadResponse(235); err != nil {
		t.Fatal(err)
	}
	return conn, ehlo
}

func cmd(conn *textproto.Conn, line string) (int, string) {
	conn.Cmd("%s", line)
	code, text, _ := conn.ReadResponse(0)
	return code, text
}

func TestExtensionsOffered(t *testing.T) {
	e := start(t, nil)
	_, ehlo := rawSession(t, e.addr)
	lines := strings.Split(ehlo, "\n")
	has := func(ext string) bool {
		for _, l := range lines {
			if l == ext || strings.HasPrefix(l, ext+" ") {
				return true
			}
		}
		return false
	}
	for _, ext := range []string{"8BITMIME", "SMTPUTF8", "SIZE"} {
		if !has(ext) {
			t.Errorf("EHLO lacks %s: %q", ext, ehlo)
		}
	}
	for _, ext := range []string{"DSN", "REQUIRETLS", "BINARYMIME"} {
		if has(ext) {
			t.Errorf("EHLO offers %s: %q", ext, ehlo)
		}
	}
}

func TestMailParamsStored(t *testing.T) {
	e := start(t, nil)
	conn, _ := rawSession(t, e.addr)
	tests := []struct {
		mail string
		rcpt string
		want store.Params
	}{
		{"MAIL FROM:<a@example.test>", "x@example.test", store.Params{}},
		{"MAIL FROM:<a@example.test> BODY=7BIT SIZE=100", "x@example.test", store.Params{Body: "7BIT", DeclaredSize: 100}},
		{"MAIL FROM:<jörg@exämple.test> BODY=8BITMIME SMTPUTF8", "用户@例子.广告",
			store.Params{Body: "8BITMIME", SMTPUTF8: true}},
	}
	for _, tt := range tests {
		if c, text := cmd(conn, tt.mail); c != 250 {
			t.Fatalf("%s: %d %s", tt.mail, c, text)
		}
		if c, text := cmd(conn, "RCPT TO:<"+tt.rcpt+">"); c != 250 {
			t.Fatalf("RCPT: %d %s", c, text)
		}
		if c, _ := cmd(conn, "DATA"); c != 354 {
			t.Fatalf("DATA: %d", c)
		}
		c, text := cmd(conn, "Subject: hi\r\n\r\nbody\r\n.")
		m := queuedRe.FindStringSubmatch(text)
		if c != 250 || m == nil {
			t.Fatalf("end of data: %d %s", c, text)
		}
		rec, err := e.store.Mail(context.Background(), m[1])
		if err != nil {
			t.Fatal(err)
		}
		if rec.Params != tt.want {
			t.Errorf("%s: params = %+v, want %+v", tt.mail, rec.Params, tt.want)
		}
	}
}

func TestNonASCIIAddressNeedsSMTPUTF8(t *testing.T) {
	e := start(t, nil)
	conn, _ := rawSession(t, e.addr)
	if c, text := cmd(conn, "MAIL FROM:<jörg@exämple.test>"); c != 553 || text != "5.6.7 Non-ASCII address requires SMTPUTF8" {
		t.Fatalf("MAIL: %d %q", c, text)
	}
	if c, _ := cmd(conn, "MAIL FROM:<a@example.test>"); c != 250 {
		t.Fatalf("MAIL: %d", c)
	}
	if c, text := cmd(conn, "RCPT TO:<用户@例子.广告>"); c != 553 || text != "5.6.7 Non-ASCII address requires SMTPUTF8" {
		t.Fatalf("RCPT: %d %q", c, text)
	}
	if c, text := cmd(conn, "RCPT TO:<a\xffb@example.test>"); c != 553 || text != "5.1.3 Invalid address" {
		t.Fatalf("RCPT with invalid UTF-8: %d %q", c, text)
	}
	if c, _ := cmd(conn, "RSET"); c != 250 {
		t.Fatal("RSET")
	}
	if c, _ := cmd(conn, "MAIL FROM:<a@example.test> SMTPUTF8"); c != 250 {
		t.Fatalf("MAIL SMTPUTF8: %d", c)
	}
	if c, text := cmd(conn, "RCPT TO:<用户@例子.广告>"); c != 250 {
		t.Fatalf("RCPT with SMTPUTF8: %d %q", c, text)
	}
}

func TestAuthChecksLimited(t *testing.T) {
	h, err := auth.Hash("secret", auth.MinIterations)
	if err != nil {
		t.Fatal(err)
	}
	fresh, err := auth.ParseSenders(strings.NewReader("app:" + h + "\n")) // nothing cached yet
	if err != nil {
		t.Fatal(err)
	}
	e := start(t, func(o *Options) { o.Senders = fresh; o.MaxAuthChecks = 1; o.authWait = 50 * time.Millisecond })
	// Take the only slot, as a long password check would.
	slots := e.srv.authSlots
	slots <- struct{}{}
	c := dial(t, e.addr)
	err = c.Auth(sasl.NewPlainClient("", "app", "secret"))
	var se *smtp.SMTPError
	if !errors.As(err, &se) || se.Code != 454 || se.Message != "Temporary authentication failure, try again later" {
		t.Fatalf("AUTH while busy: %v", err)
	}
	<-slots
	// Not counted as a failed login; the next try works and is cached.
	if err := c.Auth(sasl.NewPlainClient("", "app", "secret")); err != nil {
		t.Fatalf("AUTH after the slot was freed: %v", err)
	}
	// A cached login needs no slot.
	slots <- struct{}{}
	defer func() { <-slots }()
	c2 := dial(t, e.addr)
	if err := c2.Auth(sasl.NewPlainClient("", "app", "secret")); err != nil {
		t.Fatalf("cached AUTH while busy: %v", err)
	}
}

func TestUnauthenticatedConnectionClosed(t *testing.T) {
	e := start(t, func(o *Options) { o.authTimeout = 300 * time.Millisecond })
	idle, err := textproto.Dial("tcp", e.addr)
	if err != nil {
		t.Fatal(err)
	}
	defer idle.Close()
	if _, _, err := idle.ReadResponse(220); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	code, _, _ := idle.ReadResponse(0)
	if code != 421 || time.Since(start) > 3*time.Second {
		t.Fatalf("idle unauthenticated connection: %d after %v", code, time.Since(start))
	}
	if _, _, err := idle.ReadResponse(0); err == nil {
		t.Fatal("connection still open")
	}

	// A logged-in connection outlives the login deadline.
	conn, _ := rawSession(t, e.addr)
	time.Sleep(500 * time.Millisecond)
	if c, _ := cmd(conn, "NOOP"); c != 250 {
		t.Fatalf("NOOP after login: %d", c)
	}
}
