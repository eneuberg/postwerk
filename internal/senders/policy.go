package senders

import (
	"errors"
	"fmt"
	"path"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/eneuberg/postwerk/internal/senders/limit"
)

// Policy is what one sender may do (D63): the envelope senders it may use
// and how much it may send. It belongs to the sender, so both keys of a
// rotation share it. The zero value allows everything, as every sender could
// before 1.1.
//
// A Policy is a value: it is checked once, by NewPolicy or ParsePolicy, and
// cannot be changed afterwards.
type Policy struct {
	from         []string // lower-case path.Match patterns; empty: any
	perMinute    int      // messages per minute; 0: no limit
	rcptsPerHour int      // recipients per hour; 0: no limit
}

// Policy bounds.
const (
	MaxFromPatterns           = 32
	maxPatternLen             = 320
	MaxPerMinuteLimit         = 100_000
	MaxRecipientsPerHourLimit = 10_000_000
	// NullSender is the pattern that allows the null sender (MAIL FROM:<>).
	NullSender = "<>"
)

// Policy keys, in the senders file and in Policy.String.
const (
	keyFrom         = "from"
	keyPerMinute    = "max_per_minute"
	keyRcptsPerHour = "max_rcpt_per_hour"
)

// ErrInvalidPolicy is wrapped by every policy error; the texts are safe to
// show and never quote a pattern.
var ErrInvalidPolicy = errors.New("invalid policy")

// NewPolicy checks and returns a policy. from holds envelope sender patterns
// such as "noreply@example.com", "*@example.com" or "<>" (the null sender);
// they are compared in lower case with path.Match. Limits of 0 mean none.
func NewPolicy(from []string, perMinute, rcptsPerHour int) (Policy, error) {
	if len(from) > MaxFromPatterns {
		return Policy{}, fmt.Errorf("%w: at most %d from patterns", ErrInvalidPolicy, MaxFromPatterns)
	}
	p := Policy{perMinute: perMinute, rcptsPerHour: rcptsPerHour}
	for i, f := range from {
		f = strings.TrimSpace(f)
		if !utf8.ValidString(f) || foldsToASCII(f) {
			return Policy{}, fmt.Errorf("%w: from pattern %d: not valid UTF-8, or a character that lower-cases to ASCII "+
				"(such as the Kelvin sign)", ErrInvalidPolicy, i+1)
		}
		f = strings.ToLower(f)
		if err := checkPattern(f); err != nil {
			return Policy{}, fmt.Errorf("%w: from pattern %d: %v", ErrInvalidPolicy, i+1, err)
		}
		if !slices.Contains(p.from, f) {
			p.from = append(p.from, f)
		}
	}
	slices.Sort(p.from)
	if perMinute < 0 || perMinute > MaxPerMinuteLimit {
		return Policy{}, fmt.Errorf("%w: %s must be between 0 and %d", ErrInvalidPolicy, keyPerMinute, MaxPerMinuteLimit)
	}
	if rcptsPerHour < 0 || rcptsPerHour > MaxRecipientsPerHourLimit {
		return Policy{}, fmt.Errorf("%w: %s must be between 0 and %d", ErrInvalidPolicy, keyRcptsPerHour, MaxRecipientsPerHourLimit)
	}
	return p, nil
}

func checkPattern(f string) error {
	switch {
	case f == NullSender:
		return nil
	case f == "" || len(f) > maxPatternLen:
		return fmt.Errorf("empty or longer than %d bytes", maxPatternLen)
	case strings.Count(f, "@") != 1:
		return errors.New(`needs exactly one "@" (for example *@example.com)`)
	case strings.ContainsFunc(f, func(r rune) bool {
		return r <= ' ' || r == 0x7f || r == ',' || r == '"' || r == '<' || r == '>' || unicode.IsSpace(r)
	}):
		return errors.New("no spaces, control characters, commas, quotes or angle brackets")
	}
	if _, err := path.Match(f, ""); err != nil {
		return errors.New("not a valid pattern")
	}
	return nil
}

