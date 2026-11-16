package auth

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"strings"
)

// APIScheme is the hash scheme of the lines in the API tokens file.
const APIScheme = "sha256"

// APITokens holds the bearer tokens of the query API (H5, D41). The file has
// one line per calling application:
//
//	name:role:sha256$<digest>
//
// with role "read" or "admin" and digest the SHA-256 of the token in unpadded
// standard base64. An API token is 32 random bytes (GenerateToken), so a
// slow hash would add no protection: nobody can guess a 256-bit value, and a
// digest from a leaked file does not lead back to the token. A fast hash
// keeps wrong tokens from costing CPU.
//
// Every check costs the same for every name: one SHA-256 of the token and
// one constant-time comparison with 32 bytes, a random dummy for an unknown
// name. It is safe for concurrent use.
type APITokens struct {
	tokens map[string]apiToken
	dummy  []byte
}

type apiToken struct {
	digest []byte
	role   Role
}

// ParseAPITokens reads an API tokens file. A line with a PBKDF2 hash, as the
// file had before D41, is refused with a hint to make a new token. Errors
// name the line number but never quote the line.
func ParseAPITokens(r io.Reader) (*APITokens, error) {
	tokens := make(map[string]apiToken)
	errs := scanLines(r, func(lineNo int, line string) error {
		parts := strings.SplitN(line, ":", 3)
		if len(parts) != 3 {
			return fmt.Errorf("line %d: expected name:role:%s$<digest>", lineNo, APIScheme)
		}
		name, role := parts[0], Role(parts[1])
		if !ValidName(name) {
			return fmt.Errorf("line %d: invalid token name", lineNo)
		}
		if role != RoleRead && role != RoleAdmin {
			return fmt.Errorf("line %d: role must be read or admin", lineNo)
		}
		if _, dup := tokens[name]; dup {
			return fmt.Errorf("line %d: token %q listed twice", lineNo, name)
		}
		d, err := parseDigest(parts[2])
		if err != nil {
			return fmt.Errorf("line %d: %w", lineNo, err)
		}
		tokens[name] = apiToken{digest: d, role: role}
		return nil
	})
	if len(errs) == 0 && len(tokens) == 0 {
		errs = append(errs, errors.New("no tokens defined"))
	}
	if len(errs) > 0 {
		return nil, errors.Join(errs...)
	}
	t := &APITokens{tokens: tokens, dummy: make([]byte, sha256.Size)}
	_, _ = rand.Read(t.dummy)
	return t, nil
}

func parseDigest(v string) ([]byte, error) {
	if strings.HasPrefix(v, Scheme+"$") {
		return nil, fmt.Errorf("a %s hash; API tokens use %s now (D41): make a new token with "+
			"'postwerk gen-api-token NAME ROLE' and replace the line", Scheme, APIScheme)
	}
	digest, ok := strings.CutPrefix(v, APIScheme+"$")
	if !ok {
		return nil, fmt.Errorf("expected %s$<digest>", APIScheme)
	}
	d, err := base64.RawStdEncoding.Strict().DecodeString(digest)
	if err != nil || len(d) != sha256.Size {
		return nil, fmt.Errorf("digest must be %d bytes of unpadded base64", sha256.Size)
	}
	return d, nil
}

// digestToken is SHA-256; tests count the calls through it.
var digestToken = func(token string) []byte {
	d := sha256.Sum256([]byte(token))
	return d[:]
}

// Verify checks a token and returns the role of its name. Known and unknown
// names, right and wrong tokens take the same steps.
func (t *APITokens) Verify(name, token string) (Role, bool) {
	want, known := t.tokens[name]
	ref := t.dummy
	if known {
		ref = want.digest
	}
	match := subtle.ConstantTimeCompare(digestToken(token), ref) == 1
	if !known || !match {
		return "", false
	}
	return want.role, true
}

// Known reports whether name is in the file. It is for deciding whether a
// failed login may name the token in an event; it says nothing about the
// token itself.
func (t *APITokens) Known(name string) bool {
	_, ok := t.tokens[name]
	return ok
}

// Len returns the number of tokens.
func (t *APITokens) Len() int { return len(t.tokens) }

// APITokenLine returns the API tokens-file line for name, role and token.
func APITokenLine(name string, role Role, token string) (string, error) {
	if !ValidName(name) {
		return "", errors.New("auth: invalid token name (use [a-z0-9][a-z0-9._-]{0,63})")
	}
	if role != RoleRead && role != RoleAdmin {
		return "", errors.New("auth: role must be read or admin")
	}
	if token == "" {
		return "", errors.New("auth: empty token")
	}
	return name + ":" + string(role) + ":" + APIScheme + "$" + base64.RawStdEncoding.EncodeToString(digestToken(token)), nil
}
