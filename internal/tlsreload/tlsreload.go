// Package tlsreload serves a TLS certificate from a certificate and a key
// file and picks up a new pair without a restart (I16).
//
// cert-manager renews a certificate long before it expires, and Kubernetes
// swaps the files of a Secret volume with an atomic symlink change. A
// Reloader re-reads both files at a fixed interval and compares their
// content (not their modification time, which a symlink swap does not
// change). A changed pair is validated first: it must parse, the key must
// match the certificate and the certificate must not have expired. Only then
// is it served; otherwise the old certificate stays and the failure is
// reported once per content. A certificate close to its expiry is reported
// once a day.
//
// Use it in a tls.Config:
//
//	r, err := tlsreload.New(tlsreload.Options{Name: "smtp", CertFile: c, KeyFile: k, Logger: log})
//	go r.Run(ctx)
//	cfg := &tls.Config{GetCertificate: r.GetCertificate, MinVersion: tls.VersionTLS12}
package tlsreload

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Event kinds. They are the same strings as in Postwerk's event log.
const (
	KindReloaded     = "tls.reloaded"
	KindReloadFailed = "tls.reload_failed"
	KindExpiring     = "tls.expiring"
)

// Defaults.
const (
	DefaultInterval      = time.Minute
	DefaultExpiryWarning = 14 * 24 * time.Hour
	// DefaultExpiryError: from here on the expiry report is an error, so
	// it reaches alerting that only takes errors (D52).
	DefaultExpiryError = 7 * 24 * time.Hour
	// DefaultSettle is how long a check waits before it reads a changed
	// pair that failed once a second time. It covers a read between the
	// certificate file and the key file while they are being replaced.
	DefaultSettle = 2 * time.Second
	// expiryRepeat is how often an expiry warning repeats.
	expiryRepeat = 24 * time.Hour
	// maxFile bounds what is read from each file.
	maxFile = 1 << 20
)

// Level of an Event.
type Level string

// Event levels, the same strings as in Postwerk's event log.
const (
	Info  Level = "info"
	Warn  Level = "warn"
	Error Level = "error"
)

// Event is a reload outcome or an expiry warning. Text names the listener
// and the certificate (subject, expiry), never key material.
type Event struct {
	Kind  string
	Level Level
	Text  string
}

// Options configure a Reloader.
type Options struct {
	// Name is the listener, for example "smtp"; it starts every event text.
	Name string
	// CertFile and KeyFile are PEM files. Both are required.
	CertFile, KeyFile string
	// Interval between checks; 0 means DefaultInterval.
	Interval time.Duration
	// ExpiryWarning: a served certificate that expires within this time is
	// reported as KindExpiring; 0 means DefaultExpiryWarning.
	ExpiryWarning time.Duration
	// ExpiryError: within this time the report is an error instead of a
	// warning; 0 means DefaultExpiryError.
	ExpiryError time.Duration
	// Settle: see DefaultSettle; 0 means DefaultSettle.
	Settle time.Duration
	// OnEvent, if set, receives every event; it must not block for long.
	OnEvent func(Event)
	Logger  *slog.Logger
	Now     func() time.Time
}

// Reloader holds the current certificate. It is safe for concurrent use.
type Reloader struct {
	opts Options
	cert atomic.Pointer[tls.Certificate]

	// mu serializes checks; the fields below belong to it.
	mu         sync.Mutex
	digest     [sha256.Size]byte // content of the pair being served
	failed     [sha256.Size]byte // content of the last pair reported as failed
	hasFailed  bool
	lastExpiry time.Time // when the last expiry warning went out
	lastLevel  Level     // its level
}

