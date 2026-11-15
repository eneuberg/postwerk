package config

import (
	"errors"
	"fmt"
	"io/fs"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/eneuberg/postwerk/internal/auth"
	"github.com/eneuberg/postwerk/internal/keys"
	"github.com/eneuberg/postwerk/internal/testutil"
)

// Fixed test keys (32 bytes each, base64).
const (
	storeKeyLine = "s1:AAECAwQFBgcICQoLDA0ODxAREhMUFRYXGBkaGxwdHh8="
	storeOldLine = "s0:ICEiIyQlJicoKSorLC0uLzAxMjM0NTY3ODk6Ozw9Pj8="
	hashKeyLine  = "h1:QEFCQ0RFRkdISUpLTE1OT1BRUlNUVVZXWFlaW1xdXl8="
)

type files map[string]string

func (f files) read(name string) ([]byte, error) {
	if v, ok := f[name]; ok {
		return []byte(v), nil
	}
	return nil, &fs.PathError{Op: "open", Path: name, Err: fs.ErrNotExist}
}

var tokenLine = sync.OnceValue(func() string {
	line, err := auth.TokenLine("ops", auth.RoleAdmin, "ui-token", auth.MinIterations)
	if err != nil {
		panic(err)
	}
	return line
})

var senderHash = sync.OnceValue(func() string {
	h, err := auth.Hash("pw", auth.MinIterations)
	if err != nil {
		panic(err)
	}
	return h
})

func baseFiles(t *testing.T) files {
	t.Helper()
	return files{
		"/run/secrets/senders":   "app:" + senderHash() + "\n",
		"/run/secrets/mailjet":   "apikey:apisecret:with-colon\nignored second line\n",
		"/run/secrets/store-key": "# active key first\n" + storeKeyLine + "\n\n" + storeOldLine + "\n",
		"/run/secrets/hash-key":  hashKeyLine + "\n",
		"/run/secrets/ui-tokens": tokenLine() + "\n",
	}
}

func minimalEnv() []string {
	return []string{
		"PATH=/usr/bin",
		"POSTWERK_SENDERS_FILE=/run/secrets/senders",
		"POSTWERK_SMTP_ALLOW_INSECURE_AUTH=true",
		"POSTWERK_STORE_KEY_FILE=/run/secrets/store-key",
		"POSTWERK_HASH_KEY_FILE=/run/secrets/hash-key",
		"POSTWERK_UI_TOKENS_FILE=/run/secrets/ui-tokens",
		"POSTWERK_TARGETS=mailpit",
		"POSTWERK_TARGET_MAILPIT_URL=smtp+plain://mailpit:1025",
	}
}

func TestMinimalDefaults(t *testing.T) {
	c, err := Load(minimalEnv(), baseFiles(t).read)
	if err != nil {
		t.Fatal(err)
	}
	if c.SMTPListen != ":2525" || c.HTTPListen != "127.0.0.1:8080" || c.DataPath != "/data/postwerk.db" ||
		c.Hostname != "postwerk" || c.MaxMessageBytes != 15728640 || c.MaxRecipients != 50 ||
		c.RetryWindow != 24*time.Hour || c.ShutdownTimeout != 100*time.Second || c.Instance != "" ||
		c.TLSCertificate != nil || !c.AllowInsecureAuth || c.Senders == nil || c.MaxConnections != 100 ||
		c.MaxInflightBytes != 256<<20 {
		t.Fatalf("defaults wrong: %+v", c)
	}
	// The recommended retention defaults.
	if c.RawKeepAfterHandoff != 0 || c.RawKeepAfterFailure != 72*time.Hour || c.RawMaxAge != 7*24*time.Hour ||
		c.PseudonymizeAfter != 30*24*time.Hour || c.RecordRetention != 90*24*time.Hour ||
		c.EventRetention != 90*24*time.Hour || !c.SecureDelete {
		t.Fatalf("retention defaults wrong: %+v", c)
	}
	if c.StoreKeys == nil || c.StoreKeys.Active().ID != "s1" || len(c.StoreKeys.All()) != 2 ||
		c.HashKeys == nil || c.HashKeys.Active().ID != "h1" {
		t.Fatalf("keys = %v %v", c.StoreKeys, c.HashKeys)
	}
	if len(c.Warnings()) != 0 {
		t.Fatalf("warnings with defaults: %q", c.Warnings())
	}
	if c.Admin.Listen != "127.0.0.1:8081" || c.Admin.NonLoopback || c.Admin.Hosts != nil || c.Admin.Tokens == nil {
		t.Fatalf("admin defaults wrong: %+v", c.Admin)
	}
	if role, ok := c.Admin.Tokens.Verify("ops", "ui-token"); !ok || role != auth.RoleAdmin {
		t.Fatal("token from the file does not verify")
	}
	want := map[string]string{
		"POSTWERK_SENDERS_FILE":   "/run/secrets/senders",
		"POSTWERK_STORE_KEY_FILE": "/run/secrets/store-key",
		"POSTWERK_HASH_KEY_FILE":  "/run/secrets/hash-key",
		"POSTWERK_UI_TOKENS_FILE": "/run/secrets/ui-tokens",
	}
	if !reflect.DeepEqual(c.SecretFiles, want) {
		t.Fatalf("secret files = %v", c.SecretFiles)
	}
	if s := fmt.Sprintf("%v %+v %#v", c.StoreKeys, c.StoreKeys.Active(), c.HashKeys.All()); strings.Contains(s, "AAEC") ||
		strings.Contains(s, "QEFC") {
		t.Fatalf("key material printed: %s", s)
	}
	wantTargets := []Target{{
		Name: "mailpit", Scheme: SchemePlain, Host: "mailpit", Addr: "mailpit:1025",
		RetryWindow: 24 * time.Hour, OnUnknown: Hold,
	}}
	if !reflect.DeepEqual(c.Targets, wantTargets) {
		t.Fatalf("targets = %+v", c.Targets)
	}
}

