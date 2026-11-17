package query

import (
	"context"
	"errors"
	"slices"
	"strings"
	"time"

	"github.com/eneuberg/postwerk/internal/senders"
	"github.com/eneuberg/postwerk/internal/store"
)

// Reads about senders for the UI and the API (D62–D67). The senders
// themselves come from Options.Senders (the registry of every enabled
// source); their activity from the store. Nothing here returns a key, a
// digest or a password hash.

// ErrNoSender is returned for a sender name that no source has and that
// sent nothing recently.
var ErrNoSender = errors.New("no such sender")

// SenderKey is one key of a sender as a reader sees it.
type SenderKey struct {
	ID        string
	CreatedAt time.Time // zero for a key from the file
	CreatedBy string
	ExpiresAt time.Time // zero: none
	RevokedAt time.Time // zero: not revoked
	RevokedBy string
	// Active: neither revoked nor expired now.
	Active bool
	// LastUsed is the last login with this key (written once a minute);
	// zero if never.
	LastUsed time.Time
}

// SenderOverview is one sender with its keys and what it did in the last
// RecentWindow.
type SenderOverview struct {
	Name string
	// Source is "file" or "db"; empty for a sender that is in no source
	// any more but sent mail within the window (Removed).
	Source  senders.Kind
	Removed bool
	Policy  senders.Policy
	// Password: the sender has a password line in the senders file;
	// PasswordLastUsed its last login.
	Password         bool
	PasswordLastUsed time.Time
	Keys             []SenderKey
	// LastUsed is the newest login of any of its credentials.
	LastUsed  time.Time
	CreatedAt time.Time
	CreatedBy string
	UpdatedAt time.Time
	UpdatedBy string
	// Recent is the activity within RecentWindow: mails, failed
	// recipients, refused logins, refused commands, rate-limit refusals.
	Recent store.SenderActivity
}

// Senders returns every sender of the enabled sources, by name, plus the
// ones that are gone but sent mail within RecentWindow.
func (s *Service) Senders(ctx context.Context) ([]SenderOverview, error) {
	return s.senders(ctx, "")
}

// Sender returns one sender, or ErrNoSender.
func (s *Service) Sender(ctx context.Context, name string) (SenderOverview, error) {
	if !senders.ValidName(name) {
		return SenderOverview{}, ErrNoSender
	}
	out, err := s.senders(ctx, name)
	if err != nil {
		return SenderOverview{}, err
	}
	if len(out) == 0 {
		return SenderOverview{}, ErrNoSender
	}
	return out[0], nil
}

// senders builds the overview of every sender, or of only.
func (s *Service) senders(ctx context.Context, only string) ([]SenderOverview, error) {
	now := s.opts.Now()
	activity, err := s.opts.Store.SenderActivitySince(ctx, now.Add(-RecentWindow))
	if err != nil {
		return nil, err
	}
	uses, err := s.opts.Store.SenderUsage(ctx)
	if err != nil {
		return nil, err
	}
	var list []senders.Sender
	if s.opts.Senders != nil {
		list = s.opts.Senders.List()
	}
	byName := map[string]*SenderOverview{}
	var names []string
	for _, snd := range list {
		if only != "" && snd.Name != only {
			continue
		}
		o := &SenderOverview{Name: snd.Name, Source: snd.Source, Policy: snd.Policy, Password: snd.Password,
			CreatedAt: snd.CreatedAt, CreatedBy: snd.CreatedBy, UpdatedAt: snd.UpdatedAt, UpdatedBy: snd.UpdatedBy}
		for _, k := range snd.Keys {
			o.Keys = append(o.Keys, SenderKey{ID: k.ID, CreatedAt: k.CreatedAt, CreatedBy: k.CreatedBy,
				ExpiresAt: k.ExpiresAt, RevokedAt: k.RevokedAt, RevokedBy: k.RevokedBy, Active: k.Active(now)})
		}
		byName[snd.Name] = o
		names = append(names, snd.Name)
	}
	for name := range activity {
		if _, ok := byName[name]; ok || (only != "" && name != only) || !senders.ValidName(name) {
			continue
		}
		byName[name] = &SenderOverview{Name: name, Removed: true}
		names = append(names, name)
	}
	for _, u := range uses {
		o := byName[u.Sender]
		if o == nil || string(o.Source) != u.Source {
			continue // usage of a former sender, or of the same name in the other source
		}
		if u.KeyID == "" {
			o.PasswordLastUsed = u.At
		}
		for i := range o.Keys {
			if o.Keys[i].ID == u.KeyID {
				o.Keys[i].LastUsed = u.At
			}
		}
		if u.At.After(o.LastUsed) {
			o.LastUsed = u.At
		}
	}
	slices.SortFunc(names, strings.Compare)
	out := make([]SenderOverview, 0, len(names))
	for _, n := range names {
		o := byName[n]
		if a := activity[n]; a != nil {
			o.Recent = *a
		}
		out = append(out, *o)
	}
	return out, nil
}
