package senders

import (
	"errors"
	"maps"
	"slices"
	"sync"
)

// member is a source the Registry composes: one of this package's sources,
// which hand out their current snapshot and accept the Registry's guard.
type member interface {
	Source
	snapshot() *set
	setGuard(*guard)
}

// guard serializes every change of a member (a file reload, a sender or key
// created in the database) with the check that it clashes with no other
// member, so two changes can never add the same name to two sources.
type guard struct {
	mu      sync.Mutex
	members []member
}

// swap runs fn, which checks and swaps in a change, under the guard's lock.
func (g *guard) swap(fn func() error) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	return fn()
}

// check reports whether candidate, the next snapshot of self, clashes with
// another member. g.mu must be held.
func (g *guard) check(self member, candidate *set) error {
	for _, m := range g.members {
		if m == self {
			continue
		}
		if err := clash(candidate, m.snapshot()); err != nil {
			return err
		}
	}
	return nil
}

// taken reports whether name or key id is used by another member than self.
// g.mu must be held.
func (g *guard) taken(self member, name, keyID string) bool {
	for _, m := range g.members {
		if m == self {
			continue
		}
		s := m.snapshot()
		if _, ok := s.senders[name]; name != "" && ok {
			return true
		}
		if keyID != "" && s.allKeyIDs()[keyID] {
			return true
		}
	}
	return false
}

// Registry is one Source over the enabled sources (POSTWERK_SENDER_KEYS):
// the senders file, the database, or both. A sender name and a key id are in
// at most one of them: NewRegistry refuses a clash, and so does every later
// change of a source.
type Registry struct {
	g *guard
}

var _ Source = (*Registry)(nil)

// NewRegistry composes sources (a *FileSource and/or a *DBSource, nil ones
// are skipped). It fails if two of them share a sender name or a key id.
func NewRegistry(sources ...Source) (*Registry, error) {
	g := &guard{}
	for _, s := range sources {
		switch m := s.(type) {
		case nil:
		case *FileSource:
			if m != nil {
				g.members = append(g.members, m)
			}
		case *DBSource:
			if m != nil {
				g.members = append(g.members, m)
			}
		default:
			return nil, errors.New("senders: a registry composes FileSource and DBSource only")
		}
	}
	for i, a := range g.members {
		for _, b := range g.members[i+1:] {
			if err := clash(a.snapshot(), b.snapshot()); err != nil {
				return nil, err
			}
		}
	}
	for _, m := range g.members {
		m.setGuard(g)
	}
	return &Registry{g: g}, nil
}

// Kinds returns the kinds of the enabled sources.
func (r *Registry) Kinds() []Kind {
	var out []Kind
	for _, m := range r.g.members {
		out = append(out, m.snapshot().kind)
	}
	return out
}

func (r *Registry) ByKeyID(id string) (Credential, bool) {
	for _, m := range r.g.members {
		if c, ok := m.ByKeyID(id); ok {
			return c, true
		}
	}
	return Credential{}, false
}

func (r *Registry) ByName(name string) (Sender, bool) {
	for _, m := range r.g.members {
		if s, ok := m.ByName(name); ok {
			return s, true
		}
	}
	return Sender{}, false
}

// Has reports whether a sender of this name exists in any source. It is
// cheaper than ByName.
func (r *Registry) Has(name string) bool {
	for _, m := range r.g.members {
		if _, ok := m.snapshot().senders[name]; ok {
			return true
		}
	}
	return false
}

func (r *Registry) List() []Sender {
	byName := map[string]Sender{}
	for _, m := range r.g.members {
		for _, s := range m.List() {
			byName[s.Name] = s
		}
	}
	out := make([]Sender, 0, len(byName))
	for _, n := range slices.Sorted(maps.Keys(byName)) {
		out = append(out, byName[n])
	}
	return out
}

// password returns the PBKDF2 checks and name's hash value from the source
// that has password lines (the file), or nil.
func (r *Registry) password(name string) (checks passwordChecker, hash string) {
	for _, m := range r.g.members {
		s := m.snapshot()
		if s.passwords != nil {
			return s.passwords, s.hash(name)
		}
	}
	return nil, ""
}
