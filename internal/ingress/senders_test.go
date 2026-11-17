package ingress

import (
	"context"
	"crypto/tls"
	"encoding/base64"
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
