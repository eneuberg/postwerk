package seal

import (
	"bytes"
	"errors"
	"strings"
	"testing"

	"github.com/eneuberg/postwerk/internal/keys"
)

const (
	lineNew = "new:AAECAwQFBgcICQoLDA0ODxAREhMUFRYXGBkaGxwdHh8="
	lineOld = "old:ICEiIyQlJicoKSorLC0uLzAxMjM0NTY3ODk6Ozw9Pj8="
)

var msg = []byte("Subject: secret\r\n\r\nThe code is 123456\r\n")

func sealer(t *testing.T, lines ...string) *Sealer {
	t.Helper()
	r, err := keys.Parse([]byte(strings.Join(lines, "\n")))
	if err != nil {
		t.Fatal(err)
	}
	s, err := New(r)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestRoundTrip(t *testing.T) {
	s := sealer(t, lineNew)
	blob, err := s.Seal("pw_1", msg)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(blob, []byte("secret")) || bytes.Contains(blob, []byte("123456")) {
		t.Fatal("plaintext in blob")
	}
	if blob[0] != versionGCM || blob[1] != 3 || string(blob[2:5]) != "new" {
		t.Fatalf("header = %x", blob[:5])
	}
	if len(blob) != 2+3+nonceSize+len(msg)+16 {
		t.Fatalf("blob length %d", len(blob))
	}
	got, err := s.Open("pw_1", blob)
	if err != nil || !bytes.Equal(got, msg) {
		t.Fatalf("open = %q, %v", got, err)
	}
	// Two seals of the same message differ (random nonce).
	again, _ := s.Seal("pw_1", msg)
	if bytes.Equal(blob, again) {
		t.Fatal("nonce reused")
	}
	if !s.Encrypting() {
		t.Fatal("Encrypting = false")
	}
}

func TestSwappedRowsFail(t *testing.T) {
	s := sealer(t, lineNew)
	blob, _ := s.Seal("pw_1", msg)
	if _, err := s.Open("pw_2", blob); !errors.Is(err, ErrOpen) || !strings.Contains(err.Error(), "authentication failed") {
		t.Fatalf("open with another mail id: %v", err)
	}
}

func TestTamperedFails(t *testing.T) {
	s := sealer(t, lineNew, lineOld)
	blob, _ := s.Seal("pw_1", msg)
	for i := range blob {
		b := bytes.Clone(blob)
		b[i] ^= 0x01
		if got, err := s.Open("pw_1", b); err == nil {
			t.Fatalf("byte %d flipped, opened %q", i, got)
		}
	}
	for _, b := range [][]byte{nil, {}, {versionGCM}, blob[:10], {0x02, 1, 2, 3}} {
		if _, err := s.Open("pw_1", b); !errors.Is(err, ErrOpen) {
			t.Fatalf("open %x: %v", b, err)
		}
	}
	// Changing the key id in the header to the other known key fails too.
	b := bytes.Clone(blob)
	copy(b[2:5], "old")
	if _, err := s.Open("pw_1", b); !errors.Is(err, ErrOpen) {
		t.Fatalf("relabelled key id: %v", err)
	}
}

func TestUnknownKeyID(t *testing.T) {
	blob, _ := sealer(t, lineOld).Seal("pw_1", msg)
	_, err := sealer(t, lineNew).Open("pw_1", blob)
	if !errors.Is(err, ErrOpen) || !strings.Contains(err.Error(), "key id old is not in the store key file") {
		t.Fatalf("err = %v", err)
	}
	if strings.Contains(err.Error(), "secret") {
		t.Fatal("error leaks content")
	}
	b := bytes.Clone(blob)
	b[2] = '\n'
	if _, err := sealer(t, lineNew).Open("pw_1", b); err == nil || !strings.Contains(err.Error(), "<invalid>") {
		t.Fatalf("invalid id: %v", err)
	}
}

func TestRotation(t *testing.T) {
	before := sealer(t, lineOld)
	oldBlob, _ := before.Seal("pw_1", msg)

	// The new key is put first; the old one stays for reading.
	after := sealer(t, lineNew, lineOld)
	newBlob, _ := after.Seal("pw_2", msg)
	if string(newBlob[2:5]) != "new" {
		t.Fatalf("new rows use key %q", newBlob[2:5])
	}
	for id, b := range map[string][]byte{"pw_1": oldBlob, "pw_2": newBlob} {
		if got, err := after.Open(id, b); err != nil || !bytes.Equal(got, msg) {
			t.Fatalf("%s: %q %v", id, got, err)
		}
	}
	// Once the old key is removed, old rows cannot be read any more.
	if _, err := sealer(t, lineNew).Open("pw_1", oldBlob); !errors.Is(err, ErrOpen) {
		t.Fatalf("old row opened without its key: %v", err)
	}
}

func TestEncryptionOff(t *testing.T) {
	off := Off()
	if off.Encrypting() {
		t.Fatal("Encrypting = true")
	}
	blob, err := off.Seal("pw_1", msg)
	if err != nil || blob[0] != versionPlain || !bytes.Equal(blob[1:], msg) {
		t.Fatalf("plain blob = %q, %v", blob, err)
	}
	if got, err := off.Open("pw_1", blob); err != nil || !bytes.Equal(got, msg) {
		t.Fatalf("open = %q %v", got, err)
	}
	// Plain rows are refused with encryption on: a flipped version byte
	// must never turn a ciphertext into a message that is sent.
	if _, err := sealer(t, lineNew).Open("pw_1", blob); !errors.Is(err, ErrOpen) {
		t.Fatalf("plain row with encryption on: %v", err)
	}
	// Encrypted rows cannot be read with encryption off.
	enc, _ := sealer(t, lineNew).Seal("pw_1", msg)
	if _, err := off.Open("pw_1", enc); !errors.Is(err, ErrOpen) {
		t.Fatalf("encrypted row with encryption off: %v", err)
	}
}
