// Package auth stores and checks sender passwords (I2).
//
// The senders file has one sender per line:
//
//	name:pbkdf2-sha256$<iterations>$<salt>$<key>
//
// with salt and key in unpadded standard base64. Empty lines and lines starting
// with "#" are ignored. Names match [a-z0-9][a-z0-9._-]{0,63}.
//
// Every check costs the same: the highest iteration count in the file. An
// unknown name is checked against a dummy hash with that count, and a sender
// whose hash has fewer iterations pays the difference with extra PBKDF2
// rounds. So the time of a failed login says nothing about which names exist
// or how their hashes were made. After a successful login, an HMAC of the
// password under a random key held in memory lets later logins of the same
// sender skip PBKDF2 (Cached), so bursts of connections do not burn CPU; only
// the right password can take that path. A wrong password always pays the
// full cost.
package auth

import (
	"bufio"
	"bytes"
	"crypto/hmac"
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strconv"
	"strings"
	"sync"
)

// Scheme is the hash scheme name in the senders file.
const Scheme = "pbkdf2-sha256"

// Iteration bounds and default.
const (
	DefaultIterations = 600_000
	MinIterations     = 100_000
	MaxIterations     = 10_000_000
)

const (
	saltLen        = 16
	keyLen         = 32
	maxPasswordLen = 1024
)