func TestFullConfig(t *testing.T) {
	cert := testutil.NewCert(t)
	f := baseFiles(t)
	f["/tls/cert.pem"] = string(cert.CertPEM)
	f["/tls/key.pem"] = string(cert.KeyPEM)
	env := []string{
		"POSTWERK_SMTP_LISTEN=0.0.0.0:2526",
		"POSTWERK_HTTP_LISTEN=:9090",
		"POSTWERK_DATA=/var/lib/pw.db",
		"POSTWERK_SENDERS_FILE=/run/secrets/senders",
		"POSTWERK_SMTP_TLS_CERT_FILE=/tls/cert.pem",
		"POSTWERK_SMTP_TLS_KEY_FILE=/tls/key.pem",
		"POSTWERK_STORE_KEY_FILE=/run/secrets/store-key",
		"POSTWERK_HASH_KEY_FILE=/run/secrets/hash-key",
		"POSTWERK_SECURE_DELETE=off",
		"POSTWERK_RAW_KEEP_AFTER_HANDOFF=1h",
		"POSTWERK_RAW_KEEP_AFTER_FAILURE=0",
		"POSTWERK_RAW_MAX_AGE=2d",
		"POSTWERK_PSEUDONYMIZE_AFTER=2d12h",
		"POSTWERK_RECORD_RETENTION=400d",
		"POSTWERK_EVENT_RETENTION=30d",
		"POSTWERK_MAX_CONNECTIONS=7",
		"POSTWERK_MAX_INFLIGHT_BYTES=2097152",
		"POSTWERK_HOSTNAME=relay.example.test",
		"POSTWERK_MAX_MESSAGE_BYTES=1048576",
		"POSTWERK_MAX_RECIPIENTS=10",
		"POSTWERK_RETRY_WINDOW=12h",
		"POSTWERK_SHUTDOWN_TIMEOUT=40s",
		"POSTWERK_INSTANCE=prod-1",
		"POSTWERK_OTEL_ENDPOINT=",
		"POSTWERK_SENTRY_DSN=",
		"POSTWERK_WEBHOOK_URL=",
		"POSTWERK_ADMIN_LISTEN=[::1]:9091",
		"POSTWERK_ADMIN_HOSTS=relay.example.test",
		"POSTWERK_UI_TOKENS_FILE=/run/secrets/ui-tokens",
		"POSTWERK_TARGETS=mailjet, capture,tls_only",
		"POSTWERK_TARGET_MAILJET_URL=smtp://in-v3.mailjet.com",
		"POSTWERK_TARGET_MAILJET_CREDENTIALS_FILE=/run/secrets/mailjet",
		"POSTWERK_TARGET_MAILJET_STATUS=mailjet",
		"POSTWERK_TARGET_CAPTURE_URL=smtp+plain://mailpit:1025",
		"POSTWERK_TARGET_CAPTURE_OPTIONAL=true",
		"POSTWERK_TARGET_CAPTURE_ON_UNKNOWN=retry",
		"POSTWERK_TARGET_CAPTURE_RETRY_WINDOW=1h",
		"POSTWERK_TARGET_CAPTURE_ALLOW_RECIPIENTS=*@Example.test, qa-*@corp.test",
		"POSTWERK_TARGET_TLS_ONLY_URL=smtps://[::1]:4650",
		"POSTWERK_TARGET_TLS_ONLY_OPTIONAL=false",
	}
	c, err := Load(env, f.read)
	if err != nil {
		t.Fatal(err)
	}
	if c.SMTPListen != "0.0.0.0:2526" || c.HTTPListen != ":9090" || c.DataPath != "/var/lib/pw.db" ||
		c.Hostname != "relay.example.test" || c.MaxMessageBytes != 1<<20 || c.MaxRecipients != 10 ||
		c.RetryWindow != 12*time.Hour || c.ShutdownTimeout != 40*time.Second || c.Instance != "prod-1" ||
		c.TLSCertificate == nil || c.AllowInsecureAuth || c.MaxConnections != 7 ||
		c.MaxInflightBytes != 2<<20 || c.Admin.Listen != "[::1]:9091" || c.Admin.Tokens == nil ||
		!reflect.DeepEqual(c.Admin.Hosts, []string{"relay.example.test"}) {
		t.Fatalf("globals wrong: %+v", c)
	}
	if c.SecureDelete || c.RawKeepAfterHandoff != time.Hour || c.RawKeepAfterFailure != 0 ||
		c.RawMaxAge != 48*time.Hour || c.PseudonymizeAfter != 60*time.Hour || c.RecordRetention != 400*24*time.Hour ||
		c.EventRetention != 30*24*time.Hour {
		t.Fatalf("retention wrong: %+v", c)
	}
	want := []Target{
		{
			Name: "mailjet", Scheme: SchemeSTARTTLS, Host: "in-v3.mailjet.com", Addr: "in-v3.mailjet.com:587",
			Username: "apikey", Password: "apisecret:with-colon", Status: "mailjet",
			RetryWindow: 12 * time.Hour, OnUnknown: Check,
		},
		{
			Name: "capture", Scheme: SchemePlain, Host: "mailpit", Addr: "mailpit:1025", Optional: true,
			RetryWindow: time.Hour, OnUnknown: Retry, AllowRecipients: []string{"*@example.test", "qa-*@corp.test"},
		},
		{
			Name: "tls_only", Scheme: SchemeSMTPS, Host: "::1", Addr: "[::1]:4650",
			RetryWindow: 12 * time.Hour, OnUnknown: Hold,
		},
	}
	if !reflect.DeepEqual(c.Targets, want) {
		t.Fatalf("targets:\n%+v\nwant\n%+v", c.Targets, want)
	}
}