// ParsePolicy reads the text form, as written by String and in the senders
// file: space-separated key=value pairs, each key at most once, in any order:
//
//	from=*@example.com,noreply@example.org max_per_minute=60 max_rcpt_per_hour=500
//
// An empty text is the zero policy.
func ParsePolicy(s string) (Policy, error) {
	var from []string
	per, rcpts := 0, 0
	seen := map[string]bool{}
	for _, field := range strings.Fields(s) {
		k, v, ok := strings.Cut(field, "=")
		if !ok {
			return Policy{}, fmt.Errorf("%w: expected key=value", ErrInvalidPolicy)
		}
		if seen[k] {
			return Policy{}, fmt.Errorf("%w: %s given twice", ErrInvalidPolicy, k)
		}
		seen[k] = true
		switch k {
		case keyFrom:
			if v == "" {
				return Policy{}, fmt.Errorf("%w: %s is empty; leave it out to allow any sender", ErrInvalidPolicy, keyFrom)
			}
			from = strings.Split(v, ",")
		case keyPerMinute, keyRcptsPerHour:
			n, err := strconv.Atoi(v)
			if err != nil || v != strconv.Itoa(n) {
				return Policy{}, fmt.Errorf("%w: %s must be a whole number", ErrInvalidPolicy, k)
			}
			if k == keyPerMinute {
				per = n
			} else {
				rcpts = n
			}
		default:
			if len(k) > 32 || !nameRe.MatchString(k) {
				k = "?"
			}
			return Policy{}, fmt.Errorf("%w: unknown key %q (use %s, %s, %s)", ErrInvalidPolicy, k, keyFrom, keyPerMinute, keyRcptsPerHour)
		}
	}
	return NewPolicy(from, per, rcpts)
}

// String returns the canonical text form; ParsePolicy reads it back.
func (p Policy) String() string {
	var parts []string
	if len(p.from) > 0 {
		parts = append(parts, keyFrom+"="+strings.Join(p.from, ","))
	}
	if p.perMinute > 0 {
		parts = append(parts, keyPerMinute+"="+strconv.Itoa(p.perMinute))
	}
	if p.rcptsPerHour > 0 {
		parts = append(parts, keyRcptsPerHour+"="+strconv.Itoa(p.rcptsPerHour))
	}
	return strings.Join(parts, " ")
}

// Summary describes the policy without the patterns, for an event text
// (patterns look like addresses and would be redacted).
func (p Policy) Summary() string {
	return fmt.Sprintf("from_patterns=%d %s=%d %s=%d", len(p.from), keyPerMinute, p.perMinute, keyRcptsPerHour, p.rcptsPerHour)
}

// From returns the envelope sender patterns; empty means any.
func (p Policy) From() []string { return slices.Clone(p.from) }

// MaxPerMinute returns the message limit per minute; 0 means none.
func (p Policy) MaxPerMinute() int { return p.perMinute }

// MaxRecipientsPerHour returns the recipient limit per hour; 0 means none.
func (p Policy) MaxRecipientsPerHour() int { return p.rcptsPerHour }

// Equal reports whether p and q allow the same.
func (p Policy) Equal(q Policy) bool {
	return slices.Equal(p.from, q.from) && p.perMinute == q.perMinute && p.rcptsPerHour == q.rcptsPerHour
}

// AllowsFrom reports whether the envelope sender addr may be used. Without
// patterns every sender may. The null sender matches only the pattern "<>".
// An address with a character that lower-cases to ASCII never matches (it
// would match an ASCII pattern while the target sees another address, D60).
func (p Policy) AllowsFrom(addr string) bool {
	if len(p.from) == 0 {
		return true
	}
	if addr == "" {
		return slices.Contains(p.from, NullSender)
	}
	if foldsToASCII(addr) {
		return false
	}
	addr = strings.ToLower(addr)
	for _, f := range p.from {
		if f == NullSender {
			continue
		}
		if ok, _ := path.Match(f, addr); ok {
			return true
		}
	}
	return false
}

// MessageRate is the limit on messages (counted at MAIL).
func (p Policy) MessageRate() limit.Rate { return limit.Rate{N: p.perMinute, Per: time.Minute} }

// RecipientRate is the limit on recipients (counted at RCPT).
func (p Policy) RecipientRate() limit.Rate { return limit.Rate{N: p.rcptsPerHour, Per: time.Hour} }

// foldsToASCII reports whether s has a non-ASCII character that lower-cases
// to an ASCII one (the Kelvin sign K becomes k).
func foldsToASCII(s string) bool {
	for _, r := range s {
		if r >= utf8.RuneSelf && unicode.ToLower(r) < utf8.RuneSelf {
			return true
		}
	}
	return false
}
