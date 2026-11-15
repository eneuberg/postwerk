package auth

import (
	"strings"
	"testing"
	"time"
)

func mustHash(t *testing.T, pw string) string {
	t.Helper()
	h, err := Hash(pw, MinIterations)
	if err != nil {
		t.Fatal(err)
	}
	return h
}

func TestHashVerify(t *testing.T) {
	file := "# senders\n\napp:" + mustHash(t, "s3cret") + "\nother-app.v2:" + mustHash(t, "pw:with:colons") + "\n"
	s, err := ParseSenders(strings.NewReader(file))
	if err != nil {
		t.Fatal(err)
	}
	if s.Len() != 2 {
		t.Fatalf("Len = %d", s.Len())
	}
	for range 2 { // second round uses the fast path
		if !s.Verify("app", "s3cret") {
			t.Fatal("correct password rejected")
		}
	}
	if !s.Verify("other-app.v2", "pw:with:colons") {
		t.Fatal("password with colons rejected")
	}
	if s.Verify("app", "wrong") {
		t.Fatal("wrong password accepted")
	}
	if s.Verify("app", "s3cret ") || s.Verify("app", "") {
		t.Fatal("near-miss password accepted")
	}
	if s.Verify("nobody", "s3cret") {
		t.Fatal("unknown user accepted")
	}
	if s.Verify("other-app.v2", "s3cret") {
		t.Fatal("password of another sender accepted")
	}
	if s.Verify("app", strings.Repeat("x", 5000)) {
		t.Fatal("overlong password accepted")
	}
}

func TestHashFormat(t *testing.T) {
	h := mustHash(t, "x")
	parts := strings.Split(h, "$")
	if len(parts) != 4 || parts[0] != "pbkdf2-sha256" || parts[1] != "100000" {
		t.Fatalf("hash = %q", h)
	}
	if h2 := mustHash(t, "x"); h2 == h {
		t.Fatal("same salt twice")
	}
	if _, err := Hash("", MinIterations); err == nil {
		t.Fatal("empty password hashed")
	}
	if _, err := Hash("x", 1000); err == nil {
		t.Fatal("low iteration count accepted")
	}
}

func TestDefaultIterations(t *testing.T) {
	h, err := Hash("x", DefaultIterations)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(h, "pbkdf2-sha256$600000$") {
		t.Fatalf("hash = %q", h)
	}
}

func TestParseErrors(t *testing.T) {
	good := mustHash(t, "pw")
	parts := strings.Split(good, "$")
	tests := map[string]string{
		"empty":           "# nothing\n",
		"no colon":        "app\n",
		"bad name upper":  "App:" + good + "\n",
		"bad name start":  "-app:" + good + "\n",
		"bad name long":   strings.Repeat("a", 65) + ":" + good + "\n",
		"duplicate":       "app:" + good + "\napp:" + good + "\n",
		"wrong scheme":    "app:bcrypt$" + strings.Join(parts[1:], "$") + "\n",
		"missing part":    "app:" + strings.Join(parts[:3], "$") + "\n",
		"low iterations":  "app:pbkdf2-sha256$1000$" + parts[2] + "$" + parts[3] + "\n",
		"iterations text": "app:pbkdf2-sha256$many$" + parts[2] + "$" + parts[3] + "\n",
		"bad salt":        "app:pbkdf2-sha256$100000$!!$" + parts[3] + "\n",
		"short key":       "app:pbkdf2-sha256$100000$" + parts[2] + "$AAAA\n",
		"padded key":      "app:pbkdf2-sha256$100000$" + parts[2] + "$" + parts[3] + "=\n",
		"plaintext":       "app:hunter2\n",
	}
	for name, file := range tests {
		t.Run(name, func(t *testing.T) {
			_, err := ParseSenders(strings.NewReader(file))
			if err == nil {
				t.Fatal("no error")
			}
			if strings.Contains(err.Error(), "hunter2") || strings.Contains(err.Error(), parts[3]) {
				t.Fatalf("error quotes secret: %v", err)
			}
		})
	}
}

func TestParseReportsAllLines(t *testing.T) {
	_, err := ParseSenders(strings.NewReader("a\nb\nc:" + mustHash(t, "x") + "\nd\n"))
	if err == nil {
		t.Fatal("no error")
	}
	for _, want := range []string{"line 1", "line 2", "line 4"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %s", err, want)
		}
	}
}

func TestReadPassword(t *testing.T) {
	tests := map[string]string{
		"pw\n":        "pw",
		"pw\r\n":      "pw",
		"pw":          "pw",
		"p w\nmore\n": "p w",
	}
	for in, want := range tests {
		got, err := ReadPassword(strings.NewReader(in))
		if err != nil || got != want {
			t.Errorf("ReadPassword(%q) = %q, %v", in, got, err)
		}
	}
	for _, in := range []string{"", "\n", strings.Repeat("x", 2000)} {
		if _, err := ReadPassword(strings.NewReader(in)); err == nil {
			t.Errorf("ReadPassword(%.10q…) accepted", in)
		}
	}
}

// TestUnknownUserCostsTheSame checks, with a generous bound, that an unknown
// name pays for a full key derivation like a wrong password does.
func TestUnknownUserCostsTheSame(t *testing.T) {
	s, err := ParseSenders(strings.NewReader("app:" + mustHash(t, "pw") + "\n"))
	if err != nil {
		t.Fatal(err)
	}
	measure := func(name string) time.Duration {
		best := time.Duration(1 << 62)
		for range 3 {
			start := time.Now()
			s.Verify(name, "wrong")
			best = min(best, time.Since(start))
		}
		return best
	}
	known, unknown := measure("app"), measure("nobody")
	if unknown < known/3 {
		t.Fatalf("unknown user took %v, wrong password %v", unknown, known)
	}
}
