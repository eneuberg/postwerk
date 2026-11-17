package ingress

import (
	"context"
	"crypto/tls"
	"encoding/base64"
	"errors"
	"net/textproto"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/emersion/go-sasl"
	"github.com/emersion/go-smtp"

	"github.com/eneuberg/postwerk/internal/senders"
	"github.com/eneuberg/postwerk/internal/senders/limit"
	"github.com/eneuberg/postwerk/internal/store"
	"github.com/eneuberg/postwerk/internal/testutil"
)

// keyEnv is a running ingress with a senders file the test can change.
type keyEnv struct {
	*env
	path string
	file *senders.FileSource
	key  string // the key of sender "keyed"
	id   string
}

func startWithKeys(t *testing.T, extra ...string) *keyEnv {
	t.Helper()
	key, id := senders.NewKey()
	line, err := senders.KeyLine("keyed", key)
	if err != nil {
		t.Fatal(err)
	}
	ke := &keyEnv{path: filepath.Join(t.TempDir(), "senders"), key: key, id: id}
	ke.write(t, append([]string{line}, extra...)...)
	ke.file, err = senders.NewFileSource(senders.FileOptions{Path: ke.path})
	if err != nil {
		t.Fatal(err)
	}
	reg, err := senders.NewRegistry(ke.file)
	if err != nil {
		t.Fatal(err)
	}
	a := senders.NewAuthenticator(reg, senders.AuthOptions{})
	ke.env = start(t, func(o *Options) {
		o.Auth, o.Limiter, o.refusalWindow = a, limit.New(nil), time.Hour
	})
	return ke
}

