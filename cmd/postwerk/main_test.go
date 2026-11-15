package main

import (
	"bytes"
	"context"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/emersion/go-sasl"
	"github.com/emersion/go-smtp"

	"github.com/eneuberg/postwerk/internal/auth"
	"github.com/eneuberg/postwerk/internal/config"
	"github.com/eneuberg/postwerk/internal/delivery"
	"github.com/eneuberg/postwerk/internal/keys"
	"github.com/eneuberg/postwerk/internal/seal"
	"github.com/eneuberg/postwerk/internal/store"
	"github.com/eneuberg/postwerk/internal/testutil"
)

func TestVersionAndUsage(t *testing.T) {
	var out, errOut bytes.Buffer
	if code := run([]string{"version"}, nil, &out, &errOut); code != 0 || out.String() != "postwerk dev\n" {
		t.Fatalf("version: %d %q", code, out.String())
	}
	if code := run([]string{"nope"}, nil, &out, &errOut); code != 2 {
		t.Fatalf("unknown command: %d", code)
	}
	if code := run([]string{"version", "extra"}, nil, &out, &errOut); code != 2 {
		t.Fatalf("extra argument: %d", code)
	}
}

func TestHashPassword(t *testing.T) {
	var out, errOut bytes.Buffer
	if code := run([]string{"hash-password"}, strings.NewReader("s3cret\n"), &out, &errOut); code != 0 {
		t.Fatalf("code %d: %s", code, errOut.String())
	}
	line := strings.TrimSpace(out.String())
	if strings.Contains(line, "s3cret") || !strings.HasPrefix(line, "pbkdf2-sha256$600000$") {
		t.Fatalf("output = %q", line)
	}
	s, err := auth.ParseSenders(strings.NewReader("app:" + line + "\n"))
	if err != nil || !s.Verify("app", "s3cret") {
		t.Fatalf("hash does not verify: %v", err)
	}
	if code := run([]string{"hash-password"}, strings.NewReader(""), &out, &errOut); code != 1 {
		t.Fatalf("empty password: %d", code)
	}
}

func TestGenToken(t *testing.T) {
	var out, errOut bytes.Buffer
	if code := run([]string{"gen-token", "ops", "admin"}, nil, &out, &errOut); code != 0 {
		t.Fatalf("code %d: %s", code, errOut.String())
	}
	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	if len(lines) != 2 || !regexp.MustCompile(`^[A-Za-z0-9_-]{43}$`).MatchString(lines[0]) ||
		!strings.HasPrefix(lines[1], "ops:admin:pbkdf2-sha256$600000$") || strings.Contains(lines[1], lines[0]) {
		t.Fatalf("output = %q", out.String())
	}
	ts, err := auth.ParseTokens(strings.NewReader(lines[1]))
	if err != nil {
		t.Fatal(err)
	}
	if role, ok := ts.Verify("ops", lines[0]); !ok || role != auth.RoleAdmin {
		t.Fatal("printed token does not verify against its line")
	}
	for _, args := range [][]string{{"gen-token"}, {"gen-token", "ops"}, {"gen-token", "ops", "root"},
		{"gen-token", "Bad Name", "read"}, {"gen-token", "ops", "read", "extra"}} {
		out.Reset()
		if code := run(args, nil, &out, &errOut); code != 2 || out.Len() != 0 {
			t.Errorf("%q: code %d, out %q", args, code, out.String())
		}
	}
}

func TestGenKey(t *testing.T) {
	seen := map[string]bool{}
	for range 3 {
		var out, errOut bytes.Buffer
		if code := run([]string{"gen-key"}, nil, &out, &errOut); code != 0 {
			t.Fatalf("code %d: %s", code, errOut.String())
		}
		line := strings.TrimSpace(out.String())
		if !regexp.MustCompile(`^[0-9]{8}[a-z0-9]{4}:[A-Za-z0-9+/]{43}=$`).MatchString(line) {
			t.Fatalf("line = %q", line)
		}
		if _, err := keys.Parse([]byte(line)); err != nil {
			t.Fatal(err)
		}
		if seen[line] {
			t.Fatal("same key twice")
		}
		seen[line] = true
	}
}

