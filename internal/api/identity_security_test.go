package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"gorm.io/gorm"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/rest"

	"github.com/lolozini/quetzal/internal/models"
	"github.com/lolozini/quetzal/internal/store"
	"github.com/lolozini/quetzal/internal/testdb"
	"github.com/lolozini/quetzal/internal/totp"
)

func identityServer(t *testing.T) (*Server, *models.User) {
	t.Helper()
	st, err := store.Open(store.Config{Driver: store.Driver(testdb.Driver()), DSN: testdb.DSN(t, "identity.db"), Silent: true})
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Migrate(); err != nil {
		t.Fatal(err)
	}
	u := uploadUser(t, st, "alice")
	return New(st, fake.NewSimpleClientset(), &rest.Config{}), u
}

func identityRequest(u *models.User, body string) *http.Request {
	r := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body))
	return r.WithContext(context.WithValue(r.Context(), userCtxKey, u))
}

func TestSensitiveAuthenticationBudgets(t *testing.T) {
	for _, route := range []string{"password", "disable2fa"} {
		t.Run(route, func(t *testing.T) {
			s, u := identityServer(t)
			if err := s.Store.EnableUserTOTP(u.ID, []string{totp.HashRecovery("recovery")}); err != nil {
				t.Fatal(err)
			}
			u.TOTPEnabled = true
			for i := range 11 {
				w := httptest.NewRecorder()
				if route == "password" {
					s.handleChangePassword(w, identityRequest(u, `{"oldPassword":"wrong","newPassword":"password123"}`))
				} else {
					s.handle2FADisable(w, identityRequest(u, `{"code":"invalid"}`))
				}
				if i == 10 && (w.Code != http.StatusTooManyRequests || w.Header().Get("Retry-After") == "") {
					t.Errorf("exhausted %s budget: status=%d Retry-After=%q", route, w.Code, w.Header().Get("Retry-After"))
				}
			}
		})
	}
}

func TestSessionRejectsStalePasswordProof(t *testing.T) {
	s, u := identityServer(t)
	// The verified user snapshot belongs to a login paused before issuance.
	if err := s.Store.UpdateUserPassword(u.ID, "changed", u.PasswordHash, ""); err != nil {
		t.Fatal(err)
	}
	w := httptest.NewRecorder()
	if err := s.startSession(w, u); err == nil {
		t.Error("session issued from stale password proof")
	}
	if len(w.Result().Cookies()) != 0 {
		t.Error("stale proof received session cookies")
	}
}

func TestResetTokenConcurrentConsumption(t *testing.T) {
	s, u := identityServer(t)
	if err := s.Store.CreatePasswordReset(&models.PasswordReset{UserID: u.ID, TokenHash: hashToken("reset"), ExpiresAt: time.Now().Add(time.Hour)}, u); err != nil {
		t.Fatal(err)
	}
	read, resume := make(chan struct{}), make(chan struct{})
	var reads atomic.Int32
	if err := s.Store.DB().Callback().Query().After("gorm:query").Register("test:reset_snapshot", func(tx *gorm.DB) {
		if tx.Statement.Table == "password_resets" && reads.Add(1) == 1 {
			close(read)
			<-resume
		}
	}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Store.DB().Callback().Query().Remove("test:reset_snapshot") })
	first := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		w := httptest.NewRecorder()
		s.handleResetPassword(w, identityRequest(u, `{"token":"reset","password":"firstpassword"}`))
		first <- w
	}()
	<-read
	w := httptest.NewRecorder()
	s.handleResetPassword(w, identityRequest(u, `{"token":"reset","password":"secondpassword"}`))
	close(resume)
	a := <-first
	if w.Code != http.StatusNoContent {
		t.Fatalf("winning reset: %d %s", w.Code, w.Body.String())
	}
	if a.Code != http.StatusBadRequest {
		t.Errorf("reused reset: %d %s, want 400", a.Code, a.Body.String())
	}
}

func TestLoginConcurrentPasswordChange(t *testing.T) {
	s, u := identityServer(t)
	read, resume := make(chan struct{}), make(chan struct{})
	var reads atomic.Int32
	if err := s.Store.DB().Callback().Query().After("gorm:query").Register("test:login_snapshot", func(tx *gorm.DB) {
		if tx.Statement.Table == "users" && reads.Add(1) == 1 {
			close(read)
			<-resume
		}
	}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Store.DB().Callback().Query().Remove("test:login_snapshot") })
	done := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		w := httptest.NewRecorder()
		s.handleLogin(w, identityRequest(u, `{"username":"alice","password":"alicepw1234"}`))
		done <- w
	}()
	<-read
	err := s.Store.UpdateUserPassword(u.ID, "changed", u.PasswordHash, "")
	close(resume)
	w := <-done
	if err != nil {
		t.Fatal(err)
	}
	if w.Code != http.StatusUnauthorized {
		t.Errorf("stale login: %d %s, want 401", w.Code, w.Body.String())
	}
	if len(w.Result().Cookies()) != 0 {
		t.Error("stale login issued cookies")
	}
}