func TestDefaultPorts(t *testing.T) {
	for url, addr := range map[string]string{
		"smtps://h":      "h:465",
		"smtp://h":       "h:587",
		"smtp+plain://h": "h:25",
		"SMTP://h:2525":  "h:2525",
	} {
		env := append(minimalEnv(), "POSTWERK_TARGET_MAILPIT_URL="+url)
		c, err := Load(env, baseFiles(t).read)
		if err != nil {
			t.Fatalf("%s: %v", url, err)
		}
		if c.Targets[0].Addr != addr {
			t.Errorf("%s: addr = %s, want %s", url, c.Targets[0].Addr, addr)
		}
	}
}

func TestErrors(t *testing.T) {
	tests := []struct {
		name string
		env  []string // added to minimalEnv; a later entry overrides
		want string
	}{
		{"unknown variable", []string{"POSTWERK_SMTP_LISTN=:25"}, "POSTWERK_SMTP_LISTN: unknown variable"},
		{"target var for unlisted target", []string{"POSTWERK_TARGET_OTHER_URL=smtp://x"}, "POSTWERK_TARGET_OTHER_URL: unknown variable"},
		{"later phase", []string{"POSTWERK_SENTRY_DSN=https://x"}, "POSTWERK_SENTRY_DSN: not implemented yet"},
		{"bad listen", []string{"POSTWERK_SMTP_LISTEN=2525"}, "POSTWERK_SMTP_LISTEN: expected host:port"},
		{"bad listen port", []string{"POSTWERK_HTTP_LISTEN=:99999"}, "POSTWERK_HTTP_LISTEN: invalid port"},
		{"bad hostname", []string{"POSTWERK_HOSTNAME=bad host"}, "POSTWERK_HOSTNAME"},
		{"bad instance", []string{"POSTWERK_INSTANCE=a b"}, "POSTWERK_INSTANCE"},
		{"bad size", []string{"POSTWERK_MAX_MESSAGE_BYTES=10"}, "POSTWERK_MAX_MESSAGE_BYTES"},
		{"size text", []string{"POSTWERK_MAX_MESSAGE_BYTES=25MB"}, "POSTWERK_MAX_MESSAGE_BYTES"},
		{"bad recipients", []string{"POSTWERK_MAX_RECIPIENTS=0"}, "POSTWERK_MAX_RECIPIENTS"},
		{"bad window", []string{"POSTWERK_RETRY_WINDOW=1w"}, "POSTWERK_RETRY_WINDOW"},
		{"window too long", []string{"POSTWERK_RETRY_WINDOW=7d"}, "POSTWERK_RETRY_WINDOW (168h0m0s) must be shorter than POSTWERK_RAW_MAX_AGE"},
		{"target window too long", []string{"POSTWERK_TARGET_MAILPIT_RETRY_WINDOW=8d"}, "POSTWERK_TARGET_MAILPIT_RETRY_WINDOW (192h0m0s) must be shorter"},
		{"tls and insecure", []string{"POSTWERK_SMTP_TLS_CERT_FILE=/x", "POSTWERK_SMTP_TLS_KEY_FILE=/y"}, "is ambiguous"},
		{"bad max connections", []string{"POSTWERK_MAX_CONNECTIONS=0"}, "POSTWERK_MAX_CONNECTIONS"},
		{"bad inflight", []string{"POSTWERK_MAX_INFLIGHT_BYTES=1GB"}, "POSTWERK_MAX_INFLIGHT_BYTES: expected a whole number"},
		{"inflight below size", []string{"POSTWERK_MAX_INFLIGHT_BYTES=1048576"}, "POSTWERK_MAX_INFLIGHT_BYTES must be at least POSTWERK_MAX_MESSAGE_BYTES"},
		{"missing store key", []string{"POSTWERK_STORE_KEY_FILE="}, "POSTWERK_STORE_KEY_FILE: required"},
		{"encryption off with key", []string{"POSTWERK_STORE_ENCRYPTION=off"}, "POSTWERK_STORE_ENCRYPTION=off together with POSTWERK_STORE_KEY_FILE is ambiguous"},
		{"bad encryption", []string{"POSTWERK_STORE_ENCRYPTION=false"}, "POSTWERK_STORE_ENCRYPTION: expected on or off"},
		{"bad secure delete", []string{"POSTWERK_SECURE_DELETE=1"}, "POSTWERK_SECURE_DELETE: expected on or off"},
		{"unreadable store key", []string{"POSTWERK_STORE_KEY_FILE=/nope"}, "POSTWERK_STORE_KEY_FILE: cannot read file"},
		{"bad store key", []string{"POSTWERK_STORE_KEY_FILE=/run/secrets/senders"}, "POSTWERK_STORE_KEY_FILE: line 1: key must be 32 bytes"},
		{"missing hash key", []string{"POSTWERK_HASH_KEY_FILE="}, "POSTWERK_HASH_KEY_FILE: required while POSTWERK_PSEUDONYMIZE_AFTER"},
		{"same key twice", []string{"POSTWERK_HASH_KEY_FILE=/run/secrets/store-key"}, "contain the same key"},
		{"zero max age", []string{"POSTWERK_RAW_MAX_AGE=0"}, "POSTWERK_RAW_MAX_AGE: expected a positive duration"},
		{"zero records", []string{"POSTWERK_RECORD_RETENTION=0s"}, "POSTWERK_RECORD_RETENTION: expected a positive"},
		{"zero events", []string{"POSTWERK_EVENT_RETENTION=0"}, "POSTWERK_EVENT_RETENTION: expected a positive"},
		{"negative keep", []string{"POSTWERK_RAW_KEEP_AFTER_FAILURE=-1h"}, "expected 0 or a positive duration"},
		{"too long", []string{"POSTWERK_RECORD_RETENTION=40000d"}, "POSTWERK_RECORD_RETENTION"},
		{"bad days", []string{"POSTWERK_RECORD_RETENTION=1dx"}, "POSTWERK_RECORD_RETENTION"},
		{"keep after handoff over max age", []string{"POSTWERK_RAW_KEEP_AFTER_HANDOFF=8d"}, "POSTWERK_RAW_KEEP_AFTER_HANDOFF (192h0m0s) must not be longer than POSTWERK_RAW_MAX_AGE"},
		{"keep after failure over max age", []string{"POSTWERK_RAW_KEEP_AFTER_FAILURE=8d"}, "POSTWERK_RAW_KEEP_AFTER_FAILURE (192h0m0s) must not be longer than POSTWERK_RAW_MAX_AGE"},
		{"pseudonymize before max age", []string{"POSTWERK_PSEUDONYMIZE_AFTER=3d"}, "POSTWERK_RAW_MAX_AGE (168h0m0s) must not be longer than POSTWERK_PSEUDONYMIZE_AFTER"},
		{"pseudonymize after records", []string{"POSTWERK_PSEUDONYMIZE_AFTER=100d"}, "POSTWERK_PSEUDONYMIZE_AFTER (2400h0m0s) must not be longer than POSTWERK_RECORD_RETENTION"},
		{"records before max age", []string{"POSTWERK_RECORD_RETENTION=5d", "POSTWERK_PSEUDONYMIZE_AFTER=0"}, "POSTWERK_RAW_MAX_AGE (168h0m0s) must not be longer than POSTWERK_RECORD_RETENTION"},
		{"events before max age", []string{"POSTWERK_EVENT_RETENTION=5d"}, "POSTWERK_RAW_MAX_AGE (168h0m0s) must not be longer than POSTWERK_EVENT_RETENTION"},
		{"negative window", []string{"POSTWERK_RETRY_WINDOW=-1h"}, "POSTWERK_RETRY_WINDOW"},
		{"bad shutdown", []string{"POSTWERK_SHUTDOWN_TIMEOUT=0s"}, "POSTWERK_SHUTDOWN_TIMEOUT"},
		{"bad bool", []string{"POSTWERK_SMTP_ALLOW_INSECURE_AUTH=yes"}, "POSTWERK_SMTP_ALLOW_INSECURE_AUTH: expected true or false"},
		{"insecure rule", []string{"POSTWERK_SMTP_ALLOW_INSECURE_AUTH=false"}, "ingress needs TLS for AUTH"},
		{"cert without key", []string{"POSTWERK_SMTP_TLS_CERT_FILE=/run/secrets/senders"}, "must be set together"},
		{"bad cert", []string{"POSTWERK_SMTP_TLS_CERT_FILE=/run/secrets/senders", "POSTWERK_SMTP_TLS_KEY_FILE=/run/secrets/senders"}, "not a valid PEM"},
		{"missing senders", []string{"POSTWERK_SENDERS_FILE="}, "POSTWERK_SENDERS_FILE: required"},
		{"unreadable senders", []string{"POSTWERK_SENDERS_FILE=/nope"}, "POSTWERK_SENDERS_FILE: cannot read file"},
		{"bad senders", []string{"POSTWERK_SENDERS_FILE=/run/secrets/mailjet"}, "POSTWERK_SENDERS_FILE: line 1"},
		{"missing targets", []string{"POSTWERK_TARGETS="}, "POSTWERK_TARGETS: required"},
		{"bad target name", []string{"POSTWERK_TARGETS=Mail-Pit"}, "must be 1 to 32"},
		{"duplicate target", []string{"POSTWERK_TARGETS=mailpit,mailpit"}, "listed twice"},
		{"missing url", []string{"POSTWERK_TARGET_MAILPIT_URL="}, "POSTWERK_TARGET_MAILPIT_URL: required"},
		{"bad scheme", []string{"POSTWERK_TARGET_MAILPIT_URL=http://x"}, "scheme must be"},
		{"userinfo", []string{"POSTWERK_TARGET_MAILPIT_URL=smtp://u:secretpw@x"}, "no user or password in the URL"},
		{"path", []string{"POSTWERK_TARGET_MAILPIT_URL=smtp://x/path"}, "no path, query"},
		{"query", []string{"POSTWERK_TARGET_MAILPIT_URL=smtp://x?tls=off"}, "no path, query"},
		{"no host", []string{"POSTWERK_TARGET_MAILPIT_URL=smtp://:25"}, "host missing"},
		{"bad port", []string{"POSTWERK_TARGET_MAILPIT_URL=smtp://x:0"}, "invalid port"},
		{"plain with credentials", []string{"POSTWERK_TARGET_MAILPIT_CREDENTIALS_FILE=/run/secrets/mailjet"}, "credentials are not allowed with smtp+plain"},
		{"unreadable credentials", []string{"POSTWERK_TARGET_MAILPIT_URL=smtps://x", "POSTWERK_TARGET_MAILPIT_CREDENTIALS_FILE=/nope"}, "cannot read file"},
		{"bad status", []string{"POSTWERK_TARGET_MAILPIT_STATUS=sendgrid"}, "unknown status adapter"},
		{"bad optional", []string{"POSTWERK_TARGET_MAILPIT_OPTIONAL=1"}, "POSTWERK_TARGET_MAILPIT_OPTIONAL: expected true or false"},
		{"bad target window", []string{"POSTWERK_TARGET_MAILPIT_RETRY_WINDOW=soon"}, "POSTWERK_TARGET_MAILPIT_RETRY_WINDOW"},
		{"bad on unknown", []string{"POSTWERK_TARGET_MAILPIT_ON_UNKNOWN=drop"}, "expected hold, retry or check"},
		{"check without status", []string{"POSTWERK_TARGET_MAILPIT_ON_UNKNOWN=check"}, "check needs a status adapter"},
		{"bad pattern", []string{"POSTWERK_TARGET_MAILPIT_ALLOW_RECIPIENTS=[a@x"}, "invalid pattern"},
		{"pattern without at", []string{"POSTWERK_TARGET_MAILPIT_ALLOW_RECIPIENTS=example.test"}, "has no @"},
		{"empty pattern", []string{"POSTWERK_TARGET_MAILPIT_ALLOW_RECIPIENTS=*@x,,"}, "empty pattern"},
		{"admin on all interfaces", []string{"POSTWERK_ADMIN_LISTEN=:8081"}, "POSTWERK_ADMIN_LISTEN: must be a loopback address"},
		{"admin on a public address", []string{"POSTWERK_ADMIN_LISTEN=192.0.2.10:8081"}, "must be a loopback address"},
		{"admin by host name", []string{"POSTWERK_ADMIN_LISTEN=postwerk.internal:8081"}, "must be a loopback address"},
		{"admin on unspecified v6", []string{"POSTWERK_ADMIN_LISTEN=[::]:8081"}, "must be a loopback address"},
		{"admin bad address", []string{"POSTWERK_ADMIN_LISTEN=8081"}, "POSTWERK_ADMIN_LISTEN: expected host:port"},
		{"admin shares health", []string{"POSTWERK_ADMIN_LISTEN=127.0.0.1:8080"}, "must differ from POSTWERK_HTTP_LISTEN"},
		{"admin bad allow", []string{"POSTWERK_ADMIN_ALLOW_NON_LOOPBACK=yes"}, "POSTWERK_ADMIN_ALLOW_NON_LOOPBACK: expected true or false"},
		{"admin without tokens", []string{"POSTWERK_UI_TOKENS_FILE="}, "POSTWERK_UI_TOKENS_FILE: required while the admin listener is on"},
		{"admin unreadable tokens", []string{"POSTWERK_UI_TOKENS_FILE=/nope"}, "POSTWERK_UI_TOKENS_FILE: cannot read file"},
		{"admin senders as tokens", []string{"POSTWERK_UI_TOKENS_FILE=/run/secrets/senders"}, "POSTWERK_UI_TOKENS_FILE: line 1: expected name:role:hash"},
		{"admin bad host", []string{"POSTWERK_ADMIN_HOSTS=https://relay.example.test"}, "POSTWERK_ADMIN_HOSTS"},
		{"admin host with port", []string{"POSTWERK_ADMIN_HOSTS=relay.example.test:8081"}, "POSTWERK_ADMIN_HOSTS"},
		{"tokens with admin off", []string{"POSTWERK_ADMIN_LISTEN=off"}, "POSTWERK_UI_TOKENS_FILE is set but the admin listener is off"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			env := append(minimalEnv(), tt.env...)
			c, err := Load(env, baseFiles(t).read)
			if err == nil {
				t.Fatalf("no error, config %+v", c)
			}
			if c != nil {
				t.Fatal("config returned with error")
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("error %q does not contain %q", err, tt.want)
			}
			for _, secret := range []string{"secretpw", "apisecret", "AAEC", "QEFC"} {
				if strings.Contains(err.Error(), secret) {
					t.Fatalf("error leaks a secret: %q", err)
				}
			}
		})
	}
}

