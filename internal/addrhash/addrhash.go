// Package addrhash replaces mail addresses by keyed hashes (K1–K3).
//
// A hash is HMAC-SHA256 under a key from the hash key file, over the
// normalized address, stored as "<key-id>:<64 lower-case hex digits>". The key
// id makes rotation possible: new hashes use the active key, and a lookup
// hashes the query with every key in the file.
//
// Normalization (fixed; changing it makes old hashes unsearchable):
//
//  1. Remove leading and trailing ASCII white space (space, tab, CR, LF).
//  2. Lower-case the ASCII letters A–Z in the whole address, local part
//     included. Every other byte stays as it is.
//
// The local part of an address is case-sensitive by RFC 5321, but practically
// no mail system treats it so; Postwerk treats it case-insensitively, so a
// lookup for "Jane@Example.com" finds mail sent to "jane@example.com". Only
// ASCII letters are folded: Unicode case mapping changes between Unicode
// versions, and a hash must stay reproducible for years and in any language.
package addrhash

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"strings"

	"github.com/eneuberg/postwerk/internal/keys"
)

// Normalize returns the normalized form of addr that is hashed.
func Normalize(addr string) string {
	addr = strings.Trim(addr, " \t\r\n")
	b := []byte(addr)
	for i, c := range b {
		if c >= 'A' && c <= 'Z' {
			b[i] = c + ('a' - 'A')
		}
	}
	return string(b)
}

// Hasher hashes addresses with the keys of one hash key file.
type Hasher struct {
	ring *keys.Ring
}

// New returns a Hasher for ring, which must not be nil.
func New(ring *keys.Ring) *Hasher { return &Hasher{ring: ring} }

// Hash returns the stored form of addr under the active key.
func (h *Hasher) Hash(addr string) string {
	return hashWith(h.ring.Active(), Normalize(addr))
}

// Lookup returns what a stored address must equal to match addr: its
// normalized plain form (for rows not pseudonymized yet) and its hash under
// every key in the file, the active key first.
func (h *Hasher) Lookup(addr string) (normalized string, hashes []string) {
	n := Normalize(addr)
	for _, k := range h.ring.All() {
		hashes = append(hashes, hashWith(k, n))
	}
	return n, hashes
}

func hashWith(k keys.Key, normalized string) string {
	m := hmac.New(sha256.New, k.Secret())
	m.Write([]byte(normalized))
	return k.ID + ":" + hex.EncodeToString(m.Sum(nil))
}
