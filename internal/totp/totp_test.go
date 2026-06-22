package totp

import (
	"testing"
	"time"
)

func TestValidateAcceptsCurrentAndSkew(t *testing.T) {
	secret, err := GenerateSecret()
	if err != nil {
		t.Fatalf("gen: %v", err)
	}
	now := time.Unix(1_700_000_000, 0)
	key, _ := decode(secret)
	counter := uint64(now.Unix()) / period
	for _, off := range []uint64{counter - 1, counter, counter + 1} {
		code := hotp(key, off)
		if !validateAt(secret, code, now) {
			t.Errorf("code for counter %d should validate within skew", off)
		}
	}
	// Two steps away must be rejected.
	if validateAt(secret, hotp(key, counter+2), now) {
		t.Error("code two steps ahead should be rejected")
	}
}

func TestValidateRejectsGarbage(t *testing.T) {
	secret, _ := GenerateSecret()
	for _, bad := range []string{"", "12345", "1234567", "abcdef", "  "} {
		if Validate(secret, bad) {
			t.Errorf("garbage code %q must not validate", bad)
		}
	}
}

func TestHOTPRFC4226Vector(t *testing.T) {
	// RFC 4226 Appendix D test vectors for the ASCII secret "12345678901234567890".
	key := []byte("12345678901234567890")
	want := []string{"755224", "287082", "359152", "969429", "338314"}
	for i, w := range want {
		if got := hotp(key, uint64(i)); got != w {
			t.Errorf("hotp(%d) = %s, want %s", i, got, w)
		}
	}
}

func TestRecoveryCodesHashStableAndNormalized(t *testing.T) {
	plain, hashes, err := NewRecoveryCodes(10)
	if err != nil {
		t.Fatalf("gen: %v", err)
	}
	if len(plain) != 10 || len(hashes) != 10 {
		t.Fatalf("want 10 codes, got %d/%d", len(plain), len(hashes))
	}
	// A displayed code (with dash) hashes to the same value when re-entered with
	// spaces, different case, or no dash.
	h := HashRecovery(plain[0])
	if h != hashes[0] {
		t.Errorf("displayed code does not match its stored hash")
	}
	if HashRecovery(" "+plain[0]+" ") != hashes[0] {
		t.Error("normalization should ignore surrounding whitespace")
	}
	// Codes are unique.
	seen := map[string]bool{}
	for _, code := range plain {
		if seen[code] {
			t.Error("duplicate recovery code")
		}
		seen[code] = true
	}
}

func TestURIContainsSecretAndIssuer(t *testing.T) {
	u := URI("ABC234", "Quetzal", "alice")
	for _, want := range []string{"otpauth://totp/", "secret=ABC234", "issuer=Quetzal", "digits=6", "period=30"} {
		if !contains(u, want) {
			t.Errorf("URI missing %q: %s", want, u)
		}
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
