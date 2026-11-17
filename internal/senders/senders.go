// Package senders holds the applications that may send through Postwerk and
// their credentials (D62–D66).
//
// One sender is one application. It logs in with SMTP AUTH PLAIN: the user
// name is the sender's name, the password one of its keys
// (pwk_<id>_<secret>, see NewKey) or, for lines written before 1.1, a
// password whose PBKDF2 hash is in the senders file. A sender has at most two
// active credentials at once, so an application can switch to a new key
// without downtime, and a Policy (allowed envelope senders, rate limits).
//
// The package is split along who needs what:
//
//	Source         read side: look up a key by id, a sender by name, list them
//	Store          managed write side: create, rotate, revoke, set the policy
//	FileSource     Source from POSTWERK_SENDERS_FILE, reloaded when it changes
//	DBSource       Source and Store over the database (internal/store)
//	Registry       one Source over the enabled sources; refuses name clashes
//	Authenticator  checks a login against a Registry; what ingress uses
//	Usage          remembers when each credential was last used, written in batches
//
// Nothing here imports the UI, the API or ingress.
package senders

import (
	"context"
	"errors"
	"regexp"
	"time"
)

// Kind names where a sender is kept.
type Kind string

// Kinds of source.
const (
	KindFile Kind = "file"
	KindDB   Kind = "db"
)

// Method is how a sender logged in.
type Method string

// Login methods.
const (
	MethodKey      Method = "key"
	MethodPassword Method = "password"
)

// MaxActiveKeys is how many credentials a sender may have at once: the
// current one and, during a rotation, its successor.
const MaxActiveKeys = 2

var nameRe = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,63}$`)

// ValidName reports whether name is a valid sender name, the same rule as
// for every name in Postwerk's files.
func ValidName(name string) bool { return nameRe.MatchString(name) }

// KeyInfo describes one key. It never holds the secret or its digest.
type KeyInfo struct {
	ID        string
	CreatedAt time.Time // zero for keys from the file
	CreatedBy string
	ExpiresAt time.Time // zero: no expiry
	RevokedAt time.Time // zero while not revoked
	RevokedBy string
}

// Active reports whether the key can log in at now.
func (k KeyInfo) Active(now time.Time) bool {
	return k.RevokedAt.IsZero() && (k.ExpiresAt.IsZero() || now.Before(k.ExpiresAt))
}

// Sender is one application as a Source knows it.
type Sender struct {
	Name   string
	Source Kind
	Policy Policy
	// Keys are the sender's keys; for the database also the revoked ones,
	// newest first.
	Keys []KeyInfo
	// Password is set when the sender has a PBKDF2 password line in the
	// senders file.
	Password bool
	// Times and actors of a managed sender; zero for the file.
	CreatedAt time.Time
	CreatedBy string
	UpdatedAt time.Time
	UpdatedBy string
}

// ActiveKeys returns the keys that can log in at now.
func (s Sender) ActiveKeys(now time.Time) []KeyInfo {
	var out []KeyInfo
	for _, k := range s.Keys {
		if k.Active(now) {
			out = append(out, k)
		}
	}
	return out
}

// Credential is an active key found by its id.
type Credential struct {
	Sender string
	Source Kind
	Key    KeyInfo
	digest Digest
}

// Source is the read side of a set of senders. Implementations are safe for
// concurrent use and answer from memory.
type Source interface {
	// ByKeyID returns the key with this id unless it is revoked (an expired
	// key is returned; the caller checks KeyInfo.Active).
	ByKeyID(id string) (Credential, bool)
	// ByName returns the sender with this name.
	ByName(name string) (Sender, bool)
	// List returns every sender, by name.
	List() []Sender
}

// Issued is a key just created. Key is the full key: it is shown once and
// never stored.
type Issued struct {
	Sender    string
	KeyID     string
	Key       string
	ExpiresAt time.Time
}

// CreateRequest is a new managed sender with its first key.
type CreateRequest struct {
	Name      string
	Policy    Policy
	ExpiresAt time.Time // zero: the key does not expire
	Actor     string    // who creates it: a UI account or an API token name
}

// Store is the managed write side. Every change is written together with
// its event (sender.created, sender.rotated, sender.revoked,
// sender.policy_changed) naming the actor.
type Store interface {
	// Create adds a sender and its first key. ErrNameTaken if the name is
	// in use in any source.
	Create(ctx context.Context, r CreateRequest) (Issued, error)
	// Rotate adds a key to a sender; ErrTooManyKeys while it has
	// MaxActiveKeys active keys. Revoke the old key once the application
	// uses the new one.
	Rotate(ctx context.Context, name string, expiresAt time.Time, actor string) (Issued, error)
	// Revoke ends one key at once; the sender's other key stays valid.
	Revoke(ctx context.Context, keyID, actor string) error
	// SetPolicy replaces the sender's policy.
	SetPolicy(ctx context.Context, name string, p Policy, actor string) error
}

// Errors of the Store and the file. Their texts are safe to show.
var (
	ErrInvalidName  = errors.New("sender names have 1 to 64 characters: a-z, 0-9, '.', '_' and '-', starting with a letter or digit")
	ErrNameTaken    = errors.New("a sender with this name exists")
	ErrNoSender     = errors.New("no such sender")
	ErrNoKey        = errors.New("no such key")
	ErrRevoked      = errors.New("the key is already revoked")
	ErrTooManyKeys  = errors.New("the sender has two active keys; revoke one first")
	ErrExpiry       = errors.New("the expiry must be in the future")
	ErrInvalidActor = errors.New("an actor is required")
)

// Event is a change of a source that is not written by the store itself:
// a reload of the senders file. Kinds below; levels as in the event log.
type Event struct {
	Kind  string
	Level string
	Text  string
}

// Event kinds of the senders file.
const (
	KindReloaded     = "senders.reloaded"
	KindReloadFailed = "senders.reload_failed"
)
