package secretbox_test

import (
	"bytes"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/hexane/atlas/internal/platform/secretbox"
)

func newKey(t *testing.T) (*secretbox.Key, string) {
	t.Helper()
	raw := make([]byte, secretbox.KeySize)
	if _, err := rand.Read(raw); err != nil {
		t.Fatal(err)
	}
	encoded := base64.StdEncoding.EncodeToString(raw)
	k, err := secretbox.ParseKey(encoded)
	if err != nil {
		t.Fatalf("ParseKey: %v", err)
	}
	return k, encoded
}

func TestSealOpenRoundTrips(t *testing.T) {
	t.Parallel()
	k, _ := newKey(t)
	plaintext := []byte("smtp-app-password")

	s, err := k.Seal(plaintext, []byte("chan-1"))
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}
	if s.KeyID != k.ID() {
		t.Errorf("KeyID = %q, want %q", s.KeyID, k.ID())
	}
	if bytes.Contains(s.Ciphertext, plaintext) {
		t.Fatal("ciphertext contains the plaintext")
	}
	got, err := k.Open(s, []byte("chan-1"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if !bytes.Equal(got, plaintext) {
		t.Errorf("Open = %q, want %q", got, plaintext)
	}
}

func TestEmptyPlaintextRoundTrips(t *testing.T) {
	t.Parallel()
	k, _ := newKey(t)
	s, err := k.Seal(nil, []byte("chan-1"))
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}
	got, err := k.Open(s, []byte("chan-1"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if got == nil || len(got) != 0 {
		t.Errorf("Open = %#v, want an empty non-nil slice", got)
	}
}

func TestTamperedCiphertextFails(t *testing.T) {
	t.Parallel()
	k, _ := newKey(t)
	s, err := k.Seal([]byte("secret"), []byte("chan-1"))
	if err != nil {
		t.Fatal(err)
	}
	for i := range s.Ciphertext {
		tampered := secretbox.Sealed{KeyID: s.KeyID, Ciphertext: bytes.Clone(s.Ciphertext)}
		tampered.Ciphertext[i] ^= 0x01
		if _, err := k.Open(tampered, []byte("chan-1")); !errors.Is(err, secretbox.ErrDecrypt) {
			t.Fatalf("flipping byte %d: err = %v, want ErrDecrypt", i, err)
		}
	}
	short := secretbox.Sealed{KeyID: s.KeyID, Ciphertext: s.Ciphertext[:10]}
	if _, err := k.Open(short, []byte("chan-1")); !errors.Is(err, secretbox.ErrDecrypt) {
		t.Errorf("truncated ciphertext: err = %v, want ErrDecrypt", err)
	}
}

// The channel id is the associated data: a ciphertext moved to another
// channel's row does not decrypt.
func TestWrongAssociatedDataFails(t *testing.T) {
	t.Parallel()
	k, _ := newKey(t)
	s, err := k.Seal([]byte("secret"), []byte("chan-1"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := k.Open(s, []byte("chan-2")); !errors.Is(err, secretbox.ErrDecrypt) {
		t.Errorf("err = %v, want ErrDecrypt", err)
	}
}

// A different key that is presented under the sealing key's id still fails
// authentication: the key id is a label, not the check.
func TestWrongKeyFails(t *testing.T) {
	t.Parallel()
	k1, _ := newKey(t)
	k2, _ := newKey(t)
	s, err := k1.Seal([]byte("secret"), []byte("chan-1"))
	if err != nil {
		t.Fatal(err)
	}
	forged := secretbox.Sealed{KeyID: k2.ID(), Ciphertext: s.Ciphertext}
	if _, err := k2.Open(forged, []byte("chan-1")); !errors.Is(err, secretbox.ErrDecrypt) {
		t.Errorf("err = %v, want ErrDecrypt", err)
	}
}

func TestKeyIDMismatchIsItsOwnError(t *testing.T) {
	t.Parallel()
	k1, _ := newKey(t)
	k2, _ := newKey(t)
	s, err := k1.Seal([]byte("secret"), []byte("chan-1"))
	if err != nil {
		t.Fatal(err)
	}
	_, err = k2.Open(s, []byte("chan-1"))
	if !errors.Is(err, secretbox.ErrKeyMismatch) {
		t.Fatalf("err = %v, want ErrKeyMismatch", err)
	}
	if err.Error() != "encryption key does not match" {
		t.Errorf("message = %q", err.Error())
	}
	if errors.Is(err, secretbox.ErrDecrypt) {
		t.Error("a key mismatch must not also report as ErrDecrypt")
	}
}

func TestKeyIDIsFirst8BytesOfSHA256Hex(t *testing.T) {
	t.Parallel()
	k, err := secretbox.NewKey(make([]byte, secretbox.KeySize))
	if err != nil {
		t.Fatal(err)
	}
	// sha256 of 32 zero bytes begins 66687aadf862bd77.
	if k.ID() != "66687aadf862bd77" {
		t.Errorf("ID = %q, want 66687aadf862bd77", k.ID())
	}
}

func TestNonceDiffersOnEveryCall(t *testing.T) {
	t.Parallel()
	k, _ := newKey(t)
	seen := map[string]bool{}
	for range 1000 {
		s, err := k.Seal([]byte("same"), []byte("chan-1"))
		if err != nil {
			t.Fatal(err)
		}
		nonce := string(s.Ciphertext[:12])
		if seen[nonce] {
			t.Fatal("a nonce was reused")
		}
		seen[nonce] = true
	}
}

func TestParseKeyValidation(t *testing.T) {
	t.Parallel()
	valid := base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{0xAB}, 32))
	for _, tc := range []struct {
		name, input string
		ok          bool
	}{
		{"valid", valid, true},
		{"valid with trailing newline", valid + "\n", true},
		{"valid with surrounding whitespace", "  " + valid + "\r\n", true},
		{"empty", "", false},
		{"16 bytes", base64.StdEncoding.EncodeToString(make([]byte, 16)), false},
		{"31 bytes", base64.StdEncoding.EncodeToString(make([]byte, 31)), false},
		{"33 bytes", base64.StdEncoding.EncodeToString(make([]byte, 33)), false},
		{"not base64", "this-is-not-base64!!this-is-not-base64!!xx", false},
		{"raw 32 ascii chars", strings.Repeat("k", 32), false},
		{"url alphabet", strings.NewReplacer("+", "-", "/", "_").Replace(base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{0xFB}, 32))), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := secretbox.ParseKey(tc.input)
			if tc.ok && err != nil {
				t.Fatalf("ParseKey: %v", err)
			}
			if !tc.ok {
				if err == nil {
					t.Fatal("ParseKey accepted an invalid key")
				}
				if tc.input != "" && strings.Contains(err.Error(), strings.TrimSpace(tc.input)) {
					t.Errorf("error %q contains the key", err)
				}
			}
		})
	}
}

// No formatting verb prints key material.
func TestKeyNeverFormatsItsMaterial(t *testing.T) {
	t.Parallel()
	raw := bytes.Repeat([]byte{0x5A}, 32) // "ZZZZ..."
	k, err := secretbox.NewKey(raw)
	if err != nil {
		t.Fatal(err)
	}
	encoded := base64.StdEncoding.EncodeToString(raw)
	for _, verb := range []string{"%v", "%+v", "%#v", "%s", "%x", "%q"} {
		out := fmt.Sprintf(verb, k)
		if strings.Contains(out, encoded) || strings.Contains(out, string(raw)) ||
			strings.Contains(out, fmt.Sprintf("%x", raw)) || strings.Contains(strings.ToLower(out), "5a5a5a5a") {
			t.Errorf("%s printed key material: %s", verb, out)
		}
	}
}
