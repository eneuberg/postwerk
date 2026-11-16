package config

import (
	"bytes"
	"crypto/tls"
	"net"
	"strings"

	"github.com/eneuberg/postwerk/internal/auth"
)

// API configures the JSON query API listener (H2, H4–H9).
type API struct {
	// Listen is the address; empty when the API is off (the default).
	Listen string
	// Hosts, if not empty, are the only Host names the API answers to
	// (lower case, no port).
	Hosts []string
	// Tokens are the API's bearer tokens; set while Listen is not empty.
	Tokens *auth.APITokens
	// TLSCertFile and TLSKeyFile name the certificate pair; empty: plain
	// HTTP. The pair was checked at start and is reloaded while running.
	TLSCertFile, TLSKeyFile string
}

const (
	envAPIListen     = "POSTWERK_API_LISTEN"
	envAPIHosts      = "POSTWERK_API_HOSTS"
	envAPITokensFile = "POSTWERK_API_TOKENS_FILE"
	envAPITLSCert    = "POSTWERK_API_TLS_CERT_FILE"
	envAPITLSKey     = "POSTWERK_API_TLS_KEY_FILE"
)

var apiVars = []string{envAPIListen, envAPIHosts, envAPITokensFile, envAPITLSCert, envAPITLSKey}

// api reads the API listener and its tokens (H4, H5). It is off unless
// POSTWERK_API_LISTEN names an address. Unlike the admin listener it may bind
// a pod address: another pod calls it.
func (l *loader) api(c *Config) {
	listen := l.env[envAPIListen]
	if listen == "" || listen == "off" {
		for _, name := range []string{envAPITokensFile, envAPIHosts, envAPITLSCert, envAPITLSKey} {
			if l.env[name] != "" {
				l.errorf("%s is set but the API is off (%s=%s); remove one of them", name, envAPIListen, listen)
			}
		}
		return
	}
	c.API.Listen = l.listen(envAPIListen, listen)
	if _, port, _ := net.SplitHostPort(c.API.Listen); port != "0" &&
		(c.API.Listen == c.HTTPListen || c.API.Listen == c.SMTPListen || c.API.Listen == c.Admin.Listen) {
		l.errorf("%s: must differ from %s, %s and %s", envAPIListen, envHTTPListen, envSMTPListen, envAdminListen)
	}
	if v := l.env[envAPIHosts]; v != "" {
		for _, h := range strings.Split(v, ",") {
			h = strings.ToLower(strings.TrimSpace(h))
			if h == "" || len(h) > 253 || (!hostnameRe.MatchString(h) && net.ParseIP(h) == nil) {
				l.errorf("%s: %q is not a host name or IP address (no port, no scheme)", envAPIHosts, h)
				continue
			}
			c.API.Hosts = append(c.API.Hosts, h)
		}
	}
	l.apiTLS(c)
	if l.env[envAPITokensFile] == "" {
		l.errorf("%s: required while the API is on (one name:role:sha256$<digest> line per calling application; "+
			"create one with 'postwerk gen-api-token <name> <read|admin>'), or set %s=off",
			envAPITokensFile, envAPIListen)
		return
	}
	b, ok := l.file(envAPITokensFile)
	if !ok {
		return
	}
	t, err := auth.ParseAPITokens(bytes.NewReader(b))
	if err != nil {
		for _, e := range unjoin(err) {
			l.errorf("%s: %v", envAPITokensFile, e)
		}
		return
	}
	c.API.Tokens = t
}

// apiTLS checks the API's certificate pair once at start (H9); the
// listener then reloads it while running (internal/tlsreload).
func (l *loader) apiTLS(c *Config) {
	certSet, keySet := l.env[envAPITLSCert] != "", l.env[envAPITLSKey] != ""
	if !certSet && !keySet {
		return
	}
	if certSet != keySet {
		l.errorf("%s and %s must be set together", envAPITLSCert, envAPITLSKey)
		return
	}
	certPEM, ok1 := l.file(envAPITLSCert)
	keyPEM, ok2 := l.file(envAPITLSKey)
	if !ok1 || !ok2 {
		return
	}
	if _, err := tls.X509KeyPair(certPEM, keyPEM); err != nil {
		l.errorf("%s, %s: not a valid PEM certificate and key pair", envAPITLSCert, envAPITLSKey)
		return
	}
	c.API.TLSCertFile, c.API.TLSKeyFile = l.env[envAPITLSCert], l.env[envAPITLSKey]
}

// apiWarnings: a non-loopback API without TLS carries bearer tokens and
// addresses in cleartext over the pod network.
func (c *Config) apiWarnings() []string {
	if c.API.Listen == "" || c.API.TLSCertFile != "" {
		return nil
	}
	if host, _, err := net.SplitHostPort(c.API.Listen); err == nil && !loopbackHost(host) {
		return []string{envAPIListen + " is not a loopback address and the API has no TLS: bearer tokens and " +
			"addresses cross the network in cleartext; set " + envAPITLSCert + " and " + envAPITLSKey +
			", or use it only on a network that encrypts traffic between nodes"}
	}
	return nil
}