func TestBadCredentialsFormat(t *testing.T) {
	f := baseFiles(t)
	f["/c/nocolon"] = "justtext\n"
	f["/c/emptypw"] = "user:\n"
	f["/c/emptyuser"] = ":pw\n"
	for _, p := range []string{"/c/nocolon", "/c/emptypw", "/c/emptyuser"} {
		env := append(minimalEnv(), "POSTWERK_TARGET_MAILPIT_URL=smtps://x", "POSTWERK_TARGET_MAILPIT_CREDENTIALS_FILE="+p)
		if _, err := Load(env, f.read); err == nil || !strings.Contains(err.Error(), "username:password") {
			t.Errorf("%s: err = %v", p, err)
		}
	}
}

func TestAllErrorsTogether(t *testing.T) {
	env := []string{
		"POSTWERK_MAX_RECIPIENTS=x",
		"POSTWERK_RETRY_WINDOW=x",
		"POSTWERK_TYPO=1",
	}
	_, err := Load(env, files{}.read)
	if err == nil {
		t.Fatal("no error")
	}
	var n int
	if j, ok := err.(interface{ Unwrap() []error }); ok {
		n = len(j.Unwrap())
	}
	// recipients, window, senders, TLS rule, store key, hash key, UI tokens, targets, typo
	if n != 9 {
		t.Fatalf("got %d errors, want 9:\n%v", n, err)
	}
	if !errors.Is(err, err) {
		t.Fatal("unreachable")
	}
}

