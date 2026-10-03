package store

import (
	"errors"
	"testing"

	"github.com/lolozini/quetzal/internal/models"
)

// Accounts made before addresses were one account's may share one. A reset by
// that address picked the oldest of them (R-13); it finds none now, and the
// accounts can still be reset by name.
func TestAnAddressTwoAccountsShareFindsNeither(t *testing.T) {
	s := newTestStore(t)
	for _, name := range []string{"older", "newer"} {
		if err := s.CreateUser(&models.User{Username: name, PasswordHash: "x", Email: "Shared@Example.com"}); err != nil {
			t.Fatal(err)
		}
	}
	if u, err := s.GetUserByEmail("shared@example.com"); !errors.Is(err, ErrNotFound) {
		t.Errorf("found %v (%v), want no account for a shared address", u, err)
	}
	if taken, _ := s.EmailTaken("SHARED@example.com", 0); !taken {
		t.Error("a shared address is not taken")
	}
}
