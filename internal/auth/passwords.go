package auth

import (
	"errors"
	"strings"
	"unicode/utf8"
)

// Password rules for UI accounts managed in the admin panel (U19, D35). The
// minimum is the single-factor minimum of NIST SP 800-63B-4, section
// 3.1.1.2; length counts Unicode code points, as the guideline says. There
// are no composition rules.
const (
	MinPasswordChars = 15
	MaxPasswordChars = 128
)

// Errors of CheckPassword; their texts are safe to show.
var (
	ErrPasswordShort  = errors.New("the password must have at least 15 characters")
	ErrPasswordLong   = errors.New("the password must have at most 128 characters")
	ErrPasswordText   = errors.New("the password is not valid text")
	ErrPasswordIsName = errors.New("the password must not be the account name")
)

// CheckPassword applies the password rules for the account name.
func CheckPassword(name, password string) error {
	if !utf8.ValidString(password) {
		return ErrPasswordText
	}
	n := utf8.RuneCountInString(password)
	switch {
	case n < MinPasswordChars:
		return ErrPasswordShort
	case n > MaxPasswordChars:
		return ErrPasswordLong
	case strings.EqualFold(strings.TrimSpace(password), name):
		return ErrPasswordIsName
	}
	return nil
}

// Checker checks passwords against hashes kept elsewhere (the UI accounts
// in the database) at one fixed cost, like Senders does for its file: a
// hash with fewer iterations pays the difference, and a missing or broken
// hash is checked against a dummy with the full count. So the time of a
// check says nothing about whether the name exists. It is safe for
// concurrent use.
type Checker struct {
	dummy hash
}

// NewChecker returns a Checker whose every check costs iterations PBKDF2
// rounds (clamped to MinIterations and MaxIterations).
func NewChecker(iterations int) *Checker {
	iterations = min(max(iterations, MinIterations), MaxIterations)
	return &Checker{dummy: newDummy(iterations)}
}

// Iterations returns the cost of one check.
func (c *Checker) Iterations() int { return c.dummy.iterations }

// Verify reports whether password matches encoded, a value made by Hash.
// An empty or malformed encoded value never matches and costs the same.
func (c *Checker) Verify(encoded, password string) bool {
	h, err := parseHash(encoded)
	if encoded == "" || err != nil || len(password) > maxPasswordLen {
		_ = check(c.dummy, password)
		return false
	}
	ok := check(h, password)
	c.Pad(h.iterations)
	return ok
}

// Pad spends the PBKDF2 rounds between done and the checker's cost, so a
// check made elsewhere with fewer rounds (the tokens file) costs the same.
func (c *Checker) Pad(done int) {
	if extra := c.dummy.iterations - done; extra > 0 {
		_, _ = derive("", c.dummy.salt, extra, keyLen)
	}
}

func newDummy(iterations int) hash {
	d := hash{iterations: iterations, salt: make([]byte, saltLen), key: make([]byte, keyLen)}
	fillRandom(d.salt)
	fillRandom(d.key)
	return d
}
