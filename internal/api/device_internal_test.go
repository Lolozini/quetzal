package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/lolozini/quetzal/internal/models"
)

// A device cookie lets its browser past the lock on an account, so one that
// could be made up, or carried over to another account, would give a guesser
// a fresh count per cookie.
func TestDeviceCookieHoldsOnlyWhereItWasEarned(t *testing.T) {
	s := &Server{processKey: newProcessKey()}
	admin := &models.User{ID: 1, PasswordHash: "hash-of-the-admin-password"}
	mallory := &models.User{ID: 2, PasswordHash: "hash-of-mallory-password"}

	earn := func(s *Server, u *models.User) *http.Cookie {
		rec := httptest.NewRecorder()
		s.rememberDevice(rec, u)
		cs := rec.Result().Cookies()
		if len(cs) != 1 {
			t.Fatalf("cookies set = %d, want 1", len(cs))
		}
		return cs[0]
	}
	known := func(s *Server, c *http.Cookie, u *models.User) bool {
		r := httptest.NewRequest(http.MethodPost, "/api/login", nil)
		r.AddCookie(c)
		return s.knownDevice(r, u) != ""
	}

	c := earn(s, admin)
	if !c.HttpOnly || c.Path != "/api/login" || c.SameSite != http.SameSiteStrictMode {
		t.Errorf("cookie = %+v, want HttpOnly, SameSite=Strict, on /api/login", c)
	}
	if !known(s, c, admin) {
		t.Fatal("a browser's own cookie is not honoured")
	}

	// Mallory's cookie, renamed for the admin's account.
	m := earn(s, mallory)
	m.Name = c.Name
	if known(s, m, admin) {
		t.Error("another account's cookie is honoured")
	}
	// An id of one's choosing under a genuine signature.
	id, sig, _ := strings.Cut(c.Value, ".")
	forged := *c
	forged.Value = strings.Repeat("0", len(id)) + "." + sig
	if known(s, &forged, admin) {
		t.Error("a cookie with a made-up id is honoured")
	}
	forged.Value = id + "." + strings.Repeat("A", len(sig))
	if known(s, &forged, admin) {
		t.Error("a cookie with a made-up signature is honoured")
	}
	// Once the password has changed.
	changed := *admin
	changed.PasswordHash = "hash-of-the-new-password"
	if known(s, c, &changed) {
		t.Error("a cookie earned with the old password is honoured")
	}
	// Signed by another install, or by this one before a restart without a
	// secret key.
	if known(&Server{processKey: newProcessKey()}, c, admin) {
		t.Error("a cookie signed with another key is honoured")
	}
	// With a secret key, it holds across processes.
	key := []byte("0123456789abcdef0123456789abcdef")
	a, b := &Server{WakeKey: key, processKey: newProcessKey()}, &Server{WakeKey: key, processKey: newProcessKey()}
	if !known(b, earn(a, admin), admin) {
		t.Error("with a secret key, another replica does not honour the cookie")
	}
}
