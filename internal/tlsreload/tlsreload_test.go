package tlsreload

import (
	"bytes"
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/eneuberg/postwerk/internal/testutil"
)

// secretDir lays out files the way a Kubernetes Secret volume does: the
// data in a timestamped directory, "..data" a symlink to it, and each key a
// symlink through "..data". An update writes a new directory and swaps
// "..data" with one rename.
type secretDir struct {
	t   *testing.T
	dir string
	n   int
}

func newSecretDir(t *testing.T, c testutil.Cert) *secretDir {
	s := &secretDir{t: t, dir: t.TempDir()}
	s.swap(c.CertPEM, c.KeyPEM)
	for _, name := range []string{"tls.crt", "tls.key"} {
		if err := os.Symlink(filepath.Join("..data", name), filepath.Join(s.dir, name)); err != nil {
			t.Fatal(err)
		}
	}
	return s
}

func (s *secretDir) swap(certPEM, keyPEM []byte) {
	s.t.Helper()
	s.n++
	ts := fmt.Sprintf("..2026_11_16_%02d", s.n)
	if err := os.Mkdir(filepath.Join(s.dir, ts), 0o755); err != nil {
		s.t.Fatal(err)
	}
	for name, b := range map[string][]byte{"tls.crt": certPEM, "tls.key": keyPEM} {
		if err := os.WriteFile(filepath.Join(s.dir, ts, name), b, 0o600); err != nil {
			s.t.Fatal(err)
		}
	}
	tmp := filepath.Join(s.dir, "..data_tmp")
	if err := os.Symlink(ts, tmp); err != nil {
		s.t.Fatal(err)
	}
	if err := os.Rename(tmp, filepath.Join(s.dir, "..data")); err != nil {
		s.t.Fatal(err)
	}
}

func (s *secretDir) cert() string { return filepath.Join(s.dir, "tls.crt") }
func (s *secretDir) key() string  { return filepath.Join(s.dir, "tls.key") }

// newCert returns a certificate valid for 90 days, past the expiry warning.
func newCert(t *testing.T) testutil.Cert {
	return testutil.NewCertUntil(t, time.Now().Add(90*24*time.Hour))
}

type recorder struct {
	mu     sync.Mutex
	events []Event
}

func (r *recorder) add(e Event) {
	r.mu.Lock()
	r.events = append(r.events, e)
	r.mu.Unlock()
}

func (r *recorder) kinds() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	var k []string
	for _, e := range r.events {
		k = append(k, e.Kind)
	}
	return k
}

func (r *recorder) last() Event {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.events[len(r.events)-1]
}

