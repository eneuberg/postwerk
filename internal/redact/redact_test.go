package redact

import (
	"strings"
	"testing"
	"unicode/utf8"
)

func TestText(t *testing.T) {
	tests := []struct {
		in, want string
	}{
		{"", ""},
		{"250 2.0.0 OK", "250 2.0.0 OK"},
		{"5.1.1 <user@example.com>: Recipient address rejected", "5.1.1 <redacted>: Recipient address rejected"},
		{"user@example.com unknown", "<redacted> unknown"},
		{"to=<a.b+tag@sub.example.co.uk>, relay=x", "to=<redacted>, relay=x"},
		{"rcpt:bounce+user=example.com@bounce.example.net rejected", "rcpt:<redacted> rejected"},
		{"two a@x.test and b@y.test", "two <redacted> and <redacted>"},
		{"line1\r\nline2\tend", "line1 line2 end"},
		{"bell\x07 and nul\x00 and del\x7f", "bell and nul and del"},
		{"  many   spaces  ", "many spaces"},
		{"bad utf8 \xff\xfe here", "bad utf8 ? here"},
		{"c1 \u0085 sep\u2028x", "c1 sep x"},
		{"quoted \"x y\"@example.com", "quoted <redacted>"},
		{"550 <\"jane doe\"@example.com>: rejected", "550 <redacted>: rejected"},
		{"said \"hello\" to a@b.test", "said \"hello\" to <redacted>"},
		{"550 5.1.1 <jörg@exämple.test>: unknown", "550 5.1.1 <redacted>: unknown"},
		{"用户@例子.广告 rejected", "<redacted> rejected"},
		{"to\u00a0用户@例子.广告\u3000rejected", "to <redacted> rejected"},
		{"кто-то@пример.рф,ünï@b.test", "<redacted>,<redacted>"},
	}
	for _, tt := range tests {
		if got := Text(tt.in); got != tt.want {
			t.Errorf("Text(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestTextLength(t *testing.T) {
	long := strings.Repeat("ä", 1000) // 2 bytes each
	got := Text(long)
	if len(got) > MaxLen {
		t.Fatalf("len = %d, want <= %d", len(got), MaxLen)
	}
	if !utf8.ValidString(got) {
		t.Fatalf("result is not valid UTF-8")
	}

	// An address at the end of a very long text never survives in part.
	in := strings.Repeat("x ", 2040) + "someone@example.com"
	if got := Text(in); strings.Contains(got, "@") || strings.Contains(got, "someone") {
		t.Fatalf("address leaked: %q", got)
	}
}

func TestTextNoAddressSurvives(t *testing.T) {
	inputs := []string{
		"<canary-leak@example.test>",
		"550 5.1.1 canary-leak@example.test... User unknown",
		"RCPT TO:<canary-leak@example.test> failed",
		"(canary-leak@example.test)",
		"'canary-leak@example.test'",
		"[canary-leak@example.test]",
	}
	for _, in := range inputs {
		if got := Text(in); strings.Contains(got, "canary-leak") || strings.Contains(got, "@") {
			t.Errorf("Text(%q) = %q, address survived", in, got)
		}
	}
}
