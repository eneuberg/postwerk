package senders

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base32"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
)

// Sender keys (D62). A key is
//
//	pwk_<id>_<secret>
//
// with id 12 and secret 52 characters of lower-case base32 (a–z, 2–7): the id
// carries 60 random bits and names the key in events, mail records and the
// UI; the secret carries 256 random bits and is never stored. Only the
// SHA-256 of the secret is kept, like an API token (D41): a random 256-bit
// value cannot be guessed, so a slow hash would only cost CPU.
const (
	KeyPrefix = "pwk_"
	idChars   = 12
	secretLen = 32 // bytes of randomness
	// secretChars is the length of the encoded secret.
	secretChars = 52
	// KeyLen is the length of a whole key.
	KeyLen = len(KeyPrefix) + idChars + 1 + secretChars
)

// KeyScheme is the hash scheme of a key line in the senders file:
//
//	name:pwk-sha256$<id>$<digest>
//
// with the digest in unpadded standard base64.
const KeyScheme = "pwk-sha256"

var b32 = base32.NewEncoding("abcdefghijklmnopqrstuvwxyz234567").WithPadding(base32.NoPadding)

// Digest is the SHA-256 of a key's secret part.
type Digest [sha256.Size]byte

// NewKey returns a new random key and its id. The key is shown once; keep
// only its digest (DigestOf).
func NewKey() (key, id string) {
	idBytes := make([]byte, 8) // 64 bits, the first 60 are used
	_, _ = rand.Read(idBytes)
	secret := make([]byte, secretLen)
	_, _ = rand.Read(secret)
	id = b32.EncodeToString(idBytes)[:idChars]
	return KeyPrefix + id + "_" + b32.EncodeToString(secret), id
}

// ParseKey splits a key into its id and secret. It reports false for
// anything that is not exactly a key; it never fails on input of any kind.
func ParseKey(s string) (id, secret string, ok bool) {
	if len(s) != KeyLen || !strings.HasPrefix(s, KeyPrefix) {
		return "", "", false
	}
	rest := s[len(KeyPrefix):]
	if rest[idChars] != '_' {
		return "", "", false
	}
	id, secret = rest[:idChars], rest[idChars+1:]
	if !ValidKeyID(id) || !base32Chars(secret) {
		return "", "", false
	}
	return id, secret, true
}

// ValidKeyID reports whether id has the form of a key id.
func ValidKeyID(id string) bool { return len(id) == idChars && base32Chars(id) }

func base32Chars(s string) bool {
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !(c >= 'a' && c <= 'z' || c >= '2' && c <= '7') {
			return false
		}
	}
	return true
}

// DigestOf returns the SHA-256 of a key's secret part.
func DigestOf(secret string) Digest { return sha256.Sum256([]byte(secret)) }

// KeyLine returns the senders-file line for a key of sender name.
func KeyLine(name, key string) (string, error) {
	if !ValidName(name) {
		return "", ErrInvalidName
	}
	id, secret, ok := ParseKey(key)
	if !ok {
		return "", errors.New("senders: not a sender key")
	}
	d := DigestOf(secret)
	return name + ":" + KeyScheme + "$" + id + "$" + base64.RawStdEncoding.EncodeToString(d[:]), nil
}

// parseKeyHash parses the value of a key line, "pwk-sha256$<id>$<digest>".
func parseKeyHash(v string) (string, Digest, error) {
	var d Digest
	rest, ok := strings.CutPrefix(v, KeyScheme+"$")
	if !ok {
		return "", d, fmt.Errorf("expected %s$<id>$<digest>", KeyScheme)
	}
	id, digest, ok := strings.Cut(rest, "$")
	if !ok || !ValidKeyID(id) {
		return "", d, fmt.Errorf("expected %s$<id>$<digest> with a %d-character key id", KeyScheme, idChars)
	}
	b, err := base64.RawStdEncoding.Strict().DecodeString(digest)
	if err != nil || len(b) != len(d) {
		return "", d, fmt.Errorf("digest must be %d bytes of unpadded base64", len(d))
	}
	copy(d[:], b)
	return id, d, nil
}
