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
			for i := range 12 {
				body := `{"code":"invalid"}`
				if route == "password" {
					body = `{"oldPassword":"wrong","newPassword":"password123"}`
				}
				r := httptest.NewRequest(http.MethodPost, "/api/me/"+route, strings.NewReader(body))
				r.Header.Set("Content-Type", "application/json")
				if i%2 == 0 {
					for _, cookie := range cookies {
						r.AddCookie(cookie)
					}
				} else {
					r.Header.Set("Authorization", "Bearer "+token)
				}
				w := httptest.NewRecorder()
				handler.ServeHTTP(w, r)
				if i >= 10 && (w.Code != http.StatusTooManyRequests || w.Header().Get("Retry-After") == "") {
					t.Errorf("attempt %d: %d Retry-After=%q", i+1, w.Code, w.Header().Get("Retry-After"))
				}
			}
		})
	}
}