// The default shutdown deadline covers the longest delivery attempt, so a
// send in progress at shutdown always finishes.
func TestShutdownCoversAttempt(t *testing.T) {
	if config.DefaultShutdownTimeout <= delivery.DefaultAttemptTimeout {
		t.Fatalf("shutdown %v <= attempt %v", config.DefaultShutdownTimeout, delivery.DefaultAttemptTimeout)
	}
	for _, d := range []time.Duration{delivery.DefaultDialTimeout, delivery.DefaultCommandTimeout, delivery.DefaultSubmissionTimeout} {
		if d > delivery.DefaultAttemptTimeout {
			t.Fatalf("timeout %v exceeds the attempt cap", d)
		}
	}
}

func TestWarnFilePermissions(t *testing.T) {
	dir := t.TempDir()
	files := map[string]os.FileMode{"open": 0o644, "closed": 0o600, "owngroup": 0o640}
	paths := map[string]string{}
	for name, mode := range files {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte("x"), mode); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(p, mode); err != nil { // the umask may have removed bits
			t.Fatal(err)
		}
		paths["POSTWERK_"+strings.ToUpper(name)] = p
	}
	var out testutil.SafeBuffer
	warnFilePermissions(paths, testutil.Logger(&out))
	logs := out.String()
	// A file created by this process belongs to its own group, so 0640 is fine.
	if strings.Count(logs, "readable by others") != 1 || !strings.Contains(logs, `"variable":"POSTWERK_OPEN"`) ||
		!strings.Contains(logs, `"mode":"0644"`) {
		t.Fatalf("logs = %s", logs)
	}
}

func TestServeConfigError(t *testing.T) {
	t.Setenv("POSTWERK_TARGETS", "")
	t.Setenv("POSTWERK_TYPO", "x")
	var out, errOut bytes.Buffer
	if code := run(nil, nil, &out, &errOut); code != 2 {
		t.Fatalf("code = %d", code)
	}
	if !strings.Contains(errOut.String(), "POSTWERK_TYPO: unknown variable") ||
		!strings.Contains(errOut.String(), "POSTWERK_TARGETS: required") {
		t.Fatalf("stderr = %s", errOut.String())
	}
}

