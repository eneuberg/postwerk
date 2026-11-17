package senders

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/eneuberg/postwerk/internal/senders/limit"
)

func TestParsePolicy(t *testing.T) {
	for _, tc := range []struct {
		in, want string // want "" with ok: the zero policy
		ok       bool
	}{
		{"", "", true},
		{"   ", "", true},
		{"from=*@example.com", "from=*@example.com", true},
		{"from=B@Example.com,*@example.com,b@example.com", "from=*@example.com,b@example.com", true},
		{"max_per_minute=60 max_rcpt_per_hour=500", "max_per_minute=60 max_rcpt_per_hour=500", true},
		{"max_rcpt_per_hour=5 from=<> max_per_minute=0", "from=<> max_rcpt_per_hour=5", true},
		{"from=", "", false},
		{"from=a@b from=c@d", "", false},
		{"max_per_minute=-1", "", false},
		{"max_per_minute=1.5", "", false},
		{"max_per_minute=+5", "", false},
		{"max_per_minute=100001", "", false},
		{"max_rcpt_per_hour=99999999999999999999", "", false},
		{"rate=5", "", false},
		{"from", "", false},
		{"from=example.com", "", false},
		{"from=a@b@c", "", false},
		{"from=a\"b@c", "", false},
		{"from=[a@b", "", false},
		{"from=K@example.com", "", false}, // Kelvin sign
		{"from=\xff@example.com", "", false},
		{"from=" + strings.Repeat("a@b,", 33) + "a@b", "", false},
	} {
		t.Run(tc.in, func(t *testing.T) {
			p, err := ParsePolicy(tc.in)
			if (err == nil) != tc.ok {
				t.Fatalf("err = %v", err)
			}
			if err != nil {
				if !errors.Is(err, ErrInvalidPolicy) {
					t.Fatalf("error not ErrInvalidPolicy: %v", err)
				}
				return
			}
			if p.String() != tc.want {
				t.Fatalf("String = %q, want %q", p.String(), tc.want)
			}
		})
	}
}

func TestAllowsFrom(t *testing.T) {
	p, err := NewPolicy([]string{"*@example.com", "noreply@example.org"}, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	null, _ := NewPolicy([]string{"<>", "a@example.com"}, 0, 0)
	for _, tc := range []struct {
		p    Policy
		addr string
		want bool
	}{
		{Policy{}, "anyone@anywhere.test", true},
		{Policy{}, "", true},
		{p, "x@example.com", true},
		{p, "X@EXAMPLE.COM", true},
		{p, "noreply@example.org", true},
		{p, "other@example.org", false},
		{p, "x@sub.example.com", false},
		{p, "x@example.com.evil.test", false},
		{p, "", false},
		{p, "K@example.com", false}, // folds to k@example.com
		{null, "", true},
		{null, "a@example.com", true},
		{null, "b@example.com", false},
	} {
		if got := tc.p.AllowsFrom(tc.addr); got != tc.want {
			t.Errorf("%q AllowsFrom(%q) = %v", tc.p, tc.addr, got)
		}
	}
}

func TestPolicyRatesAndSummary(t *testing.T) {
	p, err := NewPolicy([]string{"a@b.test"}, 60, 500)
	if err != nil {
		t.Fatal(err)
	}
	if p.MessageRate() != (limit.Rate{N: 60, Per: time.Minute}) || p.RecipientRate() != (limit.Rate{N: 500, Per: time.Hour}) {
		t.Fatal("rates")
	}
	if !(Policy{}).MessageRate().Unlimited() || !(Policy{}).RecipientRate().Unlimited() {
		t.Fatal("zero policy limits")
	}
	if s := p.Summary(); strings.Contains(s, "@") || s != "from_patterns=1 max_per_minute=60 max_rcpt_per_hour=500" {
		t.Fatalf("summary %q", s)
	}
	q, _ := ParsePolicy(p.String())
	if !p.Equal(q) || p.Equal(Policy{}) {
		t.Fatal("Equal")
	}
	from := p.From()
	from[0] = "changed@x"
	if p.From()[0] != "a@b.test" {
		t.Fatal("From exposes the policy's slice")
	}
}

func FuzzPolicy(f *testing.F) {
	for _, s := range []string{"", "from=*@example.com max_per_minute=5", "from=<>,[a-z]*@x.test", "from=a@b,,c@d",
		"max_rcpt_per_hour=0", "from=\\@x"} {
		f.Add(s, "user@example.com")
	}
	f.Fuzz(func(t *testing.T, s, addr string) {
		p, err := ParsePolicy(s)
		if err != nil {
			return
		}
		// The canonical form reads back to the same policy.
		q, err := ParsePolicy(p.String())
		if err != nil || !p.Equal(q) {
			t.Fatalf("%q: round trip %q: %v", s, p.String(), err)
		}
		_ = p.AllowsFrom(addr) // never panics
		if len(p.From()) == 0 && !p.AllowsFrom(addr) {
			t.Fatal("no patterns refused an address")
		}
	})
}