func TestAllowed(t *testing.T) {
	tg := Target{AllowRecipients: []string{"*@example.test", "qa-*@corp.test"}}
	tests := []struct {
		rcpts []string
		want  bool
	}{
		{[]string{"a@example.test"}, true},
		{[]string{"A@EXAMPLE.TEST", "qa-1@corp.test"}, true},
		{[]string{"a@example.test", "real@customer.test"}, false},
		{[]string{"a@sub.example.test"}, false},
		{[]string{"qa@corp.test"}, false},
	}
	for _, tt := range tests {
		if got := tg.Allowed(tt.rcpts); got != tt.want {
			t.Errorf("Allowed(%q) = %v", tt.rcpts, got)
		}
	}
	if !(Target{}).Allowed([]string{"anyone@anywhere.test"}) {
		t.Error("target without list must allow all")
	}
}

func TestRetentionZeroAndOff(t *testing.T) {
	f := baseFiles(t)
	env := append(minimalEnv(),
		"POSTWERK_STORE_KEY_FILE=", "POSTWERK_STORE_ENCRYPTION=off",
		"POSTWERK_HASH_KEY_FILE=", "POSTWERK_PSEUDONYMIZE_AFTER=0",
		"POSTWERK_RAW_KEEP_AFTER_FAILURE=0s", "POSTWERK_SECURE_DELETE=on")
	c, err := Load(env, f.read)
	if err != nil {
		t.Fatal(err)
	}
	if c.StoreKeys != nil || c.HashKeys != nil || c.PseudonymizeAfter != 0 || c.RawKeepAfterFailure != 0 {
		t.Fatalf("config = %+v", c)
	}
	w := strings.Join(c.Warnings(), "\n")
	if !strings.Contains(w, "POSTWERK_STORE_ENCRYPTION=off") || !strings.Contains(w, "POSTWERK_PSEUDONYMIZE_AFTER=0") {
		t.Fatalf("warnings = %s", w)
	}
}

