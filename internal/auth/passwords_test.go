package auth

import (
	"errors"
	"strings"
	"testing"
)

func TestCheckPassword(t *testing.T) {
	for pw, want := range map[string]error{
		strings.Repeat("a", 14):           ErrPasswordShort,
		strings.Repeat("a", 15):           nil,
		"correct horse battery":           nil,
		strings.Repeat("ä", 15):           nil, // code points, not bytes
		strings.Repeat("ä", 14):           ErrPasswordShort,
		strings.Repeat("x", 128):          nil,
		strings.Repeat("x", 129):          ErrPasswordLong,
		"operations.team":                 ErrPasswordIsName,
		"OPERATIONS.TEAM":                 ErrPasswordIsName,
		" operations.team ":               ErrPasswordIsName,
		"operations.team!":                nil,
		"\xff" + strings.Repeat("a", 20):  ErrPasswordText,
		"no digits no capitals no symbol": nil, // no composition rules
	} {
		if got := CheckPassword("operations.team", pw); !errors.Is(got, want) {
			t.Errorf("CheckPassword(%q) = %v, want %v", pw, got, want)
		}
	}
}

// Every check costs the checker's iteration count: a weaker hash pays the
// difference, a missing or broken hash pays the dummy.
func TestCheckerCost(t *testing.T) {
	weak, err := Hash("weak-password-123", MinIterations)
	if err != nil {
		t.Fatal(err)
	}
	strong, err := Hash("strong-password-123", 2*MinIterations)
	if err != nil {
		t.Fatal(err)
	}
	c := NewChecker(2 * MinIterations)
	if c.Iterations() != 2*MinIterations || NewChecker(1).Iterations() != MinIterations {
		t.Fatal("iterations not clamped")
	}
	var iterations int
	orig := derive
	derive = func(pw string, salt []byte, iter, n int) ([]byte, error) {
		iterations += iter
		return orig(pw, salt, iter, n)
	}
	t.Cleanup(func() { derive = orig })
	for _, tt := range []struct {
		hash, pw string
		ok       bool
	}{
		{weak, "weak-password-123", true}, {weak, "wrong", false}, {strong, "strong-password-123", true},
		{strong, "wrong", false}, {"", "anything", false}, {"pbkdf2-sha256$broken", "x", false},
		{weak, strings.Repeat("x", 2000), false},
	} {
		iterations = 0
		if got := c.Verify(tt.hash, tt.pw); got != tt.ok {
			t.Errorf("Verify(%.20q, %.10q) = %v", tt.hash, tt.pw, got)
		}
		if iterations != 2*MinIterations {
			t.Errorf("Verify(%.20q) cost %d iterations", tt.hash, iterations)
		}
	}
	iterations = 0
	c.Pad(MinIterations)
	if iterations != MinIterations {
		t.Fatalf("Pad cost %d", iterations)
	}
}
