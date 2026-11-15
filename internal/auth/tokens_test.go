package auth

import (
	"regexp"
	"strings"
	"testing"
)

func TestTokens(t *testing.T) {
	tok := GenerateToken()
	if !regexp.MustCompile(`^[A-Za-z0-9_-]{43}$`).MatchString(tok) || tok == GenerateToken() {
		t.Fatalf("token = %q", tok)
	}
	adminLine, err := TokenLine("ops", RoleAdmin, tok, MinIterations)
	if err != nil {
		t.Fatal(err)
	}
	readLine, err := TokenLine("support.team", RoleRead, "read-token", MinIterations)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(adminLine, "ops:admin:pbkdf2-sha256$100000$") || strings.Contains(adminLine, tok) {
		t.Fatalf("line = %q", adminLine)
	}
	ts, err := ParseTokens(strings.NewReader("# people\n" + adminLine + "\n\n" + readLine + "\n"))
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
	for _, c := range [][2]string{{"ops", "read-token"}, {"support.team", tok}, {"nobody", tok}, {"", ""}} {
		if role, ok := ts.Verify(c[0], c[1]); ok || role != "" {
			t.Errorf("Verify(%q) accepted", c[0])
		}
	}
	if _, err := TokenLine("Bad Name", RoleRead, tok, MinIterations); err == nil {
		t.Error("bad name accepted")
	}
	if _, err := TokenLine("ok", "write", tok, MinIterations); err == nil {
		t.Error("bad role accepted")
	}
}

func TestTokensCostTheSame(t *testing.T) {
	line, err := TokenLine("ops", RoleAdmin, "good", MinIterations)
	if err != nil {
		t.Fatal(err)
	}
	strong, err := TokenLine("strong", RoleRead, "other", 2*MinIterations)
	if err != nil {
		t.Fatal(err)
	}
	ts, err := ParseTokens(strings.NewReader(line + "\n" + strong + "\n"))
	if err != nil {
		t.Fatal(err)
	}
	var iterations int
	orig := derive
	derive = func(pw string, salt []byte, iter, n int) ([]byte, error) {
		iterations += iter
		return orig(pw, salt, iter, n)
	}
	t.Cleanup(func() { derive = orig })
	for _, c := range [][2]string{{"ops", "wrong"}, {"nobody", "wrong"}, {"", ""}, {"ops", "good"}} {
		iterations = 0
		ts.Verify(c[0], c[1])
		if iterations != 2*MinIterations {
			t.Errorf("Verify(%q) cost %d iterations, want %d", c[0], iterations, 2*MinIterations)
		}
	}
}

func TestParseTokensErrors(t *testing.T) {
	line, err := TokenLine("ops", RoleAdmin, "x", MinIterations)
	if err != nil {
		t.Fatal(err)
	}
	h := strings.SplitN(line, ":", 3)[2]
	for in, want := range map[string]string{
		"":                               "no tokens defined",
		"ops:" + h:                       "line 1: expected name:role:hash", // a senders-file line
		"ops-only":                       "line 1: expected name:role:hash",
		"Ops:read:" + h:                  "line 1: invalid token name",
		"ops:root:" + h:                  "line 1: role must be read or admin",
		"ops:read:plain-secret-token":    "line 1: expected pbkdf2-sha256",
		line + "\n" + line:               `line 2: token "ops" listed twice`,
		"ops:read:" + h + "\nbad\nx:y:z": "line 2",
	} {
		_, err := ParseTokens(strings.NewReader(in))
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("ParseTokens(%.30q) = %v, want %q", in, err, want)
		}
		if err != nil && strings.Contains(err.Error(), "plain-secret-token") {
			t.Errorf("error quotes the line: %v", err)
		}
	}
}
