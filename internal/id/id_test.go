package id

import (
	"strings"
	"testing"
	"time"
)

func TestShape(t *testing.T) {
	for range 1000 {
		s := New()
		if !Valid(s) {
			t.Fatalf("New() = %q, not valid", s)
		}
		if len(s) != 29 || !strings.HasPrefix(s, "pw_") {
			t.Fatalf("New() = %q, wrong shape", s)
		}
	}
}

func TestUnique(t *testing.T) {
	seen := make(map[string]bool)
	now := time.Now()
	for range 10000 {
		s := NewAt(now)
		if seen[s] {
			t.Fatalf("duplicate id %q", s)
		}
		seen[s] = true
	}
}

func TestOrder(t *testing.T) {
	base := time.UnixMilli(1_763_000_000_000)
	prev := NewAt(base)
	for i := 1; i < 500; i++ {
		next := NewAt(base.Add(time.Duration(i) * time.Millisecond))
		if next <= prev {
			t.Fatalf("ids not ordered: %q then %q", prev, next)
		}
		prev = next
	}
}

func TestEncodeKnown(t *testing.T) {
	var zero [16]byte
	if got := encode(zero); got != strings.Repeat("0", 26) {
		t.Errorf("encode(zero) = %q", got)
	}
	var max [16]byte
	for i := range max {
		max[i] = 0xff
	}
	if got := encode(max); got != "7"+strings.Repeat("Z", 25) {
		t.Errorf("encode(max) = %q", got)
	}
	// The time part of an id is its first 10 characters.
	got := NewAt(time.UnixMilli(1))[3:13]
	if got != "0000000001" {
		t.Errorf("time part for 1 ms = %q", got)
	}
}

func TestValid(t *testing.T) {
	good := "pw_" + "01JCX4" + strings.Repeat("A", 20)
	if !Valid(good) {
		t.Fatalf("Valid(%q) = false", good)
	}
	bad := []string{
		"",
		"pw_",
		"px_" + good[3:],
		"PW_" + good[3:],
		good + "A",
		good[:len(good)-1],
		"pw_8" + good[4:],           // first char above '7'
		"pw_" + "01jcx4" + good[9:], // lower case
		good[:10] + "I" + good[11:],
		good[:10] + "L" + good[11:],
		good[:10] + "O" + good[11:],
		good[:10] + "U" + good[11:],
		good[:10] + "-" + good[11:],
		good[:10] + "\x00" + good[11:],
	}
	for _, s := range bad {
		if Valid(s) {
			t.Errorf("Valid(%q) = true", s)
		}
	}
}
