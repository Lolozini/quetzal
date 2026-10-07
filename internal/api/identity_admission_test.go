package api

import (
	"crypto/rand"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/lolozini/quetzal/internal/auth"
)

type passwordEntropyGate struct {
	entered chan struct{}
	release chan struct{}
}

func (g *passwordEntropyGate) Read(p []byte) (int, error) {
	g.entered <- struct{}{}
	<-g.release
	clear(p)
	return len(p), nil
}

func TestPasswordSaturationReturnsRetryableStatus(t *testing.T) {
	s, u := identityServer(t)
	gate := &passwordEntropyGate{entered: make(chan struct{}, 2), release: make(chan struct{})}
	original := rand.Reader
	rand.Reader = gate
	done := make(chan error, 2)
	for range 2 {
		go func() { _, err := auth.HashPassword("password"); done <- err }()
	}
	t.Cleanup(func() {
		close(gate.release)
		for range 2 {
			if err := <-done; err != nil {
				t.Error(err)
			}
		}
		rand.Reader = original
	})
	for range 2 {
		<-gate.entered
	}
	for _, username := range []string{"alice", "unknown"} {
		w := httptest.NewRecorder()
		s.handleLogin(w, identityRequest(u, `{"username":"`+username+`","password":"alicepw1234"}`))
		if w.Code != http.StatusTooManyRequests || w.Header().Get("Retry-After") != "1" {
			t.Errorf("saturated login %s: %d %q", username, w.Code, w.Header().Get("Retry-After"))
		}
	}
	w := httptest.NewRecorder()
	s.handleChangePassword(w, identityRequest(u, `{"oldPassword":"alicepw1234","newPassword":"newpassword"}`))
	if w.Code != http.StatusTooManyRequests || w.Header().Get("Retry-After") != "1" {
		t.Errorf("saturated reauthentication: %d %q", w.Code, w.Header().Get("Retry-After"))
	}
}
