package config

import (
	"strings"
	"testing"

	"github.com/eneuberg/postwerk/internal/auth"
	"github.com/eneuberg/postwerk/internal/testutil"
)

func TestAPIListener(t *testing.T) {
	f := baseFiles(t)
	f["/run/secrets/api-tokens"] = tokenLine() + "\n"
	load := func(extra ...string) (*Config, error) {
		t.Helper()
		return Load(append(minimalEnv(), extra...), f.read)
	}
	// Off by default; nothing of the API may be set then.
	c, err := load()
	if err != nil || c.API.Listen != "" || c.API.Tokens != nil {
		t.Fatalf("default: %+v %v", c.API, err)
	}
	for _, v := range []string{"POSTWERK_API_TOKENS_FILE=/run/secrets/api-tokens", "POSTWERK_API_HOSTS=x.example.test",
		"POSTWERK_API_TLS_CERT_FILE=/c"} {
		if _, err := load("POSTWERK_API_LISTEN=off", v); err == nil || !strings.Contains(err.Error(), "the API is off") {
			t.Errorf("%s while off: %v", v, err)
		}
	}
	// A pod address is allowed, with a warning without TLS.
	c, err = load("POSTWERK_API_LISTEN=:8082", "POSTWERK_API_TOKENS_FILE=/run/secrets/api-tokens",
		"POSTWERK_API_HOSTS=Postwerk.Mail.svc, 192.0.2.7")
	if err != nil {
		t.Fatal(err)
	}
	if c.API.Listen != ":8082" || len(c.API.Hosts) != 2 || c.API.Hosts[0] != "postwerk.mail.svc" ||
		c.SecretFiles["POSTWERK_API_TOKENS_FILE"] != "/run/secrets/api-tokens" {
		t.Fatalf("api = %+v", c.API)
	}
	if role, ok := c.API.Tokens.Verify("ops", "ui-token"); !ok || role != auth.RoleAdmin {
		t.Fatal("api token does not verify")
	}
	if w := c.Warnings(); len(w) != 1 || !strings.Contains(w[0], "POSTWERK_API_LISTEN is not a loopback address and the API has no TLS") {
		t.Fatalf("warnings = %q", w)
	}
	// Loopback: no warning.
	if c, err := load("POSTWERK_API_LISTEN=127.0.0.1:8082", "POSTWERK_API_TOKENS_FILE=/run/secrets/api-tokens"); err != nil || len(c.Warnings()) != 0 {
		t.Fatalf("loopback: %v %q", err, c.Warnings())
	}
	// TLS: a valid pair, no warning.
	cert := testutil.NewCert(t)
	f["/c"], f["/k"] = string(cert.CertPEM), string(cert.KeyPEM)
	c, err = load("POSTWERK_API_LISTEN=:8082", "POSTWERK_API_TOKENS_FILE=/run/secrets/api-tokens",
		"POSTWERK_API_TLS_CERT_FILE=/c", "POSTWERK_API_TLS_KEY_FILE=/k")
	if err != nil || c.API.TLSCertFile != "/c" || c.API.TLSKeyFile != "/k" || len(c.Warnings()) != 0 {
		t.Fatalf("tls: %+v %v %q", c.API, err, c.Warnings())
	}

	for _, tc := range []struct {
		env  []string
		want string
	}{
		{[]string{"POSTWERK_API_LISTEN=:8082"}, "POSTWERK_API_TOKENS_FILE: required while the API is on"},
		{[]string{"POSTWERK_API_LISTEN=8082", "POSTWERK_API_TOKENS_FILE=/run/secrets/api-tokens"}, "POSTWERK_API_LISTEN: expected host:port"},
		{[]string{"POSTWERK_API_LISTEN=127.0.0.1:8081", "POSTWERK_API_TOKENS_FILE=/run/secrets/api-tokens"}, "must differ"},
		{[]string{"POSTWERK_API_LISTEN=:8082", "POSTWERK_API_TOKENS_FILE=/run/secrets/senders"}, "POSTWERK_API_TOKENS_FILE: line 1"},
		{[]string{"POSTWERK_API_LISTEN=:8082", "POSTWERK_API_TOKENS_FILE=/nope"}, "POSTWERK_API_TOKENS_FILE: cannot read"},
		{[]string{"POSTWERK_API_LISTEN=:8082", "POSTWERK_API_TOKENS_FILE=/run/secrets/api-tokens", "POSTWERK_API_HOSTS=a:80"}, "not a host name"},
		{[]string{"POSTWERK_API_LISTEN=:8082", "POSTWERK_API_TOKENS_FILE=/run/secrets/api-tokens", "POSTWERK_API_TLS_CERT_FILE=/c"}, "must be set together"},
		{[]string{"POSTWERK_API_LISTEN=:8082", "POSTWERK_API_TOKENS_FILE=/run/secrets/api-tokens",
			"POSTWERK_API_TLS_CERT_FILE=/c", "POSTWERK_API_TLS_KEY_FILE=/run/secrets/hash-key"}, "not a valid PEM"},
	} {
		_, err := load(tc.env...)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%q: %v, want %q", tc.env, err, tc.want)
		}
	}
}
