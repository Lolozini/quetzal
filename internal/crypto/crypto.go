// Package crypto provides authenticated encryption (AES-256-GCM) used to keep
// sensitive values out of the database in clear text.
package crypto

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"io"
	"os"
)

// ErrKeySize is returned for a key that is not 32 bytes (AES-256).
var ErrKeySize = errors.New("encryption key must be 32 bytes")

// Seal encrypts plaintext with key (32 bytes) and returns base64(nonce||ciphertext).
func Seal(key, plaintext []byte) (string, error) {
	gcm, err := newGCM(key)
	if err != nil {
		return "", err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return "", err
	}
	ct := gcm.Seal(nonce, nonce, plaintext, nil)
	return base64.StdEncoding.EncodeToString(ct), nil
}

// Open reverses Seal.
func Open(key []byte, encoded string) ([]byte, error) {
	gcm, err := newGCM(key)
	if err != nil {
		return nil, err
	}
	data, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return nil, err
	}
	ns := gcm.NonceSize()
	if len(data) < ns {
		return nil, errors.New("ciphertext too short")
	}
	return gcm.Open(nil, data[:ns], data[ns:], nil)
}

func newGCM(key []byte) (cipher.AEAD, error) {
	if len(key) != 32 {
		return nil, ErrKeySize
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}

// KeyFromEnv loads a 32-byte key from the given env var. The value may be the
// raw 32 bytes or a base64 encoding of 32 bytes. Returns nil if unset/invalid.
func KeyFromEnv(name string) []byte {
	v := os.Getenv(name)
	if v == "" {
		return nil
	}
	if len(v) == 32 {
		return []byte(v)
	}
	if b, err := base64.StdEncoding.DecodeString(v); err == nil && len(b) == 32 {
		return b
	}
	return nil
}
