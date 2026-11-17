package senders

import (
	"regexp"
	"strings"
	"testing"
)

var keyRe = regexp.MustCompile(`^pwk_[a-z2-7]{12}_[a-z2-7]{52}$`)

func TestNewKey(t *testing.T) {
	seen := map[string]bool{}
	for range 100 {
		key, id := NewKey()
		if !keyRe.MatchString(key) || !strings.HasPrefix(key, KeyPrefix+id+"_") || len(key) != KeyLen {
			t.Fatalf("key %q id %q", key, id)
		}
		gotID, secret, ok := ParseKey(key)
		if !ok || gotID != id || len(secret) != secretChars {
			t.Fatalf("ParseKey(%q) = %q %q %v", key, gotID, secret, ok)
		}
		if seen[id] {
			t.Fatal("id repeated")
		}
		seen[id] = true
	}
}

func TestParseKey(t *testing.T) {
	good, _ := NewKey()
	for _, tc := range []struct {
		name string
		in   string
		ok   bool
	}{
		{"good", good, true},
		{"empty", "", false},
		{"password", "change-me-local-only", false},
		{"upper case", strings.ToUpper(good), false},
		{"wrong prefix", "pwx_" + good[4:], false},
		{"no separator", good[:16] + "x" + good[17:], false},
		{"too short", good[:len(good)-1], false},
		{"too long", good + "a", false},
		{"digit 1 not in alphabet", good[:20] + "1" + good[21:], false},
		{"digit 8 in id", good[:5] + "8" + good[6:], false},
		{"padding", good[:len(good)-1] + "=", false},
		{"space", good[:len(good)-1] + " ", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, _, ok := ParseKey(tc.in); ok != tc.ok {
				t.Fatalf("ok = %v", ok)
			}
		})
	}
}

func TestKeyLine(t *testing.T) {
	key, id := NewKey()
	line, err := KeyLine("app", key)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(line, "app:"+KeyScheme+"$"+id+"$") || strings.Contains(line, key[len(KeyPrefix)+13:]) {
		t.Fatalf("line %q", line)
	}
	gotID, d, err := parseKeyHash(strings.TrimPrefix(line, "app:"))
	_, secret, _ := ParseKey(key)
	if err != nil || gotID != id || d != DigestOf(secret) {
		t.Fatalf("parse back: %v", err)
	}
	if _, err := KeyLine("Bad Name", key); err == nil {
		t.Fatal("bad name accepted")
	}
	if _, err := KeyLine("app", "not-a-key"); err == nil {
		t.Fatal("not a key accepted")
	}
}

func FuzzParseKey(f *testing.F) {
	k, _ := NewKey()
	for _, s := range []string{k, "", "pwk_", "pwk_aaaaaaaaaaaa_", strings.Repeat("a", KeyLen), "pwk_\xff"} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		id, secret, ok := ParseKey(s)
		if !ok {
			if id != "" || secret != "" {
				t.Fatal("parts returned for a refused key")
			}
			return
		}
		if !keyRe.MatchString(s) || KeyPrefix+id+"_"+secret != s {
			t.Fatalf("accepted %q", s)
		}
	})
}

func FuzzParseKeyHash(f *testing.F) {
	f.Add(KeyScheme + "$aaaaaaaaaaaa$" + strings.Repeat("A", 43))
	f.Add("pwk-sha256$$")
	f.Fuzz(func(t *testing.T, s string) {
		id, _, err := parseKeyHash(s)
		if err == nil && !ValidKeyID(id) {
			t.Fatalf("accepted id %q", id)
		}
	})
}
