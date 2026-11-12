package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/lolozini/quetzal/internal/models"
	"github.com/lolozini/quetzal/internal/totp"
)

func TestSensitiveBudgetSharedBySessionAndAPIKey(t *testing.T) {
	for _, route := range []string{"password", "2fa/disable"} {
		t.Run(route, func(t *testing.T) {
			s, u := identityServer(t)
			w := httptest.NewRecorder()
			if err := s.startSession(w, u); err != nil {
				t.Fatal(err)
			}
			cookies := w.Result().Cookies()
			token := apiKeyPrefix + "budget-test"
			if err := s.Store.CreateAPIKey(&models.APIKey{UserID: u.ID, Name: "test", Hash: hashToken(token)}); err != nil {
				t.Fatal(err)
			}
			if err := s.Store.EnableUserTOTP(u.ID, []string{totp.HashRecovery("recovery")}); err != nil {
				t.Fatal(err)
			}
			handler := s.Handler()
			attempt := func(withKey bool) *httptest.ResponseRecorder {
				body := `{"code":"invalid"}`
				if route == "password" {
					body = `{"oldPassword":"wrong","newPassword":"password123"}`
				}
				r := httptest.NewRequest(http.MethodPost, "/api/me/"+route, strings.NewReader(body))
				r.Header.Set("Content-Type", "application/json")
				if withKey {
					r.Header.Set("Authorization", "Bearer "+token)
				} else {
					for _, cookie := range cookies {
						r.AddCookie(cookie)
					}
				}
				w := httptest.NewRecorder()
				handler.ServeHTTP(w, r)
				return w
			}
			// An API key never reaches these routes: it cannot guess a password or a
			// code there, so it cannot spend the budget either.
			if w := attempt(true); w.Code != http.StatusForbidden {
				t.Errorf("an API key on %s = %d, want 403", route, w.Code)
			}
			// The signed-in session has the budget to itself.
			for i := range 12 {
				w := attempt(false)
				if i >= 10 && (w.Code != http.StatusTooManyRequests || w.Header().Get("Retry-After") == "") {
					t.Errorf("attempt %d: %d Retry-After=%q", i+1, w.Code, w.Header().Get("Retry-After"))
				}
			}
		})
	}
}
