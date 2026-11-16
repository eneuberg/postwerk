package auth

import (
	"strings"
	"testing"
)

func TestAPITokens(t *testing.T) {
	tok := GenerateToken()
	adminLine, err := APITokenLine("ops", RoleAdmin, tok)
	if err != nil {
		t.Fatal(err)
	}
	readLine, err := APITokenLine("support.team", RoleRead, "read-token")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(adminLine, "ops:admin:sha256$") || len(adminLine) != len("ops:admin:sha256$")+43 ||
		strings.Contains(adminLine, tok) {
		t.Fatalf("line = %q", adminLine)
	}
	ts, err := ParseAPITokens(strings.NewReader("# services\n" + adminLine + "\n\n" + readLine + "\n"))
	if err != nil {
		t.Fatal(err)
	}
	if ts.Len() != 2 || !ts.Known("ops") || ts.Known("nobody") {
		t.Fatal("names wrong")
	}
	if role, ok := ts.Verify("ops", tok); !ok || role != RoleAdmin {
		t.Fatalf("admin: %v %v", role, ok)
	}
	if role, ok := ts.Verify("support.team", "read-token"); !ok || role != RoleRead {
		t.Fatalf("read: %v %v", role, ok)
	}
	for _, c := range [][2]string{{"ops", "read-token"}, {"support.team", tok}, {"nobody", tok}, {"", ""}, {"ops", tok + "x"}} {
		if role, ok := ts.Verify(c[0], c[1]); ok || role != "" {
			t.Errorf("Verify(%q, %q) accepted", c[0], c[1])
		}
	}
	for _, c := range []struct {
		name  string
		role  Role
		token string
	}{{"Bad Name", RoleRead, tok}, {"ok", "write", tok}, {"ok", RoleRead, ""}} {
		if _, err := APITokenLine(c.name, c.role, c.token); err == nil {
			t.Errorf("APITokenLine(%q, %q, %q) accepted", c.name, c.role, c.token)
		}
	}
}

// Every check takes the same steps: one digest of the token and a compare
// with 32 bytes, for unknown names, wrong and right tokens alike; no PBKDF2.
func TestAPITokensCostTheSame(t *testing.T) {
	line, err := APITokenLine("ops", RoleAdmin, "good")
	if err != nil {
		t.Fatal(err)
	}
	ts, err := ParseAPITokens(strings.NewReader(line + "\n"))
	if err != nil {
		t.Fatal(err)
	}
	var digests, iterations int
	origDigest, origDerive := digestToken, derive
	digestToken = func(tok string) []byte { digests++; return origDigest(tok) }
	derive = func(pw string, salt []byte, iter, n int) ([]byte, error) {
		iterations += iter
		return origDerive(pw, salt, iter, n)
	}
	t.Cleanup(func() { digestToken, derive = origDigest, origDerive })
	for _, c := range [][2]string{{"ops", "wrong"}, {"nobody", "wrong"}, {"nobody", "good"}, {"", ""}, {"ops", "good"}} {
		digests, iterations = 0, 0
		ts.Verify(c[0], c[1])
		if digests != 1 || iterations != 0 {
			t.Errorf("Verify(%q, %q): %d digests, %d PBKDF2 iterations; want 1 and 0", c[0], c[1], digests, iterations)
		}
	}
	// An unknown name is compared with a random dummy of the same length,
	// never with nothing.
	if len(ts.dummy) != 32 || string(ts.dummy) == string(make([]byte, 32)) {
		t.Fatalf("dummy = %x", ts.dummy)
	}
}

func TestParseAPITokensErrors(t *testing.T) {
	line, err := APITokenLine("ops", RoleAdmin, "x")
	if err != nil {
		t.Fatal(err)
	}
	d := strings.SplitN(line, ":", 3)[2]
	old, err := TokenLine("ops", RoleRead, "x", MinIterations)
	if err != nil {
		t.Fatal(err)
	}
	for in, want := range map[string]string{
		"":                               "no tokens defined",
		"ops:" + d:                       "line 1: expected name:role:sha256$<digest>",
		"Ops:read:" + d:                  "line 1: invalid token name",
		"ops:root:" + d:                  "line 1: role must be read or admin",
		"ops:read:plain-secret-token":    "line 1: expected sha256$<digest>",
		"ops:read:sha256$c2hvcnQ":        "line 1: digest must be 32 bytes",
		old:                              "line 1: a pbkdf2-sha256 hash; API tokens use sha256 now (D41): make a new token with 'postwerk gen-api-token NAME ROLE'",
		line + "\n" + line:               `line 2: token "ops" listed twice`,
		"ops:read:" + d + "\nbad\nx:y:z": "line 2",
	} {
		_, err := ParseAPITokens(strings.NewReader(in))
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("ParseAPITokens(%.30q) = %v, want %q", in, err, want)
		}
		if err != nil && strings.Contains(err.Error(), "plain-secret-token") {
			t.Errorf("error quotes the line: %v", err)
		}
	}
}
