package main

import (
	"bytes"
	"context"
	"io"
	"log"
	"net"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/emersion/go-sasl"
	"github.com/emersion/go-smtp"

	"github.com/eneuberg/postwerk/internal/auth"
	"github.com/eneuberg/postwerk/internal/config"
	"github.com/eneuberg/postwerk/internal/keys"
	"github.com/eneuberg/postwerk/internal/senders"
	"github.com/eneuberg/postwerk/internal/store"
	"github.com/eneuberg/postwerk/internal/testutil"
)

func TestGenSender(t *testing.T) {
	var out, errOut bytes.Buffer
	if code := run([]string{"gen-sender", "billing"}, nil, &out, &errOut); code != 0 {
		t.Fatalf("code %d: %s", code, errOut.String())
	}
	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	if len(lines) != 2 || !strings.HasPrefix(lines[0], "pwk_") || !strings.HasPrefix(lines[1], "billing:pwk-sha256$") {
		t.Fatalf("output %q", out.String())
	}
	id, secret, ok := senders.ParseKey(lines[0])
	if !ok || strings.Contains(lines[1], secret) || !strings.Contains(lines[1], "$"+id+"$") {
		t.Fatalf("line %q", lines[1])
	}
	if n, err := senders.CheckFile([]byte(lines[1]+"\n"), false); err != nil || n != 1 {
		t.Fatalf("line does not parse: %v", err)
	}
	for _, args := range [][]string{{"gen-sender"}, {"gen-sender", "Bad Name"}, {"gen-sender", "a", "b"}} {
		out.Reset()
		if code := run(args, nil, &out, &errOut); code != 2 || out.Len() != 0 {
			t.Errorf("%q: code %d, out %q", args, code, out.String())
		}
	}
}