func (ke *keyEnv) write(t *testing.T, lines ...string) {
	t.Helper()
	content := "app:" + testHash() + "\n" + strings.Join(lines, "\n") + "\n"
	if err := os.WriteFile(ke.path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestKeyLoginAndAudit(t *testing.T) {
	ke := startWithKeys(t)
	c := dial(t, ke.addr)
	if err := c.Hello("client.example.test"); err != nil {
		t.Fatal(err)
	}
	if err := c.Auth(sasl.NewPlainClient("", "keyed", ke.key)); err != nil {
		t.Fatal(err)
	}
	text, err := sendMsg(c, "noreply@example.test", []string{"x@example.test"}, msg)
	if err != nil {
		t.Fatal(err)
	}
	m, err := ke.store.Mail(context.Background(), strings.TrimPrefix(text, "2.0.0 queued as "))
	if err != nil {
		t.Fatal(err)
	}
	want := store.MailAudit{KeyID: ke.id, KeySource: "file", ClientIP: "127.0.0.1", EHLO: "client.example.test", AuthMech: "PLAIN"}
	if m.Sender != "keyed" || m.Audit != want {
		t.Fatalf("audit %+v, want %+v", m.Audit, want)
	}
	evs, _ := ke.store.MailEvents(context.Background(), m.ID, 10)
	if len(evs) == 0 || evs[0].Kind != store.KindMailAccepted || evs[0].Text != "source=file key="+ke.id+" client=127.0.0.1" {
		t.Fatalf("mail.accepted %+v", evs)
	}
	// A password line is recorded as such.
	c2 := dial(t, ke.addr)
	if err := c2.Auth(sasl.NewPlainClient("", "app", "secret")); err != nil {
		t.Fatal(err)
	}
	text, err = sendMsg(c2, "", []string{"x@example.test"}, msg)
	if err != nil {
		t.Fatal(err)
	}
	m, _ = ke.store.Mail(context.Background(), strings.TrimPrefix(text, "2.0.0 queued as "))
	if m.Audit.KeyID != "" || m.Audit.KeySource != "file" || m.Audit.EHLO != "localhost" {
		t.Fatalf("password audit %+v", m.Audit)
	}
	evs, _ = ke.store.MailEvents(context.Background(), m.ID, 10)
	if evs[0].Text != "source=file auth=password client=127.0.0.1" {
		t.Fatalf("mail.accepted text %q", evs[0].Text)
	}
}

func TestAuditWithTLS(t *testing.T) {
	cert := testutil.NewCert(t)
	e := start(t, func(o *Options) { o.TLSCertificate = &cert.TLS; o.AllowInsecure = false })
	c, err := smtp.DialStartTLS(e.addr, &tls.Config{RootCAs: cert.Pool, ServerName: "127.0.0.1"})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if err := c.Auth(sasl.NewPlainClient("", "app", "secret")); err != nil {
		t.Fatal(err)
	}
	text, err := sendMsg(c, "", []string{"x@example.test"}, msg)
	if err != nil {
		t.Fatal(err)
	}
	m, _ := e.store.Mail(context.Background(), strings.TrimPrefix(text, "2.0.0 queued as "))
	if m.Audit.TLSVersion != "TLS 1.3" || !strings.HasPrefix(m.Audit.TLSCipher, "TLS_") {
		t.Fatalf("tls audit %+v", m.Audit)
	}
}

func TestPolicyEnforced(t *testing.T) {
	ke := startWithKeys(t, "keyed:policy from=*@example.test max_per_minute=2 max_rcpt_per_hour=3")
	conn := keySession(t, ke)
	for _, step := range []struct {
		line string
		want int
		text string
	}{
		{"MAIL FROM:<x@other.test>", 550, "5.7.1 Sender address not allowed for this login"},
		{"MAIL FROM:<>", 550, "5.7.1 Sender address not allowed for this login"},
		{"MAIL FROM:<a@example.test>", 250, ""},
		{"RCPT TO:<r1@example.test>", 250, ""},
		{"RCPT TO:<r2@example.test>", 250, ""},
		{"RSET", 250, ""},
		{"MAIL FROM:<b@example.test>", 250, ""},
		{"RCPT TO:<r3@example.test>", 250, ""},
		{"RCPT TO:<r4@example.test>", 451, "4.7.1 Rate limit exceeded, try again later"},
		{"RSET", 250, ""},
		{"MAIL FROM:<c@example.test>", 451, "4.7.1 Rate limit exceeded, try again later"},
	} {
		c, text := cmd(conn, step.line)
		if c != step.want || (step.text != "" && text != step.text) {
			t.Fatalf("%s: %d %q, want %d %q", step.line, c, text, step.want, step.text)
		}
	}
	// The folded events: one per sender, reason and window.
	ke.srv.refused.Close()
	got := refusalEvents(t, ke.store)
	want := []string{
		"ingress.rate_limited keyed limit=max_per_minute limited=1",
		"ingress.rate_limited keyed limit=max_rcpt_per_hour limited=1",
		"ingress.rejected keyed reason=from_not_allowed rejected=2",
	}
	if !slices.Equal(got, want) {
		t.Fatalf("events\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	if strings.Contains(strings.Join(got, ""), "other.test") || strings.Contains(ke.logs.String(), "other.test") {
		t.Fatal("an address reached an event or the log")
	}
}

// A key removed from the file ends the sessions that use it at the next MAIL.
func TestRemovedKeyEndsSession(t *testing.T) {
	ke := startWithKeys(t)
	conn := keySession(t, ke)
	if c, _ := cmd(conn, "MAIL FROM:<a@example.test>"); c != 250 {
		t.Fatal("MAIL")
	}
	cmd(conn, "RSET")
	ke.write(t) // the key's line is gone
	if !ke.file.Check(context.Background()) {
		t.Fatal("file not reloaded")
	}
	if c, text := cmd(conn, "MAIL FROM:<a@example.test>"); c != 530 {
		t.Fatalf("MAIL after removal: %d %s", c, text)
	}
	c := dial(t, ke.addr)
	if err := c.Auth(sasl.NewPlainClient("", "keyed", ke.key)); code(err) != 535 {
		t.Fatalf("login with a removed key: %v", err)
	}
}

// keySession is a raw session logged in with the key of "keyed".
func keySession(t *testing.T, ke *keyEnv) *textproto.Conn {
	t.Helper()
	conn, err := textproto.Dial("tcp", ke.addr)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	if _, _, err := conn.ReadResponse(220); err != nil {
		t.Fatal(err)
	}
	for _, step := range []struct {
		line string
		want int
	}{{"EHLO x", 250}, {"AUTH PLAIN " + plain("keyed", ke.key), 235}} {
		if c, text := cmd(conn, step.line); c != step.want {
			t.Fatalf("%s: %d %s", step.line, c, text)
		}
	}
	return conn
}

// plain is the AUTH PLAIN response for name and password.
func plain(name, password string) string {
	return base64.StdEncoding.EncodeToString([]byte("\x00" + name + "\x00" + password))
}

// The From: header is bound by the same patterns as MAIL FROM (D81).
func TestFromHeaderAllowed(t *testing.T) {
	policy, err := senders.ParsePolicy("from=*@example.test,noreply@shop.test")
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name     string
		fields   []string
		smtputf8 bool
		want     bool
	}{
		{"match", []string{"a@example.test"}, false, true},
		{"display name", []string{`Shop <noreply@shop.test>`}, false, true},
		{"case", []string{`"Shop" <NoReply@Shop.TEST>`}, false, true},
		{"encoded-word display name", []string{"=?utf-8?q?M=C3=BCller?= <a@example.test>"}, false, true},
		{"unknown charset in display name", []string{"=?x-unknown?q?abc?= <a@example.test>"}, false, true},
		{"comment", []string{"a@example.test (Shop)"}, false, true},
		{"several allowed", []string{"a@example.test, Shop <noreply@shop.test>"}, false, true},
		{"group of allowed", []string{"team: a@example.test, b@example.test;"}, false, true},
		{"mismatch", []string{"evil@other.test"}, false, false},
		{"address in display name", []string{`"ok@example.test" <evil@other.test>`}, false, false},
		{"address in comment", []string{"evil@other.test (ok@example.test)"}, false, false},
		{"one of several not allowed", []string{"a@example.test, evil@other.test"}, false, false},
		{"group with one not allowed", []string{"team: a@example.test, evil@other.test;"}, false, false},
		{"empty group", []string{"undisclosed-recipients:;"}, false, false},
		{"missing", nil, false, false},
		{"empty", []string{""}, false, false},
		{"duplicate", []string{"a@example.test", "b@example.test"}, false, false},
		{"unparseable", []string{"a@example.test <"}, false, false},
		{"quoted local part with @", []string{`"evil@other.test"@example.test`}, false, false},
		{"subdomain", []string{"a@evil.example.test.other.test"}, false, false},
		{"kelvin sign", []string{"noreply@shop.testK"}, true, false},
		{"non-ASCII without SMTPUTF8", []string{"ä@example.test"}, false, false},
		{"non-ASCII with SMTPUTF8", []string{"ä@example.test"}, true, true},
	} {
		if got := fromHeaderAllowed(policy, tc.fields, tc.smtputf8); got != tc.want {
			t.Errorf("%s: %q allowed = %v, want %v", tc.name, tc.fields, got, tc.want)
		}
	}
	// Without from patterns nothing is checked, as before.
	for _, fields := range [][]string{nil, {"evil@other.test"}, {"a", "b"}} {
		if !fromHeaderAllowed(senders.Policy{}, fields, false) {
			t.Errorf("no patterns: %q refused", fields)
		}
	}
}

// A From: header outside the patterns is refused at the end of DATA, with
// the reply and event of a refused MAIL FROM, and nothing is stored.
func TestFromHeaderRefusedAtData(t *testing.T) {
	ke := startWithKeys(t, "keyed:policy from=*@example.test")
	c := dial(t, ke.addr)
	if err := c.Auth(sasl.NewPlainClient("", "keyed", ke.key)); err != nil {
		t.Fatal(err)
	}
	for _, body := range []string{
		"From: \"a@example.test\" <evil@other.test>\r\nSubject: hi\r\n\r\nbody\r\n",
		"Subject: no From\r\n\r\nFrom: a@example.test\r\n",
		"From: a@example.test\r\nFROM: b@example.test\r\n\r\nbody\r\n",
	} {
		_, err := sendMsg(c, "a@example.test", []string{"x@example.test"}, body)
		var se *smtp.SMTPError
		if !errors.As(err, &se) || se.Code != 550 || se.EnhancedCode != (smtp.EnhancedCode{5, 7, 1}) {
			t.Fatalf("%q: %v, want 550 5.7.1", body, err)
		}
		if err := c.Reset(); err != nil {
			t.Fatal(err)
		}
	}
	if mails, err := ke.store.ListMails(context.Background(), store.MailFilter{Limit: 10}); err != nil || len(mails) != 0 {
		t.Fatalf("stored %d mails (%v)", len(mails), err)
	}
	// An allowed header goes through on the same connection.
	if _, err := sendMsg(c, "a@example.test", []string{"x@example.test"}, msg); err != nil {
		t.Fatal(err)
	}
	ke.srv.refused.Close()
	got := refusalEvents(t, ke.store)
	if want := []string{"ingress.rejected keyed reason=from_not_allowed rejected=3"}; !slices.Equal(got, want) {
		t.Fatalf("events %q, want %q", got, want)
	}
	if strings.Contains(ke.logs.String(), "other.test") {
		t.Fatal("an address reached the log")
	}
}
