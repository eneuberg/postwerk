// Package keys reads Postwerk's key files (E5): the store key file (encryption of
// raw messages at rest) and the hash key file (keyed hashes of addresses).
// Both use the same format, but they must hold different keys.
//
// One key per line:
//
//	<key-id>:<base64 of 32 bytes>
//
// The key id is 1 to 16 characters of a-z and 0-9; the key is 32 random
// bytes in standard base64 with padding (44 characters). The first key line is
// the active key: new data uses it. Further lines are older keys, kept so
// that data written with them can still be read or looked up. Empty lines and
// lines starting with "#" are ignored.
//
// Keys are read once at start from files, never from environment variables.
// Error texts name the line, never its content. A Key prints as its id only,
// so a key cannot end up in a log by accident.
package keys

import (
	"bufio"
	"bytes"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"regexp"
	"time"
)

// Size is the length of every key in bytes.
const Size = 32

// maxKeys bounds the number of keys in one file.
const maxKeys = 64

var idRe = regexp.MustCompile(`^[a-z0-9]{1,16}$`)

// ValidID reports whether id is a valid key id.
func ValidID(id string) bool { return idRe.MatchString(id) }

// Key is one named key.
type Key struct {
	ID     string
	secret []byte
}

// Secret returns the key bytes. Callers must not change or log them.
func (k Key) Secret() []byte { return k.secret }

// String returns the key id only.
func (k Key) String() string { return "key " + k.ID }

// GoString returns the key id only, also for %#v.
func (k Key) GoString() string { return k.String() }

// Ring is the content of one key file. It is never empty.
type Ring struct {
	keys []Key
}

// Active returns the key for new data: the first key in the file.
func (r *Ring) Active() Key { return r.keys[0] }

// Lookup returns the key with the given id.
func (r *Ring) Lookup(id string) (Key, bool) {
	for _, k := range r.keys {
		if k.ID == id {
			return k, true
		}
	}
	return Key{}, false
}

// All returns every key, the active one first.
func (r *Ring) All() []Key { return append([]Key(nil), r.keys...) }

// String lists the key ids only.
func (r *Ring) String() string {
	s := "keys["
	for i, k := range r.keys {
		if i > 0 {
			s += ","
		}
		s += k.ID
	}
	return s + "]"
}

// GoString lists the key ids only, also for %#v.
func (r *Ring) GoString() string { return r.String() }

// Shares reports whether r and o contain the same key material under any id.
func (r *Ring) Shares(o *Ring) bool {
	for _, a := range r.keys {
		for _, b := range o.keys {
			if subtle.ConstantTimeCompare(a.secret, b.secret) == 1 {
				return true
			}
		}
	}
	return false
}

// Parse reads a key file. All problems are returned together.
func Parse(b []byte) (*Ring, error) {
	var errs []error
	r := &Ring{}
	seen := make(map[string]bool)
	sc := bufio.NewScanner(bytes.NewReader(b))
	sc.Buffer(make([]byte, 0, 256), 4096)
	line := 0
	for sc.Scan() {
		line++
		l := bytes.TrimSpace(sc.Bytes())
		if len(l) == 0 || l[0] == '#' {
			continue
		}
		id, enc, ok := bytes.Cut(l, []byte(":"))
		if !ok {
			errs = append(errs, fmt.Errorf("line %d: expected <key-id>:<base64 key>", line))
			continue
		}
		if !ValidID(string(id)) {
			errs = append(errs, fmt.Errorf("line %d: key id must be 1 to 16 of a-z and 0-9", line))
			continue
		}
		if seen[string(id)] {
			errs = append(errs, fmt.Errorf("line %d: key id %s used twice", line, id))
			continue
		}
		secret := make([]byte, base64.StdEncoding.DecodedLen(len(enc)))
		n, err := base64.StdEncoding.Strict().Decode(secret, enc)
		if err != nil || n != Size {
			errs = append(errs, fmt.Errorf("line %d: key must be %d bytes in standard base64 (create one with 'postwerk gen-key')", line, Size))
			continue
		}
		seen[string(id)] = true
		r.keys = append(r.keys, Key{ID: string(id), secret: secret[:n]})
	}
	if err := sc.Err(); err != nil {
		errs = append(errs, fmt.Errorf("line %d: %v", line+1, err))
	}
	if len(r.keys) > maxKeys {
		errs = append(errs, fmt.Errorf("at most %d keys", maxKeys))
	}
	if len(errs) == 0 && len(r.keys) == 0 {
		errs = append(errs, errors.New("no key (create one with 'postwerk gen-key')"))
	}
	if len(errs) > 0 {
		return nil, errors.Join(errs...)
	}
	return r, nil
}

// Generate returns a new key file line "<key-id>:<base64>" made from
// crypto/rand. The id is the date (YYYYMMDD) and four random characters, so
// ids sort by age and rarely collide.
func Generate(now time.Time) string {
	const alphabet = "abcdefghijklmnopqrstuvwxyz0123456789"
	secret := make([]byte, Size)
	_, _ = rand.Read(secret) // never fails (crypto/rand)
	suffix := make([]byte, 4)
	for i := range suffix {
		suffix[i] = alphabet[randIndex(len(alphabet))]
	}
	return now.UTC().Format("20060102") + string(suffix) + ":" + base64.StdEncoding.EncodeToString(secret)
}

// randIndex returns a uniform random number in [0, n) for n ≤ 256.
func randIndex(n int) int {
	limit := 256 - 256%n
	var b [1]byte
	for {
		_, _ = rand.Read(b[:])
		if int(b[0]) < limit {
			return int(b[0]) % n
		}
	}
}
