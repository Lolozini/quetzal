package api

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"net/http"
	"net/netip"
	"strings"
	"time"

	"github.com/lolozini/quetzal/internal/auth"
	"github.com/lolozini/quetzal/internal/models"
)

// Failed sign-ins used to count against the account alone, wherever they came
// from: ten wrong passwords for "admin" from anywhere kept the administrator
// out for fifteen minutes, and forty requests an hour kept them out for good.
//
// A browser that signs in to an account now keeps a device cookie for it, a
// signed statement that it knew the account's password. Attempts from such a
// browser count against that browser alone; attempts from any other count
// against the account, as before. Hammering an account then locks out the
// browsers that never signed in to it, not its owner's, while guessing from
// many addresses still gets ten tries per window between all of them. This is
// OWASP's "Slow Down Online Guessing Attacks with Device Cookies".

const (
	deviceCookiePrefix = "quetzal_device_"
	// A browser stays known for a year after it last signed in.
	deviceCookieTTL = 365 * 24 * time.Hour
)

// deviceKey signs device cookies. It derives from the secret key when there is
// one, so they hold across restarts and replicas; without one it is this
// process's own, and a restart forgets which browsers were known.
func (s *Server) deviceKey() []byte {
	if len(s.WakeKey) == 0 {
		return s.processKey
	}
	mac := hmac.New(sha256.New, s.WakeKey)
	mac.Write([]byte("quetzal device cookie"))
	return mac.Sum(nil)
}

func newProcessKey() []byte {
	k := make([]byte, 32)
	_, _ = rand.Read(k) // cannot fail: crypto/rand crashes the program instead
	return k
}

// deviceMAC ties a device cookie to its account and to the account's password:
// a new password, which is what someone sets when they fear the old one leaked,
// retires every browser that knew the old one.
func deviceMAC(key []byte, u *models.User, id string) []byte {
	mac := hmac.New(sha256.New, key)
	fmt.Fprintf(mac, "%d\x00%s\x00%s", u.ID, id, u.PasswordHash)
	return mac.Sum(nil)
}

func deviceCookieName(u *models.User) string {
	return fmt.Sprintf("%s%d", deviceCookiePrefix, u.ID)
}

// knownDevice returns the id of the request's device cookie for u, or "" when
// it carries no valid one.
func (s *Server) knownDevice(r *http.Request, u *models.User) string {
	c, err := r.Cookie(deviceCookieName(u))
	if err != nil {
		return ""
	}
	id, sig, ok := strings.Cut(c.Value, ".")
	if !ok || id == "" {
		return ""
	}
	got, err := base64.RawURLEncoding.DecodeString(sig)
	if err != nil || !hmac.Equal(got, deviceMAC(s.deviceKey(), u, id)) {
		return ""
	}
	return id
}

// rememberDevice gives the browser a device cookie for u, under a fresh id.
func (s *Server) rememberDevice(w http.ResponseWriter, u *models.User) {
	id, err := auth.NewToken()
	if err != nil {
		return // the browser stays unknown, as it was
	}
	http.SetCookie(w, &http.Cookie{
		Name:     deviceCookieName(u),
		Value:    id + "." + base64.RawURLEncoding.EncodeToString(deviceMAC(s.deviceKey(), u, id)),
		Path:     "/api/login", // the only place that reads it
		MaxAge:   int(deviceCookieTTL / time.Second),
		HttpOnly: true,
		Secure:   s.Secure,
		SameSite: http.SameSiteStrictMode,
	})
}

// authAddress is the key r's address is counted under by AuthIPLimiter: the
// address itself, or for IPv6 its /64, all of which one subscriber commonly
// holds and could otherwise draw a fresh budget from, address after address.
func (s *Server) authAddress(r *http.Request) string {
	ip := s.clientIP(r)
	a, err := netip.ParseAddr(ip)
	if err != nil {
		return ip
	}
	if a = a.Unmap(); a.Is4() {
		return a.String()
	}
	p, err := a.WithZone("").Prefix(64)
	if err != nil {
		return ip
	}
	return p.String()
}
