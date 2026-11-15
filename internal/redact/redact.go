// Package redact cleans free text from other systems, such as SMTP replies
// and network errors, before it is stored, logged or exported (S5).
//
// Anything that looks like a mail address becomes "<redacted>", control
// characters are removed and the result is cut to MaxLen bytes. The rule for
// addresses is deliberately broad: every run of characters around an "@" is
// replaced, so a reply such as "550 5.1.1 <user@example.com>: unknown" keeps
// its codes and loses the address.
package redact

import (
	"regexp"
	"strings"
	"unicode/utf8"
)

// MaxLen is the maximum length of a redacted text in bytes.
const MaxLen = 300

// Placeholder replaces every address.
const Placeholder = "<redacted>"

// maxInput bounds the work done on very long inputs. Text beyond it would be
// cut anyway.
const maxInput = 4096

// addrRe matches a token that contains "@", with optional angle brackets.
// The token ends at white space, brackets, quotes and common separators. A
// quoted local part ("jane doe"@example.com) counts as part of the token.
var addrRe = regexp.MustCompile(`<?(?:"[^"]*"|[^\s<>"'(),;:\[\]]*)@[^\s<>"'(),;:\[\]]*>?`)

// Text returns s with addresses replaced, control characters removed, runs of
// white space collapsed and the length cut to MaxLen bytes.
func Text(s string) string {
	if len(s) > maxInput {
		s = s[:maxInput]
	}
	s = strings.ToValidUTF8(s, "?")
	s = strings.Map(func(r rune) rune {
		switch {
		case r == '\t' || r == '\n' || r == '\r':
			return ' '
		case r < 0x20 || r == 0x7f || (r >= 0x80 && r < 0xa0):
			return -1
		case r == 0x2028 || r == 0x2029: // line and paragraph separators
			return ' '
		}
		return r
	}, s)
	s = addrRe.ReplaceAllLiteralString(s, Placeholder)
	s = strings.Join(strings.Fields(s), " ")
	return truncate(s, MaxLen)
}

// truncate cuts s to at most n bytes without splitting a UTF-8 sequence.
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}
