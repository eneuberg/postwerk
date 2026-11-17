package main

import (
	"bytes"
	"context"
	"crypto/tls"
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
	"github.com/eneuberg/postwerk/internal/keys"
	"github.com/eneuberg/postwerk/internal/testutil"
)

func TestGenAPIToken(t *testing.T) {
	var out, errOut bytes.Buffer
	if code := run([]string{"gen-api-token", "support", "read"}, nil, &out, &errOut); code != 0 {
		t.Fatalf("code %d: %s", code, errOut.String())
	}
	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	if len(lines) != 2 || !regexp.MustCompile(`^support\.[A-Za-z0-9_-]{43}$`).MatchString(lines[0]) ||
		!regexp.MustCompile(`^support:read:sha256\$[A-Za-z0-9+/]{43}$`).MatchString(lines[1]) {
		t.Fatalf("output = %q", out.String())
	}
	ts, err := auth.ParseAPITokens(strings.NewReader(lines[1]))
	if err != nil {
		t.Fatal(err)
	}
	if role, ok := ts.Verify("support", strings.TrimPrefix(lines[0], "support.")); !ok || role != auth.RoleRead {
		t.Fatal("printed token does not verify against its line")
	}
	for _, args := range [][]string{{"gen-api-token"}, {"gen-api-token", "x", "root"}, {"gen-api-token", "x", "read", "y"}} {
		out.Reset()
		if code := run(args, nil, &out, &errOut); code != 2 || out.Len() != 0 {
			t.Errorf("%q: code %d, out %q", args, code, out.String())
		}
	}
}