// TestServe runs the whole program against a capture target: one mail in,
// delivered, health endpoints answer, and a cancelled context shuts it down.
func TestServe(t *testing.T) {
	var got []byte
	gotCh := make(chan struct{})
	tsrv := smtp.NewServer(smtp.BackendFunc(func(*smtp.Conn) (smtp.Session, error) {
		return &captureSession{done: func(b []byte) { got = b; close(gotCh) }}, nil
	}))
	tsrv.ErrorLog = log.New(io.Discard, "", 0)
	tl, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go tsrv.Serve(tl)
	defer tsrv.Close()

	h, err := auth.Hash("secret", auth.MinIterations)
	if err != nil {
		t.Fatal(err)
	}
	tokenLine, err := auth.TokenLine("ops", auth.RoleAdmin, "ui-token", auth.MinIterations)
	if err != nil {
		t.Fatal(err)
	}
	dbPath := filepath.Join(t.TempDir(), "pw.db")
	files := map[string]string{
		"/senders":   "app:" + h + "\n",
		"/store-key": keys.Generate(time.Now()) + "\n",
		"/hash-key":  keys.Generate(time.Now()) + "\n",
		"/ui-tokens": tokenLine + "\n",
	}
	cfg, err := config.Load([]string{
		"POSTWERK_SMTP_LISTEN=127.0.0.1:0",
		"POSTWERK_HTTP_LISTEN=127.0.0.1:0",
		"POSTWERK_ADMIN_LISTEN=127.0.0.1:0",
		"POSTWERK_UI_TOKENS_FILE=/ui-tokens",
		"POSTWERK_DATA=" + dbPath,
		"POSTWERK_SENDERS_FILE=/senders",
		"POSTWERK_STORE_KEY_FILE=/store-key",
		"POSTWERK_HASH_KEY_FILE=/hash-key",
		"POSTWERK_SMTP_ALLOW_INSECURE_AUTH=true",
		"POSTWERK_SHUTDOWN_TIMEOUT=5s",
		"POSTWERK_INSTANCE=test",
		"POSTWERK_TARGETS=capture",
		"POSTWERK_TARGET_CAPTURE_URL=smtp+plain://" + tl.Addr().String(),
	}, func(name string) ([]byte, error) { return []byte(files[name]), nil })
	if err != nil {
		t.Fatal(err)
	}

	var out testutil.SafeBuffer
	logger := testutil.Logger(&out)
	ctx, cancel := context.WithCancel(context.Background())
	addrs := make(chan [3]net.Addr, 1)
	done := make(chan error, 1)
	go func() {
		done <- serve(ctx, cfg, logger, &out, func(s, h, a net.Addr) { addrs <- [3]net.Addr{s, h, a} })
	}()
	var a [3]net.Addr
	select {
	case a = <-addrs:
	case err := <-done:
		t.Fatalf("serve returned early: %v", err)
	}

	for _, path := range []string{"/healthz", "/readyz"} {
		resp, err := http.Get("http://" + a[1].String() + path)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != 200 {
			t.Fatalf("%s = %d", path, resp.StatusCode)
		}
	}

	// The UI answers on its own loopback listener, never on the health one.
	if a[2] == nil || !strings.HasPrefix(a[2].String(), "127.0.0.1:") || a[2].String() == a[1].String() {
		t.Fatalf("admin listener = %v", a[2])
	}
	for _, c := range []struct {
		addr, path string
		want       int
	}{
		{a[2].String(), "/login", 200},
		{a[2].String(), "/", 303},
		{a[1].String(), "/login", 404},
	} {
		resp, err := http.Get("http://" + c.addr + c.path)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		got := resp.StatusCode
		if resp.Request.URL.Path != c.path {
			got = 303 // followed the redirect to the login page
		}
		if got != c.want || (c.addr == a[2].String() && resp.Header.Get("Content-Security-Policy") == "") {
			t.Fatalf("GET %s%s = %d (%s), want %d", c.addr, c.path, got, resp.Request.URL.Path, c.want)
		}
	}

	c, err := smtp.Dial(a[0].String())
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Auth(sasl.NewPlainClient("", "app", "secret")); err != nil {
		t.Fatal(err)
	}
	if err := c.SendMail("a@example.test", []string{"b@example.test"}, strings.NewReader("Subject: x\r\n\r\nhi\r\n")); err != nil {
		t.Fatal(err)
	}
	c.Close()
	select {
	case <-gotCh:
	case <-time.After(10 * time.Second):
		t.Fatal("target did not receive the mail")
	}
	if !strings.HasPrefix(string(got), "X-Postwerk-Id: pw_") {
		t.Fatalf("target got %q", got)
	}
	testutil.Eventually(t, 5*time.Second, "handed_off event on stdout", func() bool {
		return strings.Contains(out.String(), `"kind":"delivery.handed_off"`)
	})

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("serve did not stop")
	}
	if !strings.Contains(out.String(), "postwerk stopped") {
		t.Fatalf("no stop log: %s", out.String())
	}
	if strings.Contains(out.String(), "b@example.test") {
		t.Fatal("recipient address on stdout")
	}
	if !strings.Contains(out.String(), `"store_encryption":true`) {
		t.Fatalf("start log without store_encryption: %s", out.String())
	}

	// The store was closed cleanly and holds the mail.
	st, err := store.Open(context.Background(), dbPath, store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	pending, _ := st.PendingByTarget(context.Background())
	if len(pending) != 0 {
		t.Fatalf("pending after delivery: %v", pending)
	}
}

type captureSession struct {
	done func([]byte)
}

func (s *captureSession) Mail(string, *smtp.MailOptions) error { return nil }
func (s *captureSession) Rcpt(string, *smtp.RcptOptions) error { return nil }
func (s *captureSession) Data(r io.Reader) error {
	b, err := io.ReadAll(r)
	if err != nil {
		return err
	}
	s.done(b)
	return nil
}
func (s *captureSession) Reset()        {}
func (s *captureSession) Logout() error { return nil }

