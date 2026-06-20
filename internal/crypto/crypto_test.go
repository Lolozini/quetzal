package crypto

import (
	"bytes"
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
	t.Setenv("K", "0123456789abcdef0123456789abcdef") // 32 raw bytes
	if k := KeyFromEnv("K"); len(k) != 32 {
		t.Errorf("raw 32-byte key not accepted: %d", len(k))
	}
	t.Setenv("K", "")
	if KeyFromEnv("K") != nil {
		t.Error("empty env should yield nil key")
	}
}
