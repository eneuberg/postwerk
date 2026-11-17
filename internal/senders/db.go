package senders

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/eneuberg/postwerk/internal/store"
)

// DB is what DBSource needs from the store (internal/store.Store).
type DB interface {
	Senders(ctx context.Context) ([]store.SenderRow, error)
	CreateSender(ctx context.Context, name string, p store.SenderPolicy, k store.NewSenderKey, actor string, at time.Time) error
	AddSenderKey(ctx context.Context, name string, k store.NewSenderKey, maxActive int, actor string, at time.Time) error
	RevokeSenderKey(ctx context.Context, id, actor string, at time.Time) (string, error)
	SetSenderPolicy(ctx context.Context, name string, p store.SenderPolicy, summary, actor string, at time.Time) error
}

// DBSource is the Source and the Store of the senders kept in the database
// (POSTWERK_SENDER_KEYS=db or both). It holds every sender in memory and
// reads the tables again after each of its own changes; this process is the
// only writer (D61), so the memory never falls behind. If that read fails,
// the keys the change was about are taken out of the memory until a later
// read succeeds, so a revoke or a narrower policy is never left unenforced.
// Logins never read the database.
type DBSource struct {
	db  DB
	now func() time.Time

	cur   atomic.Pointer[set]
	guard atomic.Pointer[guard]
	// mu serializes changes when there is no Registry guard (tests).
	mu sync.Mutex
}

var (
	_ Source = (*DBSource)(nil)
	_ Store  = (*DBSource)(nil)
)

// NewDBSource reads the managed senders. now nil means time.Now.
func NewDBSource(ctx context.Context, db DB, now func() time.Time) (*DBSource, error) {
	if now == nil {
		now = time.Now
	}
	d := &DBSource{db: db, now: now}
	s, err := d.read(ctx)
	if err != nil {
		return nil, err
	}
	d.cur.Store(s)
	return d, nil
}

func (d *DBSource) read(ctx context.Context) (*set, error) {
	rows, err := d.db.Senders(ctx)
	if err != nil {
		return nil, err
	}
	s := newSet(KindDB)
	for _, r := range rows {
		p, err := NewPolicy(r.Policy.AllowedFrom, r.Policy.MaxPerMinute, r.Policy.MaxRcptPerHour)
		if err != nil {
			// Only this package writes the rows, through NewPolicy.
			return nil, fmt.Errorf("senders: stored policy of %q: %w", r.Name, err)
		}
		snd := Sender{Name: r.Name, Source: KindDB, Policy: p, CreatedAt: r.CreatedAt, CreatedBy: r.CreatedBy,
			UpdatedAt: r.UpdatedAt, UpdatedBy: r.UpdatedBy}
		for _, k := range r.Keys {
			info := KeyInfo{ID: k.ID, CreatedAt: k.CreatedAt, CreatedBy: k.CreatedBy, ExpiresAt: k.ExpiresAt,
				RevokedAt: k.RevokedAt, RevokedBy: k.RevokedBy}
			snd.Keys = append(snd.Keys, info)
			if info.RevokedAt.IsZero() && len(k.Digest) == len(Digest{}) {
				c := Credential{Sender: r.Name, Source: KindDB, Key: info}
				copy(c.digest[:], k.Digest)
				s.keys[k.ID] = c
			}
		}
		s.senders[r.Name] = snd
	}
	if err := s.finish(); err != nil {
		return nil, err
	}
	return s, nil
}

