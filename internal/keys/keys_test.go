package keys

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

const (
	lineA = "a1:AAECAwQFBgcICQoLDA0ODxAREhMUFRYXGBkaGxwdHh8="
	lineB = "b2:ICEiIyQlJicoKSorLC0uLzAxMjM0NTY3ODk6Ozw9Pj8="
)

func TestParse(t *testing.T) {
	r, err := Parse([]byte("# store keys\n\n" + lineA + "\r\n  " + lineB + "  \n"))
	if err != nil {
		t.Fatal(err)
	}
	if r.Active().ID != "a1" || len(r.All()) != 2 || len(r.Active().Secret()) != Size {
		t.Fatalf("ring = %v", r)
	}
	if k, ok := r.Lookup("b2"); !ok || k.Secret()[0] != 0x20 {
		t.Fatal("lookup b2")
	}
	if _, ok := r.Lookup("zz"); ok {
		t.Fatal("unknown id found")
	}
	other, _ := Parse([]byte(lineB))
	if !r.Shares(other) {
		t.Fatal("shared key not detected")
	}
	only, _ := Parse([]byte(lineA))
	if only.Shares(other) {
		t.Fatal("different keys reported as shared")
	}
}

func TestParseErrors(t *testing.T) {
	tests := map[string]string{
		"":                               "no key",
		"# only a comment\n":             "no key",
		"nocolon":                        "line 1: expected",
		"UPPER:" + lineA[3:]:             "line 1: key id",
		"waytoolongkeyid17:" + lineA[3:]: "line 1: key id",
		"a1:c2hvcnQ=":                    "line 1: key must be 32 bytes",
		"a1:" + lineA[3:46]:              "line 1: key must be 32 bytes", // unpadded
		lineA + "\n" + lineA:             "line 2: key id a1 used twice",
	}
	for in, want := range tests {
		_, err := Parse([]byte(in))
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("Parse(%q) = %v, want %q", in, err, want)
		}
		if err != nil && strings.Contains(err.Error(), "AAEC") {
			t.Errorf("error quotes key material: %v", err)
		}
	}
}

func TestKeysDoNotPrint(t *testing.T) {
	r, _ := Parse([]byte(lineA))
	k := r.Active()
	for _, s := range []string{
		fmt.Sprint(k), fmt.Sprintf("%v %+v %#v %s", k, k, k, k), fmt.Sprint(r), fmt.Sprintf("%#v %+v", r, r),
		fmt.Sprintf("%+v", struct{ K Key }{k}),
	} {
		if strings.Contains(s, "AAEC") || strings.Contains(s, "[0 1 2") || !strings.Contains(s, "a1") {
			t.Fatalf("printed: %q", s)
		}
	}
}

func TestGenerate(t *testing.T) {
	now := time.Date(2026, 11, 15, 0, 0, 0, 0, time.UTC)
	seen := map[string]bool{}
	for range 50 {
		line := Generate(now)
		r, err := Parse([]byte(line))
		if err != nil {
			t.Fatalf("%q: %v", line, err)
		}
		id := r.Active().ID
		if !strings.HasPrefix(id, "20261115") || len(id) != 12 {
			t.Fatalf("id = %q", id)
		}
		if seen[line] {
			t.Fatal("duplicate key")
		}
		seen[line] = true
	}
}
