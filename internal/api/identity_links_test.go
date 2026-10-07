package api

import (
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"github.com/lolozini/quetzal/internal/models"
)

func TestPasswordMutationRevokesOldLinks(t *testing.T) {
	for _, flow := range []string{"self", "admin", "reset"} {
		t.Run(flow, func(t *testing.T) {
			s, u := identityServer(t)
			if err := s.Store.UpdateUserEmail(u.ID, "old@example.com"); err != nil {
				t.Fatal(err)
			}
			u.Email = "old@example.com"
			if err := s.Store.CreatePasswordReset(&models.PasswordReset{UserID: u.ID, TokenHash: hashToken("reset"), ExpiresAt: time.Now().Add(time.Hour)}, u); err != nil {
				t.Fatal(err)
			}
			if err := s.Store.StartEmailConfirmation(&models.EmailConfirmation{UserID: u.ID, Email: "attacker@example.com", TokenHash: "confirm", ExpiresAt: time.Now().Add(time.Hour)}, u.PasswordHash); err != nil {
				t.Fatal(err)
			}
			w := httptest.NewRecorder()
			switch flow {
			case "self":
				s.handleChangePassword(w, identityRequest(u, `{"oldPassword":"alicepw1234","newPassword":"changedpassword"}`))
			case "admin":
				u.IsAdmin = true
				r := identityRequest(u, `{"password":"changedpassword"}`)
				r.SetPathValue("uid", strconv.FormatUint(uint64(u.ID), 10))
				s.handleUpdateUser(w, r)
			case "reset":
				s.handleResetPassword(w, identityRequest(u, `{"token":"reset","password":"changedpassword"}`))
			}
			if w.Code != http.StatusOK && w.Code != http.StatusNoContent {
				t.Fatalf("password mutation: %d %s", w.Code, w.Body.String())
			}
			w = httptest.NewRecorder()
			s.handleResetPassword(w, identityRequest(u, `{"token":"reset","password":"stolenpassword"}`))
			if w.Code != http.StatusBadRequest {
				t.Errorf("old reset still accepted: %d", w.Code)
			}
			if _, err := s.Store.ConfirmEmail("confirm"); err == nil {
				t.Error("old confirmation accepted after password mutation")
			}
			current, err := s.Store.GetUser(u.ID)
			if err != nil {
				t.Fatal(err)
			}
			if current.PendingEmail != "" || current.Email != "old@example.com" {
				t.Errorf("pending email survived: %q / %q", current.Email, current.PendingEmail)
			}
		})
	}
}
