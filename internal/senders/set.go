package senders

import (
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/eneuberg/postwerk/internal/auth"
)

// set is an immutable snapshot of one source: what FileSource and DBSource
// swap in as a whole, so a reader sees one state or the next, never a mix.
type set struct {
	kind    Kind
	senders map[string]Sender
	keys    map[string]Credential // active and expired keys by id; never revoked ones
	// hashes are the PBKDF2 password lines by sender (file only); passwords
	// checks them at equal cost (auth.Senders). nil without any.
	hashes    map[string]string
	passwords *auth.Senders
}

func newSet(kind Kind) *set {
	return &set{kind: kind, senders: map[string]Sender{}, keys: map[string]Credential{}}
}

// finish sorts the keys and builds the password checks.
func (s *set) finish() error {
	for name, snd := range s.senders {
		slices.SortStableFunc(snd.Keys, func(a, b KeyInfo) int { return b.CreatedAt.Compare(a.CreatedAt) })
		s.senders[name] = snd
	}
	if len(s.hashes) > 0 {
		p, err := auth.NewSenders(s.hashes)
		if err != nil {
			return err
		}
		s.passwords = p
	}
	return nil
}

func (s *set) ByKeyID(id string) (Credential, bool) {
	c, ok := s.keys[id]
	return c, ok
}

func (s *set) ByName(name string) (Sender, bool) {
	snd, ok := s.senders[name]
	if !ok {
		return Sender{}, false
	}
	snd.Keys = slices.Clone(snd.Keys)
	return snd, true
}

func (s *set) List() []Sender {
	out := make([]Sender, 0, len(s.senders))
	for _, name := range slices.Sorted(maps.Keys(s.senders)) {
		snd, _ := s.ByName(name)
		out = append(out, snd)
	}
	return out
}

// hash returns the PBKDF2 value of name's password line, or "".
func (s *set) hash(name string) string { return s.hashes[name] }

// clash returns an error naming the first sender name or key id that s and
// other both have. Key ids include revoked keys, so an id is never reused.
func clash(s, other *set) error {
	var names []string
	for name := range s.senders {
		if _, ok := other.senders[name]; ok {
			names = append(names, name)
		}
	}
	if len(names) > 0 {
		slices.Sort(names)
		return fmt.Errorf("sender %s is in the %s and in the %s; a name may be in one place only",
			strings.Join(names, ", "), describe(s.kind), describe(other.kind))
	}
	ids := s.allKeyIDs()
	for id := range other.allKeyIDs() {
		if ids[id] {
			return fmt.Errorf("key id %s is in the %s and in the %s", id, describe(s.kind), describe(other.kind))
		}
	}
	return nil
}

func (s *set) allKeyIDs() map[string]bool {
	ids := map[string]bool{}
	for _, snd := range s.senders {
		for _, k := range snd.Keys {
			ids[k.ID] = true
		}
	}
	return ids
}

func describe(k Kind) string {
	if k == KindFile {
		return "senders file"
	}
	return "database"
}
