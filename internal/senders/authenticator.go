package senders

import (
	"crypto/rand"
	"crypto/subtle"
	"errors"
	"log/slog"
	"runtime"
	"time"
)

// Errors of Authenticate.
var (
	// ErrBadCredentials: wrong key, password or name; the client gets 535.
	ErrBadCredentials = errors.New("senders: authentication failed")
	// ErrBusy: no slot for a password check came free in time; the client
	// gets 454 and may retry (D26).
	ErrBusy = errors.New("senders: too many password checks at once")
)

// Principal is a sender that logged in.
type Principal struct {
	Sender string
	Source Kind
	Method Method
	// KeyID is the key used; empty for a password.
	KeyID  string
	Policy Policy
	// hash is the password line's value at login, so Refresh notices a
	// changed password.
	hash string
}

// passwordChecker is the part of auth.Senders the Authenticator uses.
type passwordChecker interface {
	Cached(name, password string) bool
	Verify(name, password string) bool
}

// lookup is what the Authenticator needs from its sources; *Registry has it.
type lookup interface {
	ByKeyID(id string) (Credential, bool)
	ByName(name string) (Sender, bool)
	Has(name string) bool
	password(name string) (passwordChecker, string)
}

// UsageRecorder is told about every successful login (Usage).
type UsageRecorder interface {
	Record(p Principal, at time.Time)
}

// AuthOptions configure an Authenticator.
type AuthOptions struct {
	// MaxChecks limits concurrent PBKDF2 password checks; 0 means
	// runtime.GOMAXPROCS(0). Key checks need no slot.
	MaxChecks int
	// Wait is how long a password check waits for a slot; 0 means 5 s.
	Wait   time.Duration
	Usage  UsageRecorder // optional
	Logger *slog.Logger
	Now    func() time.Time
}

// Authenticator checks SMTP logins (I2, I12, I13, D62). It is safe for
// concurrent use.
//
// The user name is the sender's name; the password is a key or, for a
// password line in the senders file, the password. A password that has the
// form of a key is checked as a key only.
//
//   - A key costs one SHA-256 and one constant-time comparison with 32
//     bytes, the key's digest or a random dummy for an unknown id, whatever
//     the name. Its time tells nothing about which ids or names exist.
//   - A password costs PBKDF2 at the strongest iteration count of the file,
//     for every name (auth.Senders, D26), unless the same login succeeded
//     before (one HMAC). At most MaxChecks run at once.
type Authenticator struct {
	src   lookup
	opts  AuthOptions
	slots chan struct{}
	dummy Digest
}

// NewAuthenticator returns an Authenticator over r.
func NewAuthenticator(r *Registry, opts AuthOptions) *Authenticator {
	if opts.MaxChecks <= 0 {
		opts.MaxChecks = runtime.GOMAXPROCS(0)
	}
	if opts.Wait <= 0 {
		opts.Wait = 5 * time.Second
	}
	if opts.Logger == nil {
		opts.Logger = slog.New(slog.DiscardHandler)
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	a := &Authenticator{src: r, opts: opts, slots: make(chan struct{}, opts.MaxChecks)}
	_, _ = rand.Read(a.dummy[:])
	return a
}

// Authenticate checks one login. It returns ErrBadCredentials or ErrBusy.
func (a *Authenticator) Authenticate(name, password string) (Principal, error) {
	var p Principal
	var err error
	if id, secret, ok := ParseKey(password); ok {
		p, err = a.key(name, id, secret)
	} else {
		p, err = a.password(name, password)
	}
	if err == nil && a.opts.Usage != nil {
		a.opts.Usage.Record(p, a.opts.Now())
	}
	return p, err
}

func (a *Authenticator) key(name, id, secret string) (Principal, error) {
	c, known := a.src.ByKeyID(id)
	ref := a.dummy
	if known {
		ref = c.digest
	}
	got := DigestOf(secret)
	match := subtle.ConstantTimeCompare(got[:], ref[:]) == 1
	if !known || !match || c.Sender != name || !c.Key.Active(a.opts.Now()) {
		return Principal{}, ErrBadCredentials
	}
	s, ok := a.src.ByName(name)
	if !ok { // removed in between
		return Principal{}, ErrBadCredentials
	}
	return Principal{Sender: name, Source: c.Source, Method: MethodKey, KeyID: id, Policy: s.Policy}, nil
}

func (a *Authenticator) password(name, password string) (Principal, error) {
	checks, hash := a.src.password(name)
	if checks == nil {
		// No password lines at all: nothing to compare with, and nothing
		// to hide by spending CPU.
		return Principal{}, ErrBadCredentials
	}
	ok := checks.Cached(name, password)
	if !ok {
		t := time.NewTimer(a.opts.Wait)
		defer t.Stop()
		select {
		case a.slots <- struct{}{}:
		case <-t.C:
			a.opts.Logger.Warn("smtp auth refused, too many password checks at once", "limit", cap(a.slots))
			return Principal{}, ErrBusy
		}
		ok = checks.Verify(name, password)
		<-a.slots
	}
	if !ok {
		return Principal{}, ErrBadCredentials
	}
	s, found := a.src.ByName(name)
	if !found {
		return Principal{}, ErrBadCredentials
	}
	return Principal{Sender: name, Source: s.Source, Method: MethodPassword, Policy: s.Policy, hash: hash}, nil
}

// Refresh returns p with the sender's current policy, or false if its
// credential is gone: the key revoked, expired or removed from the file, the
// password changed, or the sender removed. Ingress calls it at every MAIL,
// so a revoked key cannot go on sending over an open connection.
func (a *Authenticator) Refresh(p Principal) (Principal, bool) {
	s, ok := a.src.ByName(p.Sender)
	if !ok || s.Source != p.Source {
		return Principal{}, false
	}
	switch p.Method {
	case MethodKey:
		c, ok := a.src.ByKeyID(p.KeyID)
		if !ok || c.Sender != p.Sender || !c.Key.Active(a.opts.Now()) {
			return Principal{}, false
		}
	case MethodPassword:
		if _, hash := a.src.password(p.Sender); hash == "" || hash != p.hash {
			return Principal{}, false
		}
	default:
		return Principal{}, false
	}
	p.Policy = s.Policy
	return p, true
}

// Known reports whether name is a sender in any source. It decides whether
// a failed login may name the sender in an event; it is not a check.
func (a *Authenticator) Known(name string) bool { return a.src.Has(name) }
