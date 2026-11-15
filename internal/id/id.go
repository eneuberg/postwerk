// Package id creates and checks Postwerk mail ids.
//
// An id is "pw_" followed by 26 characters of Crockford base32. The 128 bits
// behind them are a 48-bit Unix time in milliseconds and 80 random bits from
// crypto/rand, so ids sort by creation time (to the millisecond) and cannot be
// guessed.
package id

import (
	"crypto/rand"
	"time"
)

// Prefix starts every mail id.
const Prefix = "pw_"

// Len is the length of a mail id in bytes.
const Len = len(Prefix) + encodedLen

const (
	encodedLen = 26
	alphabet   = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"
)

// New returns a new id for the current time.
func New() string {
	return NewAt(time.Now())
}

// NewAt returns a new id for time t with fresh random bits.
func NewAt(t time.Time) string {
	var b [16]byte
	ms := uint64(t.UnixMilli())
	for i := 5; i >= 0; i-- {
		b[i] = byte(ms)
		ms >>= 8
	}
	// crypto/rand.Read never returns an error and always fills the buffer.
	_, _ = rand.Read(b[6:])
	return Prefix + encode(b)
}

// encode writes 128 bits as 26 base32 characters. The first character holds
// the top 3 bits, so it is at most '7'.
func encode(b [16]byte) string {
	var out [encodedLen]byte
	// Treat b as a 128-bit big-endian number and take 5 bits at a time from
	// the low end. 26*5 = 130 bits, the top two are always zero.
	hi := uint64(b[0])<<56 | uint64(b[1])<<48 | uint64(b[2])<<40 | uint64(b[3])<<32 |
		uint64(b[4])<<24 | uint64(b[5])<<16 | uint64(b[6])<<8 | uint64(b[7])
	lo := uint64(b[8])<<56 | uint64(b[9])<<48 | uint64(b[10])<<40 | uint64(b[11])<<32 |
		uint64(b[12])<<24 | uint64(b[13])<<16 | uint64(b[14])<<8 | uint64(b[15])
	for i := encodedLen - 1; i >= 0; i-- {
		out[i] = alphabet[lo&0x1f]
		lo = lo>>5 | hi<<59
		hi >>= 5
	}
	return string(out[:])
}

// Valid reports whether s has the exact shape of a mail id: the prefix, 26
// upper-case Crockford base32 characters and a first character of at most '7'.
func Valid(s string) bool {
	if len(s) != Len || s[:len(Prefix)] != Prefix {
		return false
	}
	body := s[len(Prefix):]
	if body[0] < '0' || body[0] > '7' {
		return false
	}
	for i := 0; i < len(body); i++ {
		if !isAlphabet(body[i]) {
			return false
		}
	}
	return true
}

func isAlphabet(c byte) bool {
	switch {
	case c >= '0' && c <= '9':
		return true
	case c >= 'A' && c <= 'Z':
		return c != 'I' && c != 'L' && c != 'O' && c != 'U'
	}
	return false
}
