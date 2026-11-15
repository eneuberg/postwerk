// Package seal encrypts stored raw messages at rest (E1–E4; key files: E5 in internal/keys).
//
// A sealed blob is
//
//	0x01 | len(key id) | key id | nonce (12 bytes) | AES-256-GCM ciphertext and tag
//
// encrypted directly with the active store key. The additional authenticated
// data is the blob header (version, key id) followed by the mail id, so a
// blob copied to another mail's row, or given another key id, fails to open.
// Opening uses whichever key the blob names, so old rows stay readable after
// a rotation while new rows use the new key.
//
// With encryption off (local development only), blobs are
//
//	0x00 | raw message
//
// With encryption on, plaintext blobs are refused: otherwise a single
// flipped version byte would turn a ciphertext into "plaintext" and send it
// to a target. Mails stored while encryption was off therefore fail with
// store.decrypt_failed after it is switched on; drain the queue first.
//
// There is no per-mail data key: raw messages live hours, not months, and
// the key id in every blob already allows rotation. See architecture.md.
package seal

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"errors"
	"fmt"

	"github.com/eneuberg/postwerk/internal/keys"
)

// Blob versions.
const (
	versionPlain byte = 0x00
	versionGCM   byte = 0x01
)

const nonceSize = 12

// ErrOpen is wrapped by every error from Open: unknown version, unknown key
// id, or failed authentication. Error texts name the key id at most, never
// message bytes.
var ErrOpen = errors.New("stored message cannot be decrypted")

// Sealer seals and opens raw messages. A Sealer without keys stores
// plaintext (encryption off).
type Sealer struct {
	ring *keys.Ring
	aead map[string]cipher.AEAD
}

// New returns a Sealer that encrypts with ring's active key. A nil ring
// switches encryption off.
func New(ring *keys.Ring) (*Sealer, error) {
	s := &Sealer{ring: ring, aead: make(map[string]cipher.AEAD)}
	if ring == nil {
		return s, nil
	}
	for _, k := range ring.All() {
		block, err := aes.NewCipher(k.Secret())
		if err != nil {
			return nil, fmt.Errorf("seal: %s: %w", k, err)
		}
		gcm, err := cipher.NewGCM(block)
		if err != nil {
			return nil, fmt.Errorf("seal: %s: %w", k, err)
		}
		s.aead[k.ID] = gcm
	}
	return s, nil
}

// Off returns a Sealer that stores plaintext.
func Off() *Sealer {
	s, _ := New(nil)
	return s
}

// Encrypting reports whether new blobs are encrypted.
func (s *Sealer) Encrypting() bool { return s.ring != nil }

// Seal returns the blob to store for mailID.
func (s *Sealer) Seal(mailID string, raw []byte) ([]byte, error) {
	if s.ring == nil {
		out := make([]byte, 0, 1+len(raw))
		out = append(out, versionPlain)
		return append(out, raw...), nil
	}
	k := s.ring.Active()
	aead := s.aead[k.ID]
	header := make([]byte, 0, 2+len(k.ID))
	header = append(header, versionGCM, byte(len(k.ID)))
	header = append(header, k.ID...)

	out := make([]byte, 0, len(header)+nonceSize+len(raw)+aead.Overhead())
	out = append(out, header...)
	nonce := make([]byte, nonceSize)
	if _, err := rand.Read(nonce); err != nil {
		return nil, fmt.Errorf("seal: nonce: %w", err)
	}
	out = append(out, nonce...)
	return aead.Seal(out, nonce, raw, aad(header, mailID)), nil
}

// Open returns the raw message from a blob stored for mailID, or ErrOpen.
func (s *Sealer) Open(mailID string, blob []byte) ([]byte, error) {
	if len(blob) == 0 {
		return nil, fmt.Errorf("%w: empty", ErrOpen)
	}
	switch blob[0] {
	case versionPlain:
		if s.ring != nil {
			return nil, fmt.Errorf("%w: unencrypted, but encryption is on", ErrOpen)
		}
		return blob[1:], nil
	case versionGCM:
	default:
		return nil, fmt.Errorf("%w: unknown format", ErrOpen)
	}
	if len(blob) < 2 {
		return nil, fmt.Errorf("%w: truncated", ErrOpen)
	}
	idLen := int(blob[1])
	if idLen == 0 || len(blob) < 2+idLen+nonceSize {
		return nil, fmt.Errorf("%w: truncated", ErrOpen)
	}
	header := blob[:2+idLen]
	id := string(header[2:])
	aead, ok := s.aead[id]
	if !ok {
		if !keys.ValidID(id) {
			id = "<invalid>"
		}
		return nil, fmt.Errorf("%w: key id %s is not in the store key file", ErrOpen, id)
	}
	nonce := blob[len(header) : len(header)+nonceSize]
	raw, err := aead.Open(nil, nonce, blob[len(header)+nonceSize:], aad(header, mailID))
	if err != nil {
		return nil, fmt.Errorf("%w: authentication failed (tampered, or stored for another mail)", ErrOpen)
	}
	return raw, nil
}

func aad(header []byte, mailID string) []byte {
	a := make([]byte, 0, len(header)+len(mailID))
	a = append(a, header...)
	return append(a, mailID...)
}
