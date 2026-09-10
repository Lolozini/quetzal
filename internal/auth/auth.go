// Package auth provides password hashing (argon2id) and opaque session tokens.
// Passwords are never stored or logged in clear text.
package auth

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"

	"golang.org/x/crypto/argon2"
)

// argon2id parameters (OWASP-ish defaults for interactive logins).
const (
	argonTime    = 1
	argonMemory  = 64 * 1024 // 64 MiB
	argonThreads = 4
	argonKeyLen  = 32
	saltLen      = 16
)

// ErrInvalidHash is returned when a stored hash cannot be parsed.
var ErrInvalidHash = errors.New("invalid password hash")

// HashPassword returns an argon2id encoded hash for the given password.
func HashPassword(password string) (string, error) {
	salt := make([]byte, saltLen)
	if _, err := rand.Read(salt); err != nil {
		return "", err
	}
	key := argon2.IDKey([]byte(password), salt, argonTime, argonMemory, argonThreads, argonKeyLen)
	return fmt.Sprintf(
		"$argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s",
		argon2.Version, argonMemory, argonTime, argonThreads,
		base64.RawStdEncoding.EncodeToString(salt),
		base64.RawStdEncoding.EncodeToString(key),
	), nil
}

// decoyHash is a valid argon2id hash of a value nobody can supply, used to spend
// the same work on a username that does not exist as on one that does.
//
// Without it, login answers in a millisecond for an unknown account and in fifty
// for a known one, because only the second reaches argon2. One request then
// tells an attacker whether a username exists -- the per-account throttle is no
// help, since a single probe is all it takes.
var decoyHash = func() string {
	h, err := HashPassword("decoy: no password produces this hash")
	if err != nil {
		// Only a failing RNG gets here, and then nothing else works either.
		panic("auth: cannot build the decoy hash: " + err.Error())
	}
	return h
}()

// SpendVerifyBudget performs the same work VerifyPassword does, and reports
// nothing. Call it on the paths that have no hash to check -- an unknown
// username -- so they take as long as the paths that do.
func SpendVerifyBudget(password string) {
	_, _ = VerifyPassword(decoyHash, password)
}

// VerifyPassword reports whether password matches the encoded argon2id hash.
func VerifyPassword(encoded, password string) (bool, error) {
	parts := strings.Split(encoded, "$")
	if len(parts) != 6 || parts[1] != "argon2id" {
		return false, ErrInvalidHash
	}
	var version int
	if _, err := fmt.Sscanf(parts[2], "v=%d", &version); err != nil {
		return false, ErrInvalidHash
	}
	var mem uint32
	var t, p uint32
	if _, err := fmt.Sscanf(parts[3], "m=%d,t=%d,p=%d", &mem, &t, &p); err != nil {
		return false, ErrInvalidHash
	}
	salt, err := base64.RawStdEncoding.DecodeString(parts[4])
	if err != nil {
		return false, ErrInvalidHash
	}
	want, err := base64.RawStdEncoding.DecodeString(parts[5])
	if err != nil {
		return false, ErrInvalidHash
	}
	got := argon2.IDKey([]byte(password), salt, t, mem, uint8(p), uint32(len(want)))
	return subtle.ConstantTimeCompare(got, want) == 1, nil
}

// NewToken returns a cryptographically random opaque session token.
func NewToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}