// A mail older than POSTWERK_RAW_MAX_AGE whose delivery was left in sending
// by a crash must be expired at the next start, not sent: recovery runs
// before the first retention run.
func TestServeExpiresInterruptedMailPastMaxAge(t *testing.T) {
	received := make(chan struct{}, 1)
	tsrv := smtp.NewServer(smtp.BackendFunc(func(*smtp.Conn) (smtp.Session, error) {
		return &captureSession{done: func([]byte) { received <- struct{}{} }}, nil
	}))
	tsrv.ErrorLog = log.New(io.Discard, "", 0)
	tl, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go tsrv.Serve(tl)
	defer tsrv.Close()

	h, err := auth.Hash("secret", auth.MinIterations)
	if err != nil {
		t.Fatal(err)
	}
	storeKey := keys.Generate(time.Now())
	dbPath := filepath.Join(t.TempDir(), "pw.db")
	files := map[string]string{
		"/senders":   "app:" + h + "\n",
		"/store-key": storeKey + "\n",
		"/hash-key":  keys.Generate(time.Now()) + "\n",
	}

	// The state a crash eight days ago left behind: claimed, not yet sent.
	ctx := context.Background()
	ring, err := keys.Parse([]byte(storeKey))
	if err != nil {
		t.Fatal(err)
	}
	sealer, err := seal.New(ring)
	if err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(ctx, dbPath, store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-8 * 24 * time.Hour)
	const mailID = "pw_01JCX4ZZZZZZZZZZZZZZZZZZZZ"
	blob, err := sealer.Seal(mailID, []byte("Subject: old\r\n\r\nbody\r\n"))
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Accept(ctx, store.NewMail{
		ID: mailID, ReceivedAt: old, Sender: "app", EnvelopeFrom: "a@example.test",
		Recipients: []string{"b@example.test"}, Raw: blob, Size: 20,
		Deliveries: []store.NewDelivery{{Target: "capture", Status: store.Queued}},
	}); err != nil {
		t.Fatal(err)
	}
	if c, err := st.Claim(ctx, "capture", old); err != nil || c == nil {
		t.Fatalf("claim: %v %v", c, err)
	}
	st.Close()

	cfg, err := config.Load([]string{
		"POSTWERK_SMTP_LISTEN=127.0.0.1:0",
		"POSTWERK_HTTP_LISTEN=127.0.0.1:0",
		"POSTWERK_DATA=" + dbPath,
		"POSTWERK_SENDERS_FILE=/senders",
		"POSTWERK_STORE_KEY_FILE=/store-key",
		"POSTWERK_HASH_KEY_FILE=/hash-key",
		"POSTWERK_SMTP_ALLOW_INSECURE_AUTH=true",
		"POSTWERK_SHUTDOWN_TIMEOUT=5s",
		"POSTWERK_ADMIN_LISTEN=off",
		"POSTWERK_TARGETS=capture",
		"POSTWERK_TARGET_CAPTURE_URL=smtp+plain://" + tl.Addr().String(),
	}, func(name string) ([]byte, error) { return []byte(files[name]), nil })
	if err != nil {
		t.Fatal(err)
	}
	var out testutil.SafeBuffer
	runCtx, cancel := context.WithCancel(ctx)
	started := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- serve(runCtx, cfg, testutil.Logger(&out), &out, func(net.Addr, net.Addr, net.Addr) { close(started) })
	}()
	select {
	case <-started:
	case err := <-done:
		t.Fatalf("serve returned early: %v", err)
	}
	select {
	case <-received:
		t.Fatal("a mail past its maximum age was sent")
	case <-time.After(500 * time.Millisecond):
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}

	st, err = store.Open(ctx, dbPath, store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ds, err := st.Deliveries(ctx, mailID)
	if err != nil || len(ds) != 1 || ds[0].Status != store.Failed {
		t.Fatalf("deliveries = %+v, %v", ds, err)
	}
	if !strings.Contains(out.String(), `"kind":"retention.raw_expired"`) {
		t.Fatalf("no retention.raw_expired event: %s", out.String())
	}
}