// change runs write under the registry's guard (or the source's own lock)
// and then reads the tables again. check, run first under the same lock,
// may refuse the change. affected names the keys the change restricts; if
// the read fails they can no longer log in until a later read succeeds
// (fail closed: the write may have landed). nil for a change that only adds
// a key, which a stale memory simply does not know yet.
func (d *DBSource) change(ctx context.Context, check func(g *guard) error, write func() error,
	affected func(Credential) bool) error {
	run := func(g *guard) error {
		if check != nil {
			if err := check(g); err != nil {
				return err
			}
		}
		werr := write()
		// Read again even after a failed write: the memory must match the
		// table, whatever happened.
		s, rerr := d.read(context.WithoutCancel(ctx))
		switch {
		case rerr == nil:
			d.cur.Store(s)
		case affected != nil:
			d.cur.Store(d.cur.Load().without(affected))
		}
		if werr != nil {
			return werr
		}
		return rerr
	}
	if g := d.guard.Load(); g != nil {
		return g.swap(func() error { return run(g) })
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	return run(nil)
}

// Create adds a managed sender with its first key (Store).
func (d *DBSource) Create(ctx context.Context, r CreateRequest) (Issued, error) {
	if !ValidName(r.Name) {
		return Issued{}, ErrInvalidName
	}
	if err := d.checkChange(r.ExpiresAt, r.Actor); err != nil {
		return Issued{}, err
	}
	key, id := NewKey()
	_, secret, _ := ParseKey(key)
	digest := DigestOf(secret)
	err := d.change(ctx, func(g *guard) error {
		if g != nil && g.taken(d, r.Name, id) {
			return ErrNameTaken
		}
		return nil
	}, func() error {
		return d.db.CreateSender(ctx, r.Name, storePolicy(r.Policy),
			store.NewSenderKey{ID: id, Digest: digest[:], ExpiresAt: r.ExpiresAt}, r.Actor, d.now())
	}, nil)
	if errors.Is(err, store.ErrSenderExists) {
		err = ErrNameTaken
	}
	if err != nil {
		return Issued{}, err
	}
	return Issued{Sender: r.Name, KeyID: id, Key: key, ExpiresAt: r.ExpiresAt}, nil
}

// Rotate adds a key to a managed sender (Store).
func (d *DBSource) Rotate(ctx context.Context, name string, expiresAt time.Time, actor string) (Issued, error) {
	if err := d.checkChange(expiresAt, actor); err != nil {
		return Issued{}, err
	}
	key, id := NewKey()
	_, secret, _ := ParseKey(key)
	digest := DigestOf(secret)
	err := d.change(ctx, func(g *guard) error {
		if g != nil && g.taken(d, "", id) {
			return errors.New("senders: key id in use, try again")
		}
		return nil
	}, func() error {
		return d.db.AddSenderKey(ctx, name, store.NewSenderKey{ID: id, Digest: digest[:], ExpiresAt: expiresAt},
			MaxActiveKeys, actor, d.now())
	}, nil)
	switch {
	case errors.Is(err, store.ErrNoSender):
		return Issued{}, ErrNoSender
	case errors.Is(err, store.ErrTooManyKeys):
		return Issued{}, ErrTooManyKeys
	case err != nil:
		return Issued{}, err
	}
	return Issued{Sender: name, KeyID: id, Key: key, ExpiresAt: expiresAt}, nil
}

// Revoke ends one key at once (Store).
func (d *DBSource) Revoke(ctx context.Context, keyID, actor string) error {
	if actor == "" {
		return ErrInvalidActor
	}
	if !ValidKeyID(keyID) {
		return ErrNoKey
	}
	err := d.change(ctx, nil, func() error {
		_, err := d.db.RevokeSenderKey(ctx, keyID, actor, d.now())
		return err
	}, func(c Credential) bool { return c.Key.ID == keyID })
	switch {
	case errors.Is(err, store.ErrNoSenderKey):
		return ErrNoKey
	case errors.Is(err, store.ErrKeyRevoked):
		return ErrRevoked
	}
	return err
}

// SetPolicy replaces a managed sender's policy (Store).
func (d *DBSource) SetPolicy(ctx context.Context, name string, p Policy, actor string) error {
	if actor == "" {
		return ErrInvalidActor
	}
	err := d.change(ctx, nil, func() error {
		return d.db.SetSenderPolicy(ctx, name, storePolicy(p), p.Summary(), actor, d.now())
	}, func(c Credential) bool { return c.Sender == name })
	if errors.Is(err, store.ErrNoSender) {
		return ErrNoSender
	}
	return err
}

func (d *DBSource) checkChange(expiresAt time.Time, actor string) error {
	if actor == "" {
		return ErrInvalidActor
	}
	if !expiresAt.IsZero() && !expiresAt.After(d.now()) {
		return ErrExpiry
	}
	return nil
}

func storePolicy(p Policy) store.SenderPolicy {
	return store.SenderPolicy{AllowedFrom: p.From(), MaxPerMinute: p.MaxPerMinute(), MaxRcptPerHour: p.MaxRecipientsPerHour()}
}

func (d *DBSource) snapshot() *set                       { return d.cur.Load() }
func (d *DBSource) setGuard(g *guard)                    { d.guard.Store(g) }
func (d *DBSource) ByKeyID(id string) (Credential, bool) { return d.cur.Load().ByKeyID(id) }
func (d *DBSource) ByName(name string) (Sender, bool)    { return d.cur.Load().ByName(name) }
func (d *DBSource) List() []Sender                       { return d.cur.Load().List() }
