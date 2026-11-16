package auth

import (
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"maps"
	"slices"
	"strings"
)

// Role is what a UI token may do (U4, U12).
type Role string

// Roles.
const (
	// RoleRead sees every page, with addresses masked.
	RoleRead Role = "read"
	// RoleAdmin sees addresses in full (and later may resend and close).
	RoleAdmin Role = "admin"
)

// Tokens holds the tokens for the UI and the later query API. The tokens
// file has one line per person or team:
//
//	name:role:pbkdf2-sha256$<iterations>$<salt>$<key>
//
// with role "read" or "admin" and the hash as in the senders file. Checks
// cost the same for every name, like sender logins (I12): the token is a
// password with a role.
type Tokens struct {
	check *Senders
	roles map[string]Role
}

// ParseTokens reads a tokens file. Errors name the line number but never
// quote the line.
func ParseTokens(r io.Reader) (*Tokens, error) {
	users := make(map[string]hash)
	roles := make(map[string]Role)
	errs := scanLines(r, func(lineNo int, line string) error {
		parts := strings.SplitN(line, ":", 3)
		if len(parts) != 3 {
			return fmt.Errorf("line %d: expected name:role:hash", lineNo)
		}
		name, role := parts[0], Role(parts[1])
		if !ValidName(name) {
			return fmt.Errorf("line %d: invalid token name", lineNo)
		}
		if role != RoleRead && role != RoleAdmin {
			return fmt.Errorf("line %d: role must be read or admin", lineNo)
		}
		if _, dup := users[name]; dup {
			return fmt.Errorf("line %d: token %q listed twice", lineNo, name)
		}
		h, err := parseHash(parts[2])
		if err != nil {
			return fmt.Errorf("line %d: %w", lineNo, err)
		}
		users[name], roles[name] = h, role
		return nil
	})
	if len(errs) == 0 && len(users) == 0 {
		errs = append(errs, errors.New("no tokens defined"))
	}
	if len(errs) > 0 {
		return nil, errors.Join(errs...)
	}
	return &Tokens{check: newSenders(users), roles: roles}, nil
}

// Verify checks a token and returns the role of its name. It costs PBKDF2
// with the strongest iteration count in the file for every name, known or
// not, unless the same login succeeded before.
func (t *Tokens) Verify(name, token string) (Role, bool) {
	if !t.check.Verify(name, token) {
		return "", false
	}
	return t.roles[name], true
}

// Known reports whether name is in the file. It is for deciding whether a
// failed login may name the token in an event; it says nothing about the
// token itself.
func (t *Tokens) Known(name string) bool {
	_, ok := t.roles[name]
	return ok
}

// Iterations returns the cost of one Verify: the strongest iteration count
// in the file. Callers that also check other hashes pad those to the same
// cost (Checker).
func (t *Tokens) Iterations() int { return t.check.dummy.iterations }

// Names returns the names in the file, sorted.
func (t *Tokens) Names() []string { return slices.Sorted(maps.Keys(t.roles)) }

// Role returns the role of name, empty if name is not in the file.
func (t *Tokens) Role(name string) Role { return t.roles[name] }

// Len returns the number of tokens.
func (t *Tokens) Len() int { return len(t.roles) }

// GenerateToken returns a new random token: 32 bytes from the operating
// system's random source, unpadded base64url (43 characters).
func GenerateToken() string {
	b := make([]byte, 32)
	_, _ = rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}

// TokenLine returns the tokens-file line for name, role and token.
func TokenLine(name string, role Role, token string, iterations int) (string, error) {
	if !ValidName(name) {
		return "", errors.New("auth: invalid token name (use [a-z0-9][a-z0-9._-]{0,63})")
	}
	if role != RoleRead && role != RoleAdmin {
		return "", errors.New("auth: role must be read or admin")
	}
	h, err := Hash(token, iterations)
	if err != nil {
		return "", err
	}
	return name + ":" + string(role) + ":" + h, nil
}
