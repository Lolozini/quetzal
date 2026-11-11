package crypto

import (
	"bytes"
	"encoding/base64"
	"strings"
	"testing"
)

func key32() []byte { return []byte("0123456789abcdef0123456789abcdef") }

func TestSealOpenRoundTrip(t *testing.T) {
	pt := []byte("super secret rcon password")
	enc, err := Seal(key32(), pt)
	if err != nil {
		t.Fatalf("seal: %v", err)
	}
	if strings.Contains(enc, "rcon") {
		t.Fatal("ciphertext leaks plaintext")
	}
	got, err := Open(key32(), enc)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if !bytes.Equal(got, pt) {
		t.Fatalf("round-trip mismatch: %q", got)
	}
}

func TestOpenWrongKeyFails(t *testing.T) {
	enc, _ := Seal(key32(), []byte("x"))
	other := []byte("FFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFF")
	if _, err := Open(other, enc); err == nil {
		t.Fatal("expected auth failure with wrong key")
	}
}

func TestBadKeySize(t *testing.T) {
	if _, err := Seal([]byte("short"), []byte("x")); err != ErrKeySize {
		t.Fatalf("err = %v, want ErrKeySize", err)
	}
}

func TestKeyFromEnv(t *testing.T) {
	raw := "0123456789abcdef0123456789abcdef" // 32 raw bytes
	hexKey := "00112233445566778899aabbccddeeff00112233445566778899aabbccddeeff"
	b64 := base64.StdEncoding.EncodeToString([]byte(raw))
	for name, tc := range map[string]struct {
		val  string
		want int
	}{
		"raw":                 {raw, 32},
		"hex":                 {hexKey, 32},
		"hex with newline":    {hexKey + "\n", 32},
		"base64":              {b64, 32},
		"base64 with newline": {b64 + "\n", 32},
		"unset":               {"", 0},
	} {
		t.Run(name, func(t *testing.T) {
			t.Setenv("K", tc.val)
			k, err := KeyFromEnv("K")
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if len(k) != tc.want {
				t.Fatalf("key length = %d, want %d", len(k), tc.want)
			}
		})
	}
}

// A malformed key must stop the process rather than be taken for "no key",
// which would store every secret in clear text.
func TestKeyFromEnvRejectsMalformed(t *testing.T) {
	for name, v := range map[string]string{
		"too short":     "short",
		"31 bytes":      "0123456789abcdef0123456789abcde",
		"33 bytes":      "0123456789abcdef0123456789abcdef0",
		"63 hex digits": "00112233445566778899aabbccddeeff00112233445566778899aabbccddeef",
		"64 non-hex":    "zz112233445566778899aabbccddeeff00112233445566778899aabbccddeeff",
		"base64 of 16":  base64.StdEncoding.EncodeToString([]byte("0123456789abcdef")),
	} {
		t.Run(name, func(t *testing.T) {
			t.Setenv("K", v)
			k, err := KeyFromEnv("K")
			if err == nil || k != nil {
				t.Fatalf("got key len %d, err %v; want an error", len(k), err)
			}
			if strings.Contains(err.Error(), v) {
				t.Error("the error must not echo the secret value")
			}
		})
	}
}