func TestParseDuration(t *testing.T) {
	for in, want := range map[string]time.Duration{
		"0": 0, "90s": 90 * time.Second, "7d": 7 * Day, "1d12h": 36 * time.Hour, "0d": 0, "30d": 30 * Day,
	} {
		if got, err := ParseDuration(in); err != nil || got != want {
			t.Errorf("ParseDuration(%q) = %v, %v; want %v", in, got, err, want)
		}
	}
	for _, in := range []string{"d", "1.5d", "1d-1h", "1dd", "x"} {
		if _, err := ParseDuration(in); err == nil {
			t.Errorf("ParseDuration(%q) accepted", in)
		}
	}
}

// A number of days that does not fit into a time.Duration must be an error,
// not wrap around to a short period: 213504d would otherwise be about 25
// minutes and pass every range check.
func TestParseDurationOverflow(t *testing.T) {
	for _, in := range []string{"213504d", "999999d", "106751d23h59m59s", "106751d300h"} {
		if d, err := ParseDuration(in); err == nil {
			t.Errorf("ParseDuration(%q) = %v, want an error", in, d)
		}
	}
	if d, err := ParseDuration("106751d"); err != nil || d != 106751*Day {
		t.Errorf("ParseDuration(106751d) = %v, %v", d, err)
	}
	env := append(minimalEnv(), "POSTWERK_RAW_MAX_AGE=213504d")
	if _, err := Load(env, baseFiles(t).read); err == nil || !strings.Contains(err.Error(), "POSTWERK_RAW_MAX_AGE: expected") {
		t.Fatalf("overflowing duration accepted: %v", err)
	}
}

