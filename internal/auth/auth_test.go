package auth

import "testing"

func TestHashAndVerify(t *testing.T) {
	hash, err := HashPassword("correct horse battery staple")
	if err != nil {
		t.Fatalf("hash: %v", err)
	}
	if hash == "correct horse battery staple" {
		t.Fatal("password stored in clear text")
	}

	ok, err := VerifyPassword(hash, "correct horse battery staple")
	if err != nil || !ok {
		t.Errorf("verify correct = %v / %v", ok, err)
	}
	ok, err = VerifyPassword(hash, "wrong")
	if err != nil || ok {
		t.Errorf("verify wrong = %v / %v, want false", ok, err)
	}
}

func TestHashesAreSalted(t *testing.T) {
	a, _ := HashPassword("same")
	b, _ := HashPassword("same")
	if a == b {
		t.Error("identical passwords produced identical hashes (missing salt)")
	}
}

func TestNewTokenUnique(t *testing.T) {
	a, _ := NewToken()
	b, _ := NewToken()
	if a == "" || a == b {
		t.Errorf("tokens not unique/non-empty: %q %q", a, b)
	}
}

func TestVerifyInvalidHash(t *testing.T) {
	if _, err := VerifyPassword("not-a-hash", "x"); err == nil {
		t.Error("expected error for malformed hash")
	}
}