// TestServeAPI runs the whole program with the API on, over TLS, and
// checks that it answers on its own listener only.
func TestServeAPI(t *testing.T) {
	dir := t.TempDir()
	cert := testutil.NewCert(t)
	h, err := auth.Hash("secret", auth.MinIterations)
	if err != nil {
		t.Fatal(err)
	}
	tokenLine, err := auth.APITokenLine("support", auth.RoleRead, "api-token")
	if err != nil {
		t.Fatal(err)
	}
	for name, content := range map[string][]byte{
		"senders": []byte("app:" + h + "\n"), "store-key": []byte(keys.Generate(time.Now()) + "\n"),
		"api-tokens": []byte(tokenLine + "\n"), "tls.crt": cert.CertPEM, "tls.key": cert.KeyPEM,
	} {
		if err := os.WriteFile(filepath.Join(dir, name), content, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	cfg, err := config.Load([]string{
		"POSTWERK_SMTP_LISTEN=127.0.0.1:0",
		"POSTWERK_HTTP_LISTEN=127.0.0.1:0",
		"POSTWERK_ADMIN_LISTEN=off",
		"POSTWERK_API_LISTEN=127.0.0.1:0",
		"POSTWERK_API_TOKENS_FILE=" + filepath.Join(dir, "api-tokens"),
		"POSTWERK_API_TLS_CERT_FILE=" + filepath.Join(dir, "tls.crt"),
		"POSTWERK_API_TLS_KEY_FILE=" + filepath.Join(dir, "tls.key"),
		"POSTWERK_API_HOSTS=localhost",
		"POSTWERK_DATA=" + filepath.Join(dir, "pw.db"),
		"POSTWERK_SENDERS_FILE=" + filepath.Join(dir, "senders"),
		"POSTWERK_STORE_KEY_FILE=" + filepath.Join(dir, "store-key"),
		"POSTWERK_PSEUDONYMIZE_AFTER=0",
		"POSTWERK_SMTP_ALLOW_INSECURE_AUTH=true",
		"POSTWERK_SHUTDOWN_TIMEOUT=5s",
		"POSTWERK_TARGETS=capture",
		"POSTWERK_TARGET_CAPTURE_URL=smtp+plain://127.0.0.1:1",
	}, readSecretFile)
	if err != nil {
		t.Fatal(err)
	}
	var out testutil.SafeBuffer
	ctx, cancel := context.WithCancel(context.Background())
	addrs := make(chan [3]net.Addr, 1)
	done := make(chan error, 1)
	go func() {
		done <- serve(ctx, cfg, testutil.Logger(&out), &out, func(sm, h, _, a net.Addr) { addrs <- [3]net.Addr{h, a, sm} })
	}()
	var a [3]net.Addr
	select {
	case a = <-addrs:
	case err := <-done:
		t.Fatalf("serve returned early: %v", err)
	}
	if a[1] == nil || a[1].String() == a[0].String() {
		t.Fatalf("api listener = %v", a[1])
	}
	_, port, _ := net.SplitHostPort(a[1].String())
	client := &http.Client{Timeout: 5 * time.Second, Transport: &http.Transport{
		TLSClientConfig: &tls.Config{RootCAs: cert.Pool, MinVersion: tls.VersionTLS12}}}
	post := func(url, bearer string) int {
		t.Helper()
		req, _ := http.NewRequest("POST", url, strings.NewReader(`{"by":"id","query":"pw_01"}`))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+bearer)
		resp, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	if got := post("https://localhost:"+port+"/v1/search", "support.api-token"); got != 200 {
		t.Fatalf("search over TLS = %d", got)
	}
	if got := post("https://localhost:"+port+"/v1/search", "support.wrong"); got != 401 {
		t.Fatalf("wrong token = %d", got)
	}
	// Plain HTTP is not served, and the health listener has no API.
	plain := &http.Client{Timeout: 5 * time.Second}
	if resp, err := plain.Post("http://"+a[1].String()+"/v1/search", "application/json", nil); err == nil {
		resp.Body.Close()
		if resp.StatusCode == 200 {
			t.Fatal("API answered plain HTTP")
		}
	}
	resp, err := http.Post("http://"+a[0].String()+"/v1/search", "application/json", strings.NewReader("{}"))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode == 200 {
		t.Fatal("health listener serves the API")
	}
	testutil.Eventually(t, 5*time.Second, "api events on stdout", func() bool {
		s := out.String()
		return strings.Contains(s, `"kind":"api.request","actor":"support","text":"search by=id status=200 results=0"`)
	})
	if !strings.Contains(out.String(), `"api_tls":true`) {
		t.Fatalf("start log lacks api_tls: %s", out.String())
	}
	// A failed SMTP login is counted too and written at shutdown (D51).
	sc, err := smtp.Dial(a[2].String())
	if err != nil {
		t.Fatal(err)
	}
	if err := sc.Auth(sasl.NewPlainClient("", "app", "wrong")); err == nil {
		t.Fatal("wrong SMTP password accepted")
	}
	sc.Close()
	// A connection that never sends a request, like an HTTP client's spare
	// keep-alive connection: net/http counts it as busy for five seconds,
	// the whole shutdown deadline here. It must not keep the folded event
	// from the final export.
	idle, err := net.Dial("tcp", a[1].String())
	if err != nil {
		t.Fatal(err)
	}
	defer idle.Close()
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("serve did not stop")
	}
	// The failed authentication is counted in its window and written and
	// exported at shutdown.
	if !strings.Contains(out.String(), `"kind":"api.auth_failed","actor":"support","text":"failures=1"`) {
		t.Fatalf("no folded auth_failed event at shutdown: %s", out.String())
	}
	if !strings.Contains(out.String(), `"kind":"ingress.auth_failed","sender":"app","text":"failures=1"`) {
		t.Fatalf("no folded ingress.auth_failed event at shutdown: %s", out.String())
	}
}

// TestServeSenderAPI runs the whole program with senders in the database
// only (POSTWERK_SENDER_KEYS=db): an admin token creates a sender through
// the API, the application logs in with the key it got and sends a mail,
// and the key is nowhere on stdout.
func TestServeSenderAPI(t *testing.T) {
	dir := t.TempDir()
	tokenLine, err := auth.APITokenLine("ci", auth.RoleAdmin, "api-token")
	if err != nil {
		t.Fatal(err)
	}
	for name, content := range map[string][]byte{
		"store-key": []byte(keys.Generate(time.Now()) + "\n"), "api-tokens": []byte(tokenLine + "\n"),
	} {
		if err := os.WriteFile(filepath.Join(dir, name), content, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	cfg, err := config.Load([]string{
		"POSTWERK_SMTP_LISTEN=127.0.0.1:0",
		"POSTWERK_HTTP_LISTEN=127.0.0.1:0",
		"POSTWERK_ADMIN_LISTEN=off",
		"POSTWERK_API_LISTEN=127.0.0.1:0",
		"POSTWERK_API_TOKENS_FILE=" + filepath.Join(dir, "api-tokens"),
		"POSTWERK_SENDER_KEYS=db",
		"POSTWERK_PSEUDONYMIZE_AFTER=0",
		"POSTWERK_DATA=" + filepath.Join(dir, "pw.db"),
		"POSTWERK_STORE_KEY_FILE=" + filepath.Join(dir, "store-key"),
		"POSTWERK_SMTP_ALLOW_INSECURE_AUTH=true",
		"POSTWERK_SHUTDOWN_TIMEOUT=5s",
		"POSTWERK_TARGETS=capture",
		"POSTWERK_TARGET_CAPTURE_URL=smtp+plain://127.0.0.1:1",
	}, readSecretFile)
	if err != nil {
		t.Fatal(err)
	}
	var out testutil.SafeBuffer
	ctx, cancel := context.WithCancel(context.Background())
	addrs := make(chan [2]net.Addr, 1)
	done := make(chan error, 1)
	go func() {
		done <- serve(ctx, cfg, testutil.Logger(&out), &out, func(sm, _, _, a net.Addr) { addrs <- [2]net.Addr{sm, a} })
	}()
	var a [2]net.Addr
	select {
	case a = <-addrs:
	case err := <-done:
		t.Fatalf("serve returned early: %v", err)
	}
	req, _ := http.NewRequest("POST", "http://"+a[1].String()+"/v1/senders",
		strings.NewReader(`{"name":"ci-app","policy":{"from":["*@example.test"]}}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer ci.api-token")
	resp, err := (&http.Client{Timeout: 5 * time.Second}).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	var body bytes.Buffer
	_, _ = body.ReadFrom(resp.Body)
	resp.Body.Close()
	key := regexp.MustCompile(`pwk_[a-z2-7]{12}_[a-z2-7]{52}`).FindString(body.String())
	if resp.StatusCode != 201 || key == "" {
		t.Fatalf("create: %d %s", resp.StatusCode, body.String())
	}
	sc, err := smtp.Dial(a[0].String())
	if err != nil {
		t.Fatal(err)
	}
	if err := sc.Auth(sasl.NewPlainClient("", "ci-app", key)); err != nil {
		t.Fatalf("login with the new key: %v", err)
	}
	if err := sc.SendMail("noreply@example.test", []string{"r@example.test"}, strings.NewReader("Subject: x\r\n\r\nhi\r\n")); err != nil {
		t.Fatalf("send: %v", err)
	}
	if err := sc.Mail("someone@elsewhere.test", nil); err == nil {
		t.Fatal("an envelope sender outside the policy was accepted")
	}
	sc.Close()
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("serve did not stop")
	}
	s := out.String()
	if !strings.Contains(s, `"kind":"sender.created","sender":"ci-app","actor":"ci"`) || !strings.Contains(s, `"kind":"mail.accepted"`) {
		t.Fatalf("stdout lacks the events: %s", s)
	}
	_, secret, _ := strings.Cut(key[4:], "_")
	if strings.Contains(s, secret) {
		t.Fatal("the key is on stdout")
	}
}