// The shutdown deadline must cover the longest delivery attempt
// (delivery.DefaultAttemptTimeout, 90 s, checked in cmd/postwerk) with room
// to spare, and the orchestrator grace period of 120 s must cover it.
func TestShutdownDefaultFitsGracePeriod(t *testing.T) {
	if DefaultShutdownTimeout <= 90*time.Second || DefaultShutdownTimeout >= 120*time.Second {
		t.Fatalf("DefaultShutdownTimeout = %v", DefaultShutdownTimeout)
	}
}

func TestKeyFileErrorsDoNotLeak(t *testing.T) {
	f := baseFiles(t)
	f["/k"] = "BAD_ID:" + strings.Repeat("A", 44) + "\nok:c2hvcnQ=\nnocolon\n"
	env := append(minimalEnv(), "POSTWERK_STORE_KEY_FILE=/k")
	_, err := Load(env, f.read)
	if err == nil {
		t.Fatal("no error")
	}
	for _, want := range []string{"line 1: key id", "line 2: key must be 32 bytes", "line 3: expected"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("missing %q in %v", want, err)
		}
	}
	if strings.Contains(err.Error(), "c2hvcnQ") || strings.Contains(err.Error(), "AAAA") {
		t.Fatalf("error quotes key material: %v", err)
	}
	if _, err := keys.Parse([]byte(storeKeyLine + "\n")); err != nil {
		t.Fatal(err)
	}
}

