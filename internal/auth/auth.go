// Package auth stores and checks sender passwords (I2).
//
// The senders file has one sender per line:
//
//	name:pbkdf2-sha256$<iterations>$<salt>$<key>
//
// with salt and key in unpadded standard base64. Empty lines and lines starting
// with "#" are ignored. Names match [a-z0-9][a-z0-9._-]{0,63}.
//
// Verification costs the same for unknown and known senders: an unknown name is
// checked against a dummy hash with the highest iteration count in the file.
// After a successful login, an HMAC of the password under a random key held in
// memory lets later logins of the same sender skip PBKDF2, so bursts of
// connections do not burn CPU. A wrong password always pays the full cost.
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
	dummy hash

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
	s := &Senders{
		users:    make(map[string]hash),
		cacheKey: make([]byte, 32),
		cache:    make(map[string][]byte),
	}
	_, _ = rand.Read(s.cacheKey)

	var errs []error
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 4096), 64*1024)
	lineNo := 0
	maxIter := 0
	for sc.Scan() {
		lineNo++
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		name, value, ok := strings.Cut(line, ":")
		if !ok {
			errs = append(errs, fmt.Errorf("line %d: expected name:hash", lineNo))
			continue
		}
		if !ValidName(name) {
			errs = append(errs, fmt.Errorf("line %d: invalid sender name", lineNo))
			continue
		}
		if _, dup := s.users[name]; dup {
			errs = append(errs, fmt.Errorf("line %d: sender %q listed twice", lineNo, name))
			continue
		}
		h, err := parseHash(value)
		if err != nil {
			errs = append(errs, fmt.Errorf("line %d: %w", lineNo, err))
			continue
		}
		s.users[name] = h
		maxIter = max(maxIter, h.iterations)
	}
	if err := sc.Err(); err != nil {
		errs = append(errs, fmt.Errorf("read: %w", err))
	}
	if len(errs) == 0 && len(s.users) == 0 {
		errs = append(errs, errors.New("no senders defined"))
	}
	if len(errs) > 0 {
		return nil, errors.Join(errs...)
	}

	// Checking against the dummy costs as much as the most expensive real
	// hash. Its key is random, so no password matches it.
	s.dummy = hash{iterations: maxIter, salt: make([]byte, saltLen), key: make([]byte, keyLen)}
	_, _ = rand.Read(s.dummy.salt)
	_, _ = rand.Read(s.dummy.key)
	return s, nil
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

// Verify reports whether password is correct for the sender name. Unknown
// names take as long as a wrong password for a known name.
func (s *Senders) Verify(name, password string) bool {
	h, known := s.users[name]
	if !known || len(password) > maxPasswordLen {
		_ = check(s.dummy, password)
		return false
	}

	mac := s.mac(password)
	s.mu.Lock()
	cached := s.cache[name]
	s.mu.Unlock()
	if cached != nil && hmac.Equal(cached, mac) {
		return true
	}

	if !check(h, password) {
		return false
	}
	s.mu.Lock()
	s.cache[name] = mac
	s.mu.Unlock()
	return true
}

func (s *Senders) mac(password string) []byte {
	m := hmac.New(sha256.New, s.cacheKey)
	m.Write([]byte(password))
	return m.Sum(nil)
}

func check(h hash, password string) bool {
	key, err := pbkdf2.Key(sha256.New, password, h.salt, h.iterations, len(h.key))
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