var nameRe = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,63}$`)

// ValidName reports whether name is a valid sender name.
func ValidName(name string) bool { return nameRe.MatchString(name) }

type hash struct {
	iterations int
	salt       []byte
	key        []byte
}

// Senders holds the known senders and checks their passwords. It is safe for
// concurrent use.
type Senders struct {
	users map[string]hash
	dummy hash // also the strongest iteration count in the file

	cacheKey []byte
	mu       sync.Mutex
	cache    map[string][]byte // name → HMAC(cacheKey, password) of the last good login
}

// Hash returns the senders-file value for password, e.g.
// "pbkdf2-sha256$600000$<salt>$<key>".
func Hash(password string, iterations int) (string, error) {
	if password == "" {
		return "", errors.New("auth: empty password")
	}
	if len(password) > maxPasswordLen {
		return "", fmt.Errorf("auth: password longer than %d bytes", maxPasswordLen)
	}
	if iterations < MinIterations || iterations > MaxIterations {
		return "", fmt.Errorf("auth: iterations must be between %d and %d", MinIterations, MaxIterations)
	}
	salt := make([]byte, saltLen)
	_, _ = rand.Read(salt)
	key, err := pbkdf2.Key(sha256.New, password, salt, iterations, keyLen)
	if err != nil {
		return "", fmt.Errorf("auth: derive key: %w", err)
	}
	enc := base64.RawStdEncoding
	return fmt.Sprintf("%s$%d$%s$%s", Scheme, iterations, enc.EncodeToString(salt), enc.EncodeToString(key)), nil
}

// ParseSenders reads a senders file. Errors name the line number but never
// quote the line, which may hold a secret.
func ParseSenders(r io.Reader) (*Senders, error) {
	users := make(map[string]hash)
	errs := scanLines(r, func(lineNo int, line string) error {
		name, value, ok := strings.Cut(line, ":")
		if !ok {
			return fmt.Errorf("line %d: expected name:hash", lineNo)
		}
		if !ValidName(name) {
			return fmt.Errorf("line %d: invalid sender name", lineNo)
		}
		if _, dup := users[name]; dup {
			return fmt.Errorf("line %d: sender %q listed twice", lineNo, name)
		}
		h, err := parseHash(value)
		if err != nil {
			return fmt.Errorf("line %d: %w", lineNo, err)
		}
		users[name] = h
		return nil
	})
	if len(errs) == 0 && len(users) == 0 {
		errs = append(errs, errors.New("no senders defined"))
	}
	if len(errs) > 0 {
		return nil, errors.Join(errs...)
	}
	return newSenders(users), nil
}

// scanLines calls fn for every line that is not empty or a comment, with the
// line trimmed, and collects the errors.
func scanLines(r io.Reader, fn func(lineNo int, line string) error) []error {
	var errs []error
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 4096), 64*1024)
	lineNo := 0
	for sc.Scan() {
		lineNo++
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if err := fn(lineNo, line); err != nil {
			errs = append(errs, err)
		}
	}
	if err := sc.Err(); err != nil {
		errs = append(errs, fmt.Errorf("read: %w", err))
	}
	return errs
}

// CheckHash reports whether v is a valid senders-file hash value
// (pbkdf2-sha256$…). The error never quotes v.
func CheckHash(v string) error {
	_, err := parseHash(v)
	return err
}

// NewSenders returns the checks for the senders and their hash values, as
// ParseSenders builds them from a file (internal/senders reads the file
// now). Every value must pass CheckHash; hashes must not be empty.
func NewSenders(hashes map[string]string) (*Senders, error) {
	if len(hashes) == 0 {
		return nil, errors.New("auth: no senders")
	}
	users := make(map[string]hash, len(hashes))
	for name, v := range hashes {
		h, err := parseHash(v)
		if err != nil {
			return nil, fmt.Errorf("auth: sender %q: %w", name, err)
		}
		users[name] = h
	}
	return newSenders(users), nil
}

// newSenders sets up the checks for users, which must not be empty.
func newSenders(users map[string]hash) *Senders {
	s := &Senders{
		users:    users,
		cacheKey: make([]byte, 32),
		cache:    make(map[string][]byte),
	}
	_, _ = rand.Read(s.cacheKey)
	maxIter := 0
	for _, h := range users {
		maxIter = max(maxIter, h.iterations)
	}
	// Checking against the dummy costs as much as the most expensive real
	// hash. Its key is random, so no password matches it.
	s.dummy = newDummy(maxIter)
	return s
}

func parseHash(v string) (hash, error) {
	parts := strings.Split(v, "$")
	if len(parts) != 4 || parts[0] != Scheme {
		return hash{}, fmt.Errorf("expected %s$<iterations>$<salt>$<key>", Scheme)
	}
	iter, err := strconv.Atoi(parts[1])
	if err != nil || iter < MinIterations || iter > MaxIterations {
		return hash{}, fmt.Errorf("iterations must be a number between %d and %d", MinIterations, MaxIterations)
	}
	enc := base64.RawStdEncoding.Strict()
	salt, err := enc.DecodeString(parts[2])
	if err != nil || len(salt) < 8 {
		return hash{}, errors.New("salt must be at least 8 bytes of unpadded base64")
	}
	key, err := enc.DecodeString(parts[3])
	if err != nil || len(key) != keyLen {
		return hash{}, fmt.Errorf("key must be %d bytes of unpadded base64", keyLen)
	}
	return hash{iterations: iter, salt: salt, key: key}, nil
}

// Len returns the number of known senders.
func (s *Senders) Len() int { return len(s.users) }

// Known reports whether name is in the senders file. It is for deciding
// whether a failed login may be named in an event (the name, never the
// password); it is not a password check.
func (s *Senders) Known(name string) bool {
	_, ok := s.users[name]
	return ok
}

// Verify reports whether password is correct for the sender name. Unless
// the login is cached, it always costs PBKDF2 with the strongest iteration
// count in the file, for known and unknown names alike.
func (s *Senders) Verify(name, password string) bool {
	if s.Cached(name, password) {
		return true
	}
	h, known := s.users[name]
	if !known || len(password) > maxPasswordLen {
		_ = check(s.dummy, password)
		return false
	}
	ok := check(h, password)
	if extra := s.dummy.iterations - h.iterations; extra > 0 {
		// Pay the difference to the strongest hash, so the time does not
		// tell weak hashes (or their names) apart.
		_, _ = derive(password, s.dummy.salt, extra, keyLen)
	}
	if !ok {
		return false
	}
	s.mu.Lock()
	s.cache[name] = s.mac(password)
	s.mu.Unlock()
	return true
}

// Cached reports whether name logged in with this password before; it costs
// one HMAC, no PBKDF2. A false result says nothing; Verify decides.
func (s *Senders) Cached(name, password string) bool {
	if len(password) > maxPasswordLen {
		return false
	}
	s.mu.Lock()
	cached := s.cache[name]
	s.mu.Unlock()
	return cached != nil && hmac.Equal(cached, s.mac(password))
}

func fillRandom(b []byte) { _, _ = rand.Read(b) }

// derive is PBKDF2-SHA256; tests count the iterations through it.
var derive = func(password string, salt []byte, iterations, keyLength int) ([]byte, error) {
	return pbkdf2.Key(sha256.New, password, salt, iterations, keyLength)
}

func (s *Senders) mac(password string) []byte {
	m := hmac.New(sha256.New, s.cacheKey)
	m.Write([]byte(password))
	return m.Sum(nil)
}

func check(h hash, password string) bool {
	key, err := derive(password, h.salt, h.iterations, len(h.key))
	if err != nil {
		return false
	}
	return subtle.ConstantTimeCompare(key, h.key) == 1
}

// ReadPassword reads one password from r: the first line, without its line
// ending. It is used by "postwerk hash-password".
func ReadPassword(r io.Reader) (string, error) {
	buf, err := io.ReadAll(io.LimitReader(r, maxPasswordLen+2))
	if err != nil {
		return "", fmt.Errorf("read password: %w", err)
	}
	line, _, _ := bytes.Cut(buf, []byte("\n"))
	line = bytes.TrimSuffix(line, []byte("\r"))
	if len(line) > maxPasswordLen {
		return "", fmt.Errorf("password longer than %d bytes", maxPasswordLen)
	}
	if len(line) == 0 {
		return "", errors.New("empty password")
	}
	return string(line), nil
}