// TestServeSenderKeys runs the whole program with senders in the file and
// in the database (POSTWERK_SENDER_KEYS=both): a file key, a password line,
// a rotated database sender with two keys, a revoked key (535), an envelope
// sender outside the policy (550), a rate limit (451), and the audit of
// every stored mail.
func TestServeSenderKeys(t *testing.T) {
	var delivered atomic.Int32
	tsrv := smtp.NewServer(smtp.BackendFunc(func(*smtp.Conn) (smtp.Session, error) {
		return &captureSession{done: func([]byte) { delivered.Add(1) }}, nil
	}))
	tsrv.ErrorLog = log.New(io.Discard, "", 0)
	tl, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go tsrv.Serve(tl)
	defer tsrv.Close()

	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "pw.db")
	// Managed senders, created before the start as the UI or the API would.
	st, err := store.Open(ctx, dbPath, store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	db, err := senders.NewDBSource(ctx, st, nil)
	if err != nil {
		t.Fatal(err)
	}
	oldKey, err := db.Create(ctx, senders.CreateRequest{Name: "billing", Actor: "ops"})
	if err != nil {
		t.Fatal(err)
	}
	newKey, err := db.Rotate(ctx, "billing", time.Time{}, "ops")
	if err != nil {
		t.Fatal(err)
	}
	leaked, err := db.Create(ctx, senders.CreateRequest{Name: "leaky", Actor: "ops"})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Revoke(ctx, leaked.KeyID, "ops"); err != nil {
		t.Fatal(err)
	}
	st.Close()

	fileKey, fileID := senders.NewKey()
	fileLine, _ := senders.KeyLine("shop", fileKey)
	h, err := auth.Hash("secret", auth.MinIterations)
	if err != nil {
		t.Fatal(err)
	}
	files := map[string]string{
		"/store-key": keys.Generate(time.Now()) + "\n",
		"/hash-key":  keys.Generate(time.Now()) + "\n",
	}
	sendersFile := realFile(t, "legacy:"+h+"\n"+fileLine+"\nshop:policy from=*@shop.example.test max_per_minute=2\n")
	cfg, err := config.Load([]string{
		"POSTWERK_SMTP_LISTEN=127.0.0.1:0",
		"POSTWERK_HTTP_LISTEN=127.0.0.1:0",
		"POSTWERK_ADMIN_LISTEN=off",
		"POSTWERK_DATA=" + dbPath,
		"POSTWERK_SENDER_KEYS=both",
		"POSTWERK_SENDERS_FILE=" + sendersFile,
		"POSTWERK_STORE_KEY_FILE=/store-key",
		"POSTWERK_HASH_KEY_FILE=/hash-key",
		"POSTWERK_SMTP_ALLOW_INSECURE_AUTH=true",
		"POSTWERK_SHUTDOWN_TIMEOUT=5s",
		"POSTWERK_TARGETS=capture",
		"POSTWERK_TARGET_CAPTURE_URL=smtp+plain://" + tl.Addr().String(),
	}, func(name string) ([]byte, error) { return readTestFile(files, name) })
	if err != nil {
		t.Fatal(err)
	}

	var out testutil.SafeBuffer
	runCtx, cancel := context.WithCancel(ctx)
	addrs := make(chan net.Addr, 1)
	done := make(chan error, 1)
	go func() {
		done <- serve(runCtx, cfg, testutil.Logger(&out), &out, func(s, _, _, _ net.Addr) { addrs <- s })
	}()
	var smtpAddr string
	select {
	case a := <-addrs:
		smtpAddr = a.String()
	case err := <-done:
		t.Fatalf("serve returned early: %v", err)
	}

	send := func(name, password, from string) (string, error) {
		t.Helper()
		c, err := smtp.Dial(smtpAddr)
		if err != nil {
			t.Fatal(err)
		}
		defer c.Close()
		if err := c.Hello("app-pod.example.test"); err != nil {
			t.Fatal(err)
		}
		if err := c.Auth(sasl.NewPlainClient("", name, password)); err != nil {
			return "", err
		}
		if err := c.Mail(from, nil); err != nil {
			return "", err
		}
		if err := c.Rcpt("someone@example.test", nil); err != nil {
			return "", err
		}
		w, err := c.Data()
		if err != nil {
			return "", err
		}
		io.WriteString(w, "From: "+from+"\r\nSubject: x\r\n\r\nhi\r\n")
		resp, err := w.CloseWithResponse()
		if err != nil {
			return "", err
		}
		return strings.TrimPrefix(resp.StatusText, "2.0.0 queued as "), nil
	}
	codeOf := func(err error) int {
		if se, ok := err.(*smtp.SMTPError); ok {
			return se.Code
		}
		return -1
	}

	ids := map[string]string{} // mail id → expected key id ("" for the password)
	for _, c := range []struct {
		name, password, from, keyID string
	}{
		{"shop", fileKey, "orders@shop.example.test", fileID},
		{"billing", oldKey.Key, "billing@example.test", oldKey.KeyID},
		{"billing", newKey.Key, "billing@example.test", newKey.KeyID},
		{"legacy", "secret", "noreply@example.test", ""},
	} {
		mailID, err := send(c.name, c.password, c.from)
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		ids[mailID] = c.keyID
	}
	if _, err := send("leaky", leaked.Key, "x@example.test"); codeOf(err) != 535 {
		t.Fatalf("revoked key: %v", err)
	}
	if _, err := send("billing", fileKey, "x@example.test"); codeOf(err) != 535 {
		t.Fatalf("another sender's key: %v", err)
	}
	if _, err := send("shop", fileKey, "spoof@bank.example.test"); codeOf(err) != 550 {
		t.Fatalf("envelope sender outside the policy: %v", err)
	}
	if _, err := send("shop", fileKey, "orders@shop.example.test"); err != nil {
		t.Fatalf("second mail within the limit: %v", err)
	}
	if _, err := send("shop", fileKey, "orders@shop.example.test"); codeOf(err) != 451 {
		t.Fatalf("third mail in a minute: %v", err)
	}
	testutil.Eventually(t, 10*time.Second, "mails delivered", func() bool { return delivered.Load() == 5 })

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("serve did not stop")
	}

	st, err = store.Open(ctx, dbPath, store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	for mailID, keyID := range ids {
		m, err := st.Mail(ctx, mailID)
		if err != nil {
			t.Fatal(err)
		}
		source := "db"
		if m.Sender == "shop" || m.Sender == "legacy" {
			source = "file"
		}
		want := store.MailAudit{KeyID: keyID, KeySource: source, ClientIP: "127.0.0.1", EHLO: "app-pod.example.test", AuthMech: "PLAIN"}
		if m.Audit != want {
			t.Fatalf("%s audit %+v, want %+v", m.Sender, m.Audit, want)
		}
	}
	// Folded refusals reached the event log at shutdown, and the last
	// logins were written.
	o := out.String()
	for _, want := range []string{
		`"kind":"ingress.rejected"`, `reason=from_not_allowed rejected=1`,
		`"kind":"ingress.rate_limited"`, `limit=max_per_minute limited=1`,
		`"kind":"ingress.auth_failed"`, `"sender":"leaky"`,
		`source=db key=` + newKey.KeyID + ` client=127.0.0.1`,
	} {
		if !strings.Contains(o, want) {
			t.Fatalf("stdout lacks %s", want)
		}
	}
	for _, secret := range []string{fileKey, oldKey.Key, newKey.Key, leaked.Key, "spoof@bank"} {
		if strings.Contains(o, secret) {
			t.Fatalf("stdout holds %q", secret)
		}
	}
	uses, err := st.SenderUsage(ctx)
	if err != nil || len(uses) != 4 {
		t.Fatalf("usage %+v %v", uses, err)
	}
}