// New loads the pair and returns a Reloader serving it. It fails if the pair
// cannot be read or is not valid, so a broken pair stops the start.
func New(opts Options) (*Reloader, error) {
	if opts.CertFile == "" || opts.KeyFile == "" {
		return nil, errors.New("tlsreload: CertFile and KeyFile are required")
	}
	if opts.Interval <= 0 {
		opts.Interval = DefaultInterval
	}
	if opts.ExpiryWarning <= 0 {
		opts.ExpiryWarning = DefaultExpiryWarning
	}
	if opts.ExpiryError <= 0 {
		opts.ExpiryError = DefaultExpiryError
	}
	if opts.Settle <= 0 {
		opts.Settle = DefaultSettle
	}
	if opts.Logger == nil {
		opts.Logger = slog.New(slog.DiscardHandler)
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	r := &Reloader{opts: opts}
	cert, digest, err := r.load()
	if err != nil {
		return nil, fmt.Errorf("tlsreload: %s: %w", opts.Name, err)
	}
	r.cert.Store(cert)
	r.digest = digest
	r.mu.Lock()
	r.checkExpiry(cert)
	r.mu.Unlock()
	return r, nil
}

// GetCertificate returns the current certificate; it fits
// tls.Config.GetCertificate.
func (r *Reloader) GetCertificate(*tls.ClientHelloInfo) (*tls.Certificate, error) {
	return r.cert.Load(), nil
}

// Certificate returns the current certificate.
func (r *Reloader) Certificate() *tls.Certificate { return r.cert.Load() }

// Run checks the files every Interval until ctx ends.
func (r *Reloader) Run(ctx context.Context) {
	t := time.NewTicker(r.opts.Interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			r.Check(ctx)
		}
	}
}

// Check reads both files once. A changed, valid pair is served from now on;
// a changed, invalid pair is read once more after Settle and, if it is still
// invalid, reported. It reports whether a new certificate is served.
func (r *Reloader) Check(ctx context.Context) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	cert, digest, err := r.load()
	if err == nil && digest == r.digest {
		r.checkExpiry(r.cert.Load())
		return false
	}
	if err != nil {
		// Possibly caught in the middle of a swap: one more read.
		select {
		case <-ctx.Done():
			return false
		case <-time.After(r.opts.Settle):
		}
		cert, digest, err = r.load()
		if err == nil && digest == r.digest {
			r.checkExpiry(r.cert.Load())
			return false
		}
	}
	if err != nil {
		if !r.hasFailed || digest != r.failed {
			r.hasFailed, r.failed = true, digest
			served := r.cert.Load()
			r.emit(Event{Kind: KindReloadFailed, Level: Error, Text: fmt.Sprintf(
				"%s: new certificate not loaded, still serving the old one (%s): %v",
				r.opts.Name, describe(served), err)})
		}
		r.checkExpiry(r.cert.Load())
		return false
	}
	r.cert.Store(cert)
	r.digest, r.hasFailed = digest, false
	r.lastExpiry, r.lastLevel = time.Time{}, "" // a new certificate gets its own warning
	r.emit(Event{Kind: KindReloaded, Level: Info, Text: fmt.Sprintf(
		"%s: certificate reloaded (%s)", r.opts.Name, describe(cert))})
	r.checkExpiry(cert)
	return true
}

// load reads and validates the pair. The digest covers the content read,
// also when it is invalid, so the same broken pair is reported only once.
func (r *Reloader) load() (*tls.Certificate, [sha256.Size]byte, error) {
	var digest [sha256.Size]byte
	certPEM, err := readFile(r.opts.CertFile)
	if err != nil {
		return nil, digest, fmt.Errorf("certificate file: %w", err)
	}
	keyPEM, err := readFile(r.opts.KeyFile)
	if err != nil {
		return nil, digest, fmt.Errorf("key file: %w", err)
	}
	h := sha256.New()
	h.Write(certPEM)
	h.Write([]byte{0})
	h.Write(keyPEM)
	copy(digest[:], h.Sum(nil))
	cert, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		return nil, digest, errors.New("not a valid PEM certificate and key pair")
	}
	if cert.Leaf == nil {
		if cert.Leaf, err = x509.ParseCertificate(cert.Certificate[0]); err != nil {
			return nil, digest, errors.New("certificate cannot be parsed")
		}
	}
	if !r.opts.Now().Before(cert.Leaf.NotAfter) {
		return nil, digest, fmt.Errorf("certificate expired at %s", cert.Leaf.NotAfter.UTC().Format(time.RFC3339))
	}
	return &cert, digest, nil
}