func TestAdminListener(t *testing.T) {
	load := func(extra ...string) *Config {
		t.Helper()
		c, err := Load(append(minimalEnv(), extra...), baseFiles(t).read)
		if err != nil {
			t.Fatal(err)
		}
		return c
	}
	for _, addr := range []string{"127.0.0.1:9000", "127.0.0.2:8081", "[::1]:8081", "localhost:8081", "LOCALHOST:0"} {
		if c := load("POSTWERK_ADMIN_LISTEN=" + addr); c.Admin.Listen != addr || c.Admin.NonLoopback {
			t.Errorf("%s: %+v", addr, c.Admin)
		}
	}
	// Off: empty or "off", and then nothing else of the UI may be set.
	for _, v := range []string{"", "off"} {
		c := load("POSTWERK_ADMIN_LISTEN="+v, "POSTWERK_UI_TOKENS_FILE=")
		if c.Admin.Listen != "" || c.Admin.Tokens != nil {
			t.Errorf("%q: admin = %+v", v, c.Admin)
		}
		if _, ok := c.SecretFiles["POSTWERK_UI_TOKENS_FILE"]; ok {
			t.Errorf("%q: tokens file read while off", v)
		}
	}
	// The explicit exception for Docker port publishing, with a warning.
	c := load("POSTWERK_ADMIN_LISTEN=:8081", "POSTWERK_ADMIN_ALLOW_NON_LOOPBACK=true",
		"POSTWERK_ADMIN_HOSTS=Relay.Example.test, 192.0.2.7")
	if c.Admin.Listen != ":8081" || !c.Admin.NonLoopback || !reflect.DeepEqual(c.Admin.Hosts, []string{"relay.example.test", "192.0.2.7"}) {
		t.Fatalf("admin = %+v", c.Admin)
	}
	if w := c.Warnings(); len(w) != 1 || !strings.Contains(w[0], "POSTWERK_ADMIN_LISTEN is not a loopback address") {
		t.Fatalf("warnings = %q", w)
	}
	// Allowed but loopback anyway: no warning.
	if c := load("POSTWERK_ADMIN_ALLOW_NON_LOOPBACK=true"); c.Admin.NonLoopback || len(c.Warnings()) != 0 {
		t.Fatalf("loopback with allow: %+v %q", c.Admin, c.Warnings())
	}
}
