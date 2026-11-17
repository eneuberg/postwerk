package config

import (
	"crypto/x509"
	"encoding/pem"
	"time"
)

// Probing a target that is down (D45).
const (
	envProbeInterval    = "POSTWERK_PROBE_INTERVAL"
	envProbeMaxInterval = "POSTWERK_PROBE_MAX_INTERVAL"
	envDownAfterProbes  = "POSTWERK_TARGET_DOWN_AFTER_PROBES"

	// DefaultProbeInterval is the wait before the first probe of a target
	// that went down; each failed probe doubles it.
	DefaultProbeInterval = 30 * time.Second
	// DefaultProbeMaxInterval caps the wait between probes. With a
	// provider that locks an account after repeated failed logins, a
	// rejected login is retried at most every 5 minutes.
	DefaultProbeMaxInterval = 5 * time.Minute
	// DefaultDownAfterProbes is how many probes of an unreachable target
	// must fail before target.down is written (D58). A rejected login
	// (535) is reported at once.
	DefaultDownAfterProbes = 1
	maxDownAfterProbes     = 10
)

var probeVars = []string{envProbeInterval, envProbeMaxInterval, envDownAfterProbes}

func (l *loader) probe(c *Config) {
	c.ProbeInterval = l.duration(envProbeInterval, DefaultProbeInterval, false)
	c.ProbeMaxInterval = l.duration(envProbeMaxInterval, DefaultProbeMaxInterval, false)
	if c.ProbeInterval > 0 && c.ProbeMaxInterval > 0 && c.ProbeMaxInterval < c.ProbeInterval {
		l.errorf("%s must be at least %s", envProbeMaxInterval, envProbeInterval)
	}
	c.DownAfterProbes = l.integer(envDownAfterProbes, DefaultDownAfterProbes, 0, maxDownAfterProbes)
}

// maxCAFile bounds a CA bundle; a system bundle is about 200 KiB.
const maxCAFile = 1 << 20

// caFile reads a PEM bundle of CA certificates for one target (D48). It is
// not a secret, so it is not listed for the permission check. Every PEM
// block must be a certificate that parses, and there must be at least one.
func (l *loader) caFile(varName, p string) *x509.CertPool {
	b, err := l.readFile(p)
	if err != nil {
		l.errorf("%s: cannot read file: %v", varName, err)
		return nil
	}
	if len(b) > maxCAFile {
		l.errorf("%s: file larger than %d bytes", varName, maxCAFile)
		return nil
	}
	pool := x509.NewCertPool()
	n := 0
	for {
		var block *pem.Block
		block, b = pem.Decode(b)
		if block == nil {
			break
		}
		if block.Type != "CERTIFICATE" {
			l.errorf("%s: PEM block %q is not a certificate", varName, block.Type)
			return nil
		}
		cert, err := x509.ParseCertificate(block.Bytes)
		if err != nil {
			l.errorf("%s: certificate %d does not parse: %v", varName, n+1, err)
			return nil
		}
		pool.AddCert(cert)
		n++
	}
	if n == 0 {
		l.errorf("%s: no PEM certificate in the file", varName)
		return nil
	}
	return pool
}