func newReloader(t *testing.T, s *secretDir, rec *recorder, mod func(*Options)) *Reloader {
	t.Helper()
	opts := Options{Name: "smtp", CertFile: s.cert(), KeyFile: s.key(), Settle: 10 * time.Millisecond, OnEvent: rec.add}
	if mod != nil {
		mod(&opts)
	}
	r, err := New(opts)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func served(t *testing.T, r *Reloader) []byte {
	t.Helper()
	c, err := r.GetCertificate(nil)
	if err != nil || c == nil {
		t.Fatalf("GetCertificate: %v, %v", c, err)
	}
	return c.Certificate[0]
}

func TestLoadAndUnchanged(t *testing.T) {
	c := newCert(t)
	s := newSecretDir(t, c)
	rec := &recorder{}
	r := newReloader(t, s, rec, nil)
	if !bytes.Equal(served(t, r), c.TLS.Certificate[0]) {
		t.Fatal("wrong certificate served")
	}
	if r.Check(context.Background()) {
		t.Fatal("Check reported a reload for unchanged files")
	}
	if k := rec.kinds(); len(k) != 0 {
		t.Fatalf("events for an unchanged pair: %v", k)
	}
}

func TestSymlinkSwap(t *testing.T) {
	old, next := newCert(t), newCert(t)
	s := newSecretDir(t, old)
	rec := &recorder{}
	r := newReloader(t, s, rec, nil)

	s.swap(next.CertPEM, next.KeyPEM)
	if !r.Check(context.Background()) {
		t.Fatal("swap not picked up")
	}
	if !bytes.Equal(served(t, r), next.TLS.Certificate[0]) {
		t.Fatal("old certificate still served after the swap")
	}
	if k := rec.kinds(); len(k) != 1 || k[0] != KindReloaded {
		t.Fatalf("events = %v, want [%s]", k, KindReloaded)
	}
	if e := rec.last(); e.Level != Info || !bytes.Contains([]byte(e.Text), []byte("smtp: certificate reloaded")) {
		t.Fatalf("event = %+v", e)
	}
	// The same content again is no reload.
	if r.Check(context.Background()) {
		t.Fatal("second Check reloaded again")
	}
}

func TestInvalidNewPairKeepsOld(t *testing.T) {
	old, other := newCert(t), newCert(t)
	expired := testutil.NewCertUntil(t, time.Now().Add(-time.Hour))
	for name, pair := range map[string][2][]byte{
		"key of another certificate": {other.CertPEM, old.KeyPEM},
		"garbage":                    {[]byte("not pem"), []byte("not pem")},
		"empty":                      {nil, nil},
		"expired":                    {expired.CertPEM, expired.KeyPEM},
	} {
		t.Run(name, func(t *testing.T) {
			s := newSecretDir(t, old)
			rec := &recorder{}
			r := newReloader(t, s, rec, nil)
			s.swap(pair[0], pair[1])
			if r.Check(context.Background()) {
				t.Fatal("invalid pair loaded")
			}
			if !bytes.Equal(served(t, r), old.TLS.Certificate[0]) {
				t.Fatal("old certificate not kept")
			}
			if k := rec.kinds(); len(k) != 1 || k[0] != KindReloadFailed {
				t.Fatalf("events = %v, want [%s]", k, KindReloadFailed)
			}
			if e := rec.last(); e.Level != Error {
				t.Fatalf("level = %s", e.Level)
			}
			// Reported once per content, not on every check.
			r.Check(context.Background())
			if k := rec.kinds(); len(k) != 1 {
				t.Fatalf("failure reported again: %v", k)
			}
			// A valid pair afterwards is loaded.
			s.swap(other.CertPEM, other.KeyPEM)
			if !r.Check(context.Background()) || !bytes.Equal(served(t, r), other.TLS.Certificate[0]) {
				t.Fatal("valid pair after a failure not loaded")
			}
			if k := rec.kinds(); k[len(k)-1] != KindReloaded {
				t.Fatalf("events = %v", k)
			}
		})
	}
}

func TestMissingFileKeepsOld(t *testing.T) {
	old := newCert(t)
	s := newSecretDir(t, old)
	rec := &recorder{}
	r := newReloader(t, s, rec, nil)
	if err := os.Remove(filepath.Join(s.dir, "..data")); err != nil {
		t.Fatal(err)
	}
	if r.Check(context.Background()) || !bytes.Equal(served(t, r), old.TLS.Certificate[0]) {
		t.Fatal("old certificate not kept")
	}
	if k := rec.kinds(); len(k) != 1 || k[0] != KindReloadFailed {
		t.Fatalf("events = %v", k)
	}
}

// A check that reads the new certificate with the old key (the files
// changed between the two reads) reads again after Settle and does not
// report a failure.
func TestSettleOverHalfWrittenPair(t *testing.T) {
	old, next := newCert(t), newCert(t)
	dir := t.TempDir()
	certFile, keyFile := filepath.Join(dir, "tls.crt"), filepath.Join(dir, "tls.key")
	write := func(name string, b []byte) {
		if err := os.WriteFile(name, b, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write(certFile, old.CertPEM)
	write(keyFile, old.KeyPEM)
	rec := &recorder{}
	r, err := New(Options{Name: "smtp", CertFile: certFile, KeyFile: keyFile, Settle: 500 * time.Millisecond, OnEvent: rec.add})
	if err != nil {
		t.Fatal(err)
	}
	write(certFile, next.CertPEM)
	done := make(chan bool)
	go func() { done <- r.Check(context.Background()) }()
	time.Sleep(50 * time.Millisecond)
	write(keyFile, next.KeyPEM)
	if !<-done {
		t.Fatal("pair not loaded after it settled")
	}
	if k := rec.kinds(); len(k) != 1 || k[0] != KindReloaded {
		t.Fatalf("events = %v, want only %s", k, KindReloaded)
	}
}

func TestNewRefusesInvalidPair(t *testing.T) {
	a, b := newCert(t), newCert(t)
	s := newSecretDir(t, a)
	s.swap(a.CertPEM, b.KeyPEM)
	if _, err := New(Options{Name: "smtp", CertFile: s.cert(), KeyFile: s.key()}); err == nil {
		t.Fatal("New accepted a mismatched pair")
	}
	if _, err := New(Options{Name: "smtp", CertFile: s.cert()}); err == nil {
		t.Fatal("New accepted a missing key file name")
	}
}

func TestExpiryWarning(t *testing.T) {
	c := testutil.NewCertUntil(t, time.Now().Add(10*24*time.Hour))
	s := newSecretDir(t, c)
	rec := &recorder{}
	var now atomic.Int64
	now.Store(time.Now().UnixNano())
	r := newReloader(t, s, rec, func(o *Options) { o.Now = func() time.Time { return time.Unix(0, now.Load()) } })
	if k := rec.kinds(); len(k) != 1 || k[0] != KindExpiring || rec.last().Level != Warn {
		t.Fatalf("events at start = %v", k)
	}
	if txt := rec.last().Text; !strings.Contains(txt, "expires in 9 days 23 hours") {
		t.Fatalf("text at start = %q", txt)
	}
	r.Check(context.Background())
	if k := rec.kinds(); len(k) != 1 {
		t.Fatalf("expiry warning repeated within a day: %v", k)
	}
	now.Add(int64(25 * time.Hour))
	r.Check(context.Background())
	if k := rec.kinds(); len(k) != 2 || k[1] != KindExpiring {
		t.Fatalf("expiry warning not repeated after a day: %v", k)
	}
	// Past expiry it is an error.
	now.Add(int64(10 * 24 * time.Hour))
	r.Check(context.Background())
	if e := rec.last(); e.Kind != KindExpiring || e.Level != Error {
		t.Fatalf("event after expiry = %+v", e)
	}
	// A fresh certificate stops the warnings.
	fresh := testutil.NewCertUntil(t, time.Unix(0, now.Load()).Add(90*24*time.Hour))
	s.swap(fresh.CertPEM, fresh.KeyPEM)
	n := len(rec.kinds())
	if !r.Check(context.Background()) {
		t.Fatal("fresh certificate not loaded")
	}
	if k := rec.kinds(); len(k) != n+1 || k[n] != KindReloaded {
		t.Fatalf("events after the fresh certificate = %v", k[n:])
	}
}

// TestExpiryEscalates: below 7 days the report is an error at once, even
// if a warning went out less than a day ago, so an error-only filter sees
// it a week ahead (D51).
func TestExpiryEscalates(t *testing.T) {
	start := time.Now()
	c := testutil.NewCertUntil(t, start.Add(7*24*time.Hour+time.Hour))
	s := newSecretDir(t, c)
	rec := &recorder{}
	var now atomic.Int64
	now.Store(start.UnixNano())
	r := newReloader(t, s, rec, func(o *Options) { o.Now = func() time.Time { return time.Unix(0, now.Load()) } })
	if e := rec.last(); len(rec.kinds()) != 1 || e.Level != Warn {
		t.Fatalf("event at start = %+v", e)
	}
	now.Add(int64(30 * time.Minute))
	r.Check(context.Background())
	if len(rec.kinds()) != 1 {
		t.Fatalf("still a warning, repeated within a day: %v", rec.kinds())
	}
	now.Add(int64(time.Hour))
	r.Check(context.Background())
	if e := rec.last(); len(rec.kinds()) != 2 || e.Level != Error || !strings.Contains(e.Text, "expires in 6 days 23 hours") {
		t.Fatalf("event below 7 days = %+v (%d events)", e, len(rec.kinds()))
	}
	now.Add(int64(time.Hour))
	r.Check(context.Background())
	if len(rec.kinds()) != 2 {
		t.Fatalf("error repeated within a day: %v", rec.kinds())
	}
}

func TestRemaining(t *testing.T) {
	for _, c := range []struct {
		d    time.Duration
		want string
	}{
		{9*24*time.Hour + 23*time.Hour + 59*time.Minute, "9 days 23 hours"},
		{14 * 24 * time.Hour, "14 days"},
		{2*24*time.Hour + time.Hour, "2 days 1 hour"},
		{47*time.Hour + 59*time.Minute, "47 hours"},
		{time.Hour + 30*time.Minute, "1 hour"},
		{59 * time.Minute, "59 minutes"},
		{90 * time.Second, "1 minute"},
	} {
		if got := Remaining(c.d); got != c.want {
			t.Errorf("Remaining(%s) = %q, want %q", c.d, got, c.want)
		}
	}
}

func TestRunPicksUpSwap(t *testing.T) {
	old, next := newCert(t), newCert(t)
	s := newSecretDir(t, old)
	rec := &recorder{}
	r := newReloader(t, s, rec, func(o *Options) { o.Interval = 10 * time.Millisecond })
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { r.Run(ctx); close(done) }()
	s.swap(next.CertPEM, next.KeyPEM)
	testutil.Eventually(t, 5*time.Second, "reload by Run", func() bool {
		return bytes.Equal(r.Certificate().Certificate[0], next.TLS.Certificate[0])
	})
	cancel()
	<-done
}

// Handshakes run while the pair is swapped back and forth; each one gets
// one of the two certificates, complete. Run with -race.
func TestConcurrentHandshakesDuringSwaps(t *testing.T) {
	a, b := newCert(t), newCert(t)
	s := newSecretDir(t, a)
	r := newReloader(t, s, &recorder{}, nil)
	pool := a.Pool.Clone()
	pool.AppendCertsFromPEM(b.CertPEM)

	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	srvCfg := &tls.Config{GetCertificate: r.GetCertificate, MinVersion: tls.VersionTLS12}
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				tc := tls.Server(c, srvCfg)
				if tc.Handshake() == nil {
					_, _ = io.WriteString(tc, "ok")
				}
			}()
		}
	}()

	ctx, cancel := context.WithCancel(context.Background())
	swapped := make(chan struct{})
	go func() {
		defer close(swapped)
		for i := 0; ctx.Err() == nil; i++ {
			if i%2 == 0 {
				s.swap(b.CertPEM, b.KeyPEM)
			} else {
				s.swap(a.CertPEM, a.KeyPEM)
			}
			r.Check(ctx)
		}
	}()
	var wg sync.WaitGroup
	errs := make(chan error, 64)
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 10 {
				c, err := tls.Dial("tcp", l.Addr().String(), &tls.Config{RootCAs: pool, ServerName: "localhost"})
				if err != nil {
					errs <- err
					return
				}
				buf := make([]byte, 2)
				_, err = io.ReadFull(c, buf)
				c.Close()
				if err != nil {
					errs <- err
					return
				}
			}
		}()
	}
	wg.Wait()
	cancel()
	<-swapped
	close(errs)
	for err := range errs {
		t.Error(err)
	}
}
