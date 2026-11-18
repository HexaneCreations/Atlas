// Package secretbox encrypts small secrets at rest with AES-256-GCM.
//
// Every ciphertext is bound to associated data the caller chooses (for a
// notification channel, its id), so a ciphertext copied onto another row
// does not decrypt. Every ciphertext also records which key produced it, so
// decrypting with a different key fails as [ErrKeyMismatch] rather than as a
// generic authentication failure.
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

// KeySize is the required key length in bytes (AES-256).
const KeySize = 32

const nonceSize = 12

var (
	// ErrKeyMismatch means the ciphertext was sealed under a different key
	// than the one supplied. It is never retried with another key.
	ErrKeyMismatch = errors.New("encryption key does not match")
	// ErrDecrypt means the ciphertext or its associated data has been
	// altered, or the ciphertext is malformed.
	ErrDecrypt = errors.New("could not decrypt: ciphertext or associated data is invalid")
	// ErrKeyEncoding means an encoded key is not valid standard base64.
	ErrKeyEncoding = errors.New("must be standard base64")
)

// Sealed is an encrypted value as stored: the id of the key that sealed it
// and the nonce followed by the GCM ciphertext and tag.
type Sealed struct {
	KeyID      string
	Ciphertext []byte
}

// Key is a loaded AES-256-GCM key. It keeps no copy of the raw key bytes, and
// every formatting path prints a placeholder, so a Key cannot leak through a
// log line, an error string or a panic value.
type Key struct {
	aead cipher.AEAD
	id   string
}

// ParseKey decodes a base64-encoded 32-byte key. Surrounding whitespace,
// including a trailing newline from a mounted secret file, is ignored. The
// returned error never contains any part of encoded.
func ParseKey(encoded string) (*Key, error) {
	raw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(encoded))
	if err != nil {
		return nil, ErrKeyEncoding
	}
	return NewKey(raw)
}

// NewKey builds a Key from raw key bytes.
func NewKey(raw []byte) (*Key, error) {
	if len(raw) != KeySize {
		return nil, fmt.Errorf("must decode to %d bytes, got %d", KeySize, len(raw))
	}
	block, err := aes.NewCipher(raw)
	if err != nil {
		return nil, errors.New("could not initialise AES")
	}
	aead, err := cipher.NewGCMWithNonceSize(block, nonceSize)
	if err != nil {
		return nil, errors.New("could not initialise GCM")
	}
	sum := sha256.Sum256(raw)
	return &Key{aead: aead, id: hex.EncodeToString(sum[:8])}, nil
}

// ID is the key's identifier: the first 8 bytes of sha256(key), hex-encoded.
// Safe to store and log; it does not reveal the key.
func (k *Key) ID() string { return k.id }

// Seal encrypts plaintext bound to aad under a fresh random nonce.
func (k *Key) Seal(plaintext, aad []byte) (Sealed, error) {
	nonce := make([]byte, nonceSize, nonceSize+len(plaintext)+k.aead.Overhead())
	if _, err := rand.Read(nonce); err != nil {
		return Sealed{}, errors.New("could not generate a nonce")
	}
	return Sealed{KeyID: k.id, Ciphertext: k.aead.Seal(nonce, nonce, plaintext, aad)}, nil
}

// Open decrypts s, which must have been sealed by this key with the same aad.
func (k *Key) Open(s Sealed, aad []byte) ([]byte, error) {
	if s.KeyID != k.id {
		return nil, ErrKeyMismatch
	}
	if len(s.Ciphertext) < nonceSize+k.aead.Overhead() {
		return nil, ErrDecrypt
	}
	nonce, body := s.Ciphertext[:nonceSize], s.Ciphertext[nonceSize:]
	plaintext, err := k.aead.Open(nil, nonce, body, aad)
	if err != nil {
		return nil, ErrDecrypt
	}
	if plaintext == nil {
		plaintext = []byte{}
	}
	return plaintext, nil
}

const redacted = "secretbox.Key([REDACTED])"

// String implements fmt.Stringer with a placeholder.
func (k *Key) String() string { return redacted }

// GoString implements fmt.GoStringer with a placeholder, covering %#v.
func (k *Key) GoString() string { return redacted }