// checkExpiry reports a served certificate close to its expiry, at most
// once per expiryRepeat, and at once when the report becomes more severe
// (warning to error). r.mu must be held.
func (r *Reloader) checkExpiry(cert *tls.Certificate) {
	if cert == nil || cert.Leaf == nil {
		return
	}
	now := r.opts.Now()
	left := cert.Leaf.NotAfter.Sub(now)
	if left >= r.opts.ExpiryWarning {
		return
	}
	level := Warn
	if left < r.opts.ExpiryError {
		level = Error
	}
	escalated := level == Error && r.lastLevel != Error
	if !r.lastExpiry.IsZero() && now.Sub(r.lastExpiry) < expiryRepeat && !escalated {
		return
	}
	r.lastExpiry, r.lastLevel = now, level
	if left <= 0 {
		r.emit(Event{Kind: KindExpiring, Level: Error, Text: fmt.Sprintf(
			"%s: the served certificate has expired (%s); clients that verify it cannot connect",
			r.opts.Name, describe(cert))})
		return
	}
	r.emit(Event{Kind: KindExpiring, Level: level, Text: fmt.Sprintf(
		"%s: the served certificate expires in %s (%s)", r.opts.Name, Remaining(left), describe(cert))})
}

func (r *Reloader) emit(e Event) {
	level := slog.LevelInfo
	switch e.Level {
	case Warn:
		level = slog.LevelWarn
	case Error:
		level = slog.LevelError
	}
	r.opts.Logger.Log(context.Background(), level, e.Text, "kind", e.Kind, "listener", r.opts.Name)
	if r.opts.OnEvent != nil {
		r.opts.OnEvent(e)
	}
}

// describe names a certificate by subject and expiry.
func describe(cert *tls.Certificate) string {
	if cert == nil || cert.Leaf == nil {
		return "no certificate"
	}
	l := cert.Leaf
	name := l.Subject.CommonName
	if name == "" && len(l.DNSNames) > 0 {
		name = l.DNSNames[0]
	}
	parts := []string{}
	if name != "" {
		parts = append(parts, "subject "+name)
	}
	parts = append(parts, "expires "+l.NotAfter.UTC().Format(time.RFC3339))
	return strings.Join(parts, ", ")
}

// Remaining writes a time left without rounding it up: days and hours from
// two days on ("9 days 23 hours"), hours below that ("47 hours"), minutes
// below an hour. Rounded down, so a report never promises more time than
// there is.
func Remaining(d time.Duration) string {
	switch {
	case d >= 48*time.Hour:
		days, hours := int(d/(24*time.Hour)), int(d%(24*time.Hour)/time.Hour)
		if hours == 0 {
			return fmt.Sprintf("%d days", days)
		}
		return fmt.Sprintf("%d days %d %s", days, hours, plural(hours, "hour"))
	case d >= time.Hour:
		h := int(d / time.Hour)
		return fmt.Sprintf("%d %s", h, plural(h, "hour"))
	default:
		m := int(d / time.Minute)
		return fmt.Sprintf("%d %s", m, plural(m, "minute"))
	}
}

func plural(n int, word string) string {
	if n == 1 {
		return word
	}
	return word + "s"
}

func readFile(name string) ([]byte, error) {
	f, err := os.Open(name)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, maxFile+1))
	if err != nil {
		return nil, err
	}
	if len(b) > maxFile {
		return nil, fmt.Errorf("larger than %d bytes", maxFile)
	}
	return b, nil
}
