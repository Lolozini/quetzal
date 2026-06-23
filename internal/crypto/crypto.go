// Package crypto provides authenticated encryption (AES-256-GCM) used to keep
// sensitive values out of the database in clear text.
package crypto

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/ed25519"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/pem"
	"errors"
	"io"
	"os"

	"golang.org/x/crypto/ssh"
)

// GenerateSSHHostKey returns a new ed25519 SSH host key in OpenSSH PEM form,
// used as the stable host key for a server's SFTP sidecar.
func GenerateSSHHostKey() ([]byte, error) {
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, err
	}
	block, err := ssh.MarshalPrivateKey(priv, "")
	if err != nil {
		return nil, err
	}
	return pem.EncodeToMemory(block), nil
}

// WakeToken derives a per-server wake-on-connect token from the secret key. The
// controller injects it into a server's activator; the apiserver recomputes and
// compares it (constant-time) to authenticate wake callbacks. A nil/empty key
// still yields a deterministic token (dev only; set QUETZAL_SECRET_KEY in prod).
func WakeToken(key []byte, slug string) string {
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte("wake:" + slug))
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

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
