package models

import (
	"errors"
	"strings"
)

// MaxUsernameLen bounds a username. The column holds 190, and a 204-character
// name went into SQLite as it was, where PostgreSQL would have answered 500.
const MaxUsernameLen = 64

// ValidUsername says what is wrong with a new account's name, or nil. Anything
// went but a name under three characters: next to the superadmin Lolozini came
// lolozini and LOLOZINI, names with spaces, HTML, a bell and a newline -- in
// access lists, activity and notifications ("lolozini deleted…"), and in logs a
// line apart. A name is ASCII letters, digits, dots, dashes and underscores,
// starting with a letter or a digit, as Pterodactyl's are; telling two names
// apart by case alone is refused where accounts are made.
func ValidUsername(name string) error {
	if len(name) < 3 || len(name) > MaxUsernameLen {
		return errors.New("a username is 3 to 64 characters")
	}
	for i, r := range name {
		alnum := r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9'
		if i == 0 && !alnum {
			return errors.New("a username starts with a letter or a digit")
		}
		if !alnum && !strings.ContainsRune("._-", r) {
			return errors.New("a username is letters, digits, dots, dashes and underscores")
		}
	}
	return nil
}
