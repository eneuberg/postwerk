package addrhash

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"testing"

	"github.com/eneuberg/postwerk/internal/keys"
)

const (
	lineNew = "h2:AAECAwQFBgcICQoLDA0ODxAREhMUFRYXGBkaGxwdHh8="
	lineOld = "h1:ICEiIyQlJicoKSorLC0uLzAxMjM0NTY3ODk6Ozw9Pj8="
)

func hasher(t *testing.T, file string) *Hasher {
	t.Helper()
	r, err := keys.Parse([]byte(file))
	if err != nil {
		t.Fatal(err)
	}
	return New(r)
}

func TestNormalize(t *testing.T) {
	for in, want := range map[string]string{
		"Jane.Doe@Example.COM":   "jane.doe@example.com",
		"  jane@example.com\r\n": "jane@example.com",
		"JÖRG@Bücher.example":    "jÖrg@bücher.example", // only A–Z are folded
		"":                       "",
	} {
		if got := Normalize(in); got != want {
			t.Errorf("Normalize(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestHash(t *testing.T) {
	h := hasher(t, lineNew)
	got := h.Hash(" Jane@Example.com ")
	// Independent computation: HMAC-SHA256(key, normalized address).
	key, _ := keys.Parse([]byte(lineNew))
	m := hmac.New(sha256.New, key.Active().Secret())
	m.Write([]byte("jane@example.com"))
	if want := "h2:" + hex.EncodeToString(m.Sum(nil)); got != want {
		t.Fatalf("hash = %s, want %s", got, want)
	}
	if h.Hash("jane@example.com") != got {
		t.Fatal("hash depends on case")
	}
	if h.Hash("john@example.com") == got {
		t.Fatal("different addresses, same hash")
	}
	if hasher(t, lineOld).Hash("jane@example.com") == got {
		t.Fatal("different keys, same hash")
	}
}

func TestLookupAfterRotation(t *testing.T) {
	stored := hasher(t, lineOld).Hash("jane@example.com") // written before the rotation
	rotated := hasher(t, lineNew+"\n"+lineOld)
	n, hashes := rotated.Lookup("JANE@example.com")
	if n != "jane@example.com" || len(hashes) != 2 || hashes[0][:3] != "h2:" {
		t.Fatalf("lookup = %q %q", n, hashes)
	}
	if hashes[1] != stored {
		t.Fatal("old hash not among the lookup hashes")
	}
	if rotated.Hash("jane@example.com") != hashes[0] {
		t.Fatal("new hashes do not use the active key")
	}
}
