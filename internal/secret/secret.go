// Package secret holds the bridge's keyed primitives: HMAC derivation of
// lookup keys, AES-GCM sealing of tokens at rest, and random token minting.
package secret

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"errors"
)

// Keys bundles the two server keys.
type Keys struct {
	mac  []byte
	aead cipher.AEAD
}

// New builds Keys from a 32-byte HMAC key and a 32-byte AES-256 key.
func New(macKey, encKey []byte) (*Keys, error) {
	if len(macKey) != 32 || len(encKey) != 32 {
		return nil, errors.New("secret: keys must be 32 bytes")
	}
	block, err := aes.NewCipher(encKey)
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	return &Keys{mac: append([]byte(nil), macKey...), aead: aead}, nil
}

// MAC returns HMAC-SHA256 over the parts, domain-separated by purpose.
// Parts are length-prefixed so ("ab","c") and ("a","bc") differ.
func (k *Keys) MAC(purpose string, parts ...[]byte) []byte {
	h := hmac.New(sha256.New, k.mac)
	writePart(h, []byte(purpose))
	for _, p := range parts {
		writePart(h, p)
	}
	return h.Sum(nil)
}

func writePart(h interface{ Write([]byte) (int, error) }, p []byte) {
	var n [4]byte
	l := len(p)
	n[0], n[1], n[2], n[3] = byte(l>>24), byte(l>>16), byte(l>>8), byte(l)
	h.Write(n[:])
	h.Write(p)
}

// MACString is MAC over string parts.
func (k *Keys) MACString(purpose string, parts ...string) []byte {
	bs := make([][]byte, len(parts))
	for i, p := range parts {
		bs[i] = []byte(p)
	}
	return k.MAC(purpose, bs...)
}

// Seal encrypts plaintext, binding it to aad (e.g. the row key).
func (k *Keys) Seal(plaintext, aad []byte) ([]byte, error) {
	nonce := make([]byte, k.aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, err
	}
	return k.aead.Seal(nonce, nonce, plaintext, aad), nil
}

// Open decrypts a value produced by Seal with the same aad.
func (k *Keys) Open(sealed, aad []byte) ([]byte, error) {
	ns := k.aead.NonceSize()
	if len(sealed) < ns {
		return nil, errors.New("secret: ciphertext too short")
	}
	return k.aead.Open(nil, sealed[:ns], sealed[ns:], aad)
}

// Equal compares secrets in constant time.
func Equal(a, b []byte) bool {
	return subtle.ConstantTimeCompare(a, b) == 1
}

// EqualString compares secret strings in constant time.
func EqualString(a, b string) bool {
	return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}

// Token returns n random bytes encoded as unpadded base64url.
// 32 bytes gives 256 bits of entropy.
func Token(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic("secret: crypto/rand failed: " + err.Error())
	}
	return base64.RawURLEncoding.EncodeToString(b)
}
