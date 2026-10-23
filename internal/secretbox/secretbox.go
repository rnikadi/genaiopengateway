// Package secretbox encrypts values stored in the database, such as
// request bodies and user tokens, with AES-256-GCM (ADR 0017).
package secretbox

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
)

// KeySize is the length of an encryption key in bytes.
const KeySize = 32

// Box seals with the current key and opens with the current or a
// previous key, so keys can be rotated without re-encrypting at once.
type Box struct {
	current string
	aeads   map[string]cipher.AEAD
}

// ParseKey decodes a base64 key of KeySize bytes.
func ParseKey(s string) ([]byte, error) {
	k, err := base64.StdEncoding.DecodeString(strings.TrimSpace(s))
	if err != nil {
		k, err = base64.RawURLEncoding.DecodeString(strings.TrimSpace(s))
	}
	if err != nil || len(k) != KeySize {
		return nil, fmt.Errorf("an encryption key must be %d random bytes, base64 encoded (openssl rand -base64 32)", KeySize)
	}
	return k, nil
}

// New returns a box that seals with current and also opens values sealed
// with any of previous.
func New(current []byte, previous ...[]byte) (*Box, error) {
	b := &Box{aeads: map[string]cipher.AEAD{}}
	for i, k := range append([][]byte{current}, previous...) {
		block, err := aes.NewCipher(k)
		if err != nil {
			return nil, err
		}
		aead, err := cipher.NewGCM(block)
		if err != nil {
			return nil, err
		}
		id := keyID(k)
		if i == 0 {
			b.current = id
		}
		b.aeads[id] = aead
	}
	return b, nil
}

// keyID names a key without revealing it: the first 8 hex digits of its
// SHA-256.
func keyID(k []byte) string {
	sum := sha256.Sum256(k)
	return hex.EncodeToString(sum[:4])
}

// Seal encrypts plaintext bound to ad, which must be given again to open
// it. The result is "<key id>.<base64 of nonce and ciphertext>".
func (b *Box) Seal(plaintext, ad []byte) string {
	aead := b.aeads[b.current]
	nonce := make([]byte, aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		panic(err) // crypto/rand never fails on supported platforms
	}
	out := aead.Seal(nonce, nonce, plaintext, ad)
	return b.current + "." + base64.RawStdEncoding.EncodeToString(out)
}

// ErrOpen means a value could not be decrypted: a wrong or missing key,
// tampering, or a value moved to another row.
var ErrOpen = errors.New("secretbox: value cannot be decrypted")

// Open decrypts a value from Seal.
func (b *Box) Open(sealed string, ad []byte) ([]byte, error) {
	id, enc, ok := strings.Cut(sealed, ".")
	aead := b.aeads[id]
	if !ok || aead == nil {
		return nil, ErrOpen
	}
	raw, err := base64.RawStdEncoding.DecodeString(enc)
	if err != nil || len(raw) < aead.NonceSize() {
		return nil, ErrOpen
	}
	out, err := aead.Open(nil, raw[:aead.NonceSize()], raw[aead.NonceSize():], ad)
	if err != nil {
		return nil, ErrOpen
	}
	return out, nil
}

// Current reports whether a value was sealed with the current key.
func (b *Box) Current(sealed string) bool { return strings.HasPrefix(sealed, b.current+".") }
