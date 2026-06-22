// Package totp implements time-based one-time passwords (RFC 6238, HMAC-SHA1,
// 6 digits, 30s step) plus single-use recovery codes, for opt-in two-factor
// authentication. The TOTP secret is reversible (needed to compute codes) and
// must be stored encrypted by the caller; recovery codes are stored only as
// SHA-256 hashes and consumed on use.
package totp

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha1"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base32"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"net/url"
	"strings"
	"time"
)

const (
	period      = 30
	digits      = 6
	secretBytes = 20 // 160-bit secret (RFC 4226 recommended)
)

var b32 = base32.StdEncoding.WithPadding(base32.NoPadding)

// GenerateSecret returns a new random base32 (unpadded) TOTP secret.
func GenerateSecret() (string, error) {
	b := make([]byte, secretBytes)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return b32.EncodeToString(b), nil
}

// Code returns the TOTP code for a base32 secret at time t.
func Code(secret string, t time.Time) (string, error) {
	key, err := decode(secret)
	if err != nil {
		return "", err
	}
	return hotp(key, uint64(t.Unix())/period), nil
}

// Validate reports whether code matches secret at the current time, tolerating
// ±1 step of clock skew.
func Validate(secret, code string) bool {
	return validateAt(secret, code, time.Now())
}

func validateAt(secret, code string, now time.Time) bool {
	code = strings.TrimSpace(code)
	if len(code) != digits {
		return false
	}
	key, err := decode(secret)
	if err != nil {
		return false
	}
	counter := uint64(now.Unix()) / period
	for _, c := range []uint64{counter - 1, counter, counter + 1} {
		if subtle.ConstantTimeCompare([]byte(hotp(key, c)), []byte(code)) == 1 {
			return true
		}
	}
	return false
}

func hotp(key []byte, counter uint64) string {
	var buf [8]byte
	binary.BigEndian.PutUint64(buf[:], counter)
	mac := hmac.New(sha1.New, key)
	mac.Write(buf[:])
	sum := mac.Sum(nil)
	offset := sum[len(sum)-1] & 0x0f
	v := (uint32(sum[offset])&0x7f)<<24 |
		uint32(sum[offset+1])<<16 |
		uint32(sum[offset+2])<<8 |
		uint32(sum[offset+3])
	return fmt.Sprintf("%0*d", digits, v%pow10(digits))
}

func decode(secret string) ([]byte, error) {
	return b32.DecodeString(strings.ToUpper(strings.TrimSpace(secret)))
}

func pow10(n int) uint32 {
	p := uint32(1)
	for i := 0; i < n; i++ {
		p *= 10
	}
	return p
}

// URI builds an otpauth:// provisioning URI (for QR codes / manual entry).
func URI(secret, issuer, account string) string {
	q := url.Values{}
	q.Set("secret", secret)
	q.Set("issuer", issuer)
	q.Set("algorithm", "SHA1")
	q.Set("digits", fmt.Sprint(digits))
	q.Set("period", fmt.Sprint(period))
	label := url.PathEscape(issuer + ":" + account)
	return "otpauth://totp/" + label + "?" + q.Encode()
}

// NewRecoveryCodes returns n display codes (shown once) and their stored hashes.
func NewRecoveryCodes(n int) (plain, hashes []string, err error) {
	for i := 0; i < n; i++ {
		b := make([]byte, 5) // 10 hex chars, ~40 bits
		if _, err = rand.Read(b); err != nil {
			return nil, nil, err
		}
		raw := hex.EncodeToString(b)
		plain = append(plain, raw[:5]+"-"+raw[5:]) // grouped for readability
		hashes = append(hashes, HashRecovery(raw))
	}
	return plain, hashes, nil
}

// HashRecovery normalizes (strip non-alphanumerics, lowercase) and SHA-256
// hashes a recovery code. Codes are high-entropy, so a fast hash is sufficient.
func HashRecovery(code string) string {
	var b strings.Builder
	for _, r := range code {
		switch {
		case r >= '0' && r <= '9', r >= 'a' && r <= 'z':
			b.WriteRune(r)
		case r >= 'A' && r <= 'Z':
			b.WriteRune(r + 32)
		}
	}
	sum := sha256.Sum256([]byte(b.String()))
	return hex.EncodeToString(sum[:])
}
