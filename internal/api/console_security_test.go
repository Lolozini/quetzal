package api_test

import (
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/lolozini/quetzal/internal/models"
)

func TestConsoleClosesOversizedMessage(t *testing.T) {
	ts, _, st := newTestServerStore(t)
	u := &models.User{Username: "console-owner"}
	if err := st.CreateUser(u); err != nil {
		t.Fatal(err)
	}
	srv := &models.Server{Slug: "console-limit", Namespace: "console-limit", OwnerID: u.ID, DesiredState: models.StateRunning}
	if err := st.CreateServer(srv); err != nil {
		t.Fatal(err)
	}
	token := "console-limit-session"
	hash := sha256.Sum256([]byte(token))
	if err := st.CreateSession(&models.Session{Token: hex.EncodeToString(hash[:]), UserID: u.ID, ExpiresAt: time.Now().Add(time.Hour)}, u.PasswordHash); err != nil {
		t.Fatal(err)
	}
	ws, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(ts.URL, "http")+"/api/servers/"+itoa(srv.ID)+"/console", http.Header{"Authorization": {"Bearer " + token}})
	if err != nil {
		t.Fatal(err)
	}
	defer ws.Close()
	// 64 KiB is enough to exercise framing, without a memory exhaustion payload.
	if err := ws.WriteMessage(websocket.TextMessage, []byte(strings.Repeat("x", 64*1024))); err != nil {
		t.Fatal(err)
	}
	_ = ws.SetReadDeadline(time.Now().Add(2 * time.Second))
	for {
		_, _, err := ws.ReadMessage()
		if err != nil {
			if !websocket.IsCloseError(err, websocket.CloseMessageTooBig) {
				t.Fatalf("want close 1009, got %v", err)
			}
			return
		}
	}
}

func TestConsoleRevokesExistingConnection(t *testing.T) {
	for _, kind := range []string{"session", "expired-session", "key", "access", "idle-access"} {
		t.Run(kind, func(t *testing.T) {
			ts, _, st := newTestServerStore(t)
			u := &models.User{Username: "console-subuser"}
			if err := st.CreateUser(u); err != nil {
				t.Fatal(err)
			}
			srv := &models.Server{Slug: "console-revoke", Namespace: "console-revoke", DesiredState: models.StateRunning}
			if err := st.CreateServer(srv); err != nil {
				t.Fatal(err)
			}
			if err := st.GrantAccess(srv.ID, u.ID, []string{models.PermView, models.PermConsole}); err != nil {
				t.Fatal(err)
			}
			token := "console-live-session"
			if kind == "key" {
				token = "qk_console-live-key"
			}
			hash := sha256.Sum256([]byte(token))
			hashed := hex.EncodeToString(hash[:])
			key := &models.APIKey{UserID: u.ID, Name: "console", Hash: hashed}
			if kind == "key" {
				if err := st.CreateAPIKey(key); err != nil {
					t.Fatal(err)
				}
			} else {
				if err := st.CreateSession(&models.Session{Token: hashed, UserID: u.ID, ExpiresAt: time.Now().Add(time.Hour)}, u.PasswordHash); err != nil {
					t.Fatal(err)
				}
			}
			ws, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(ts.URL, "http")+"/api/servers/"+itoa(srv.ID)+"/console", http.Header{"Authorization": {"Bearer " + token}})
			if err != nil {
				t.Fatal(err)
			}
			defer ws.Close()
			if _, _, err := ws.ReadMessage(); err != nil {
				t.Fatal(err)
			}
			switch kind {
			case "session":
				err = st.DeleteSession(hashed)
			case "expired-session":
				if err = st.DeleteSession(hashed); err == nil {
					err = st.CreateSession(&models.Session{Token: hashed, UserID: u.ID, ExpiresAt: time.Now().Add(-time.Hour)}, u.PasswordHash)
				}
			case "key":
				err = st.DeleteAPIKey(key.ID)
			default:
				err = st.RevokeAccess(srv.ID, u.ID)
			}
			if err != nil {
				t.Fatal(err)
			}
			if kind != "idle-access" {
				if err := ws.WriteJSON(map[string]string{"type": "stdin", "data": "must-not-execute"}); err != nil {
					t.Fatal(err)
				}
			}
			_ = ws.SetReadDeadline(time.Now().Add(7 * time.Second))
			for {
				_, _, err := ws.ReadMessage()
				if err != nil {
					if !websocket.IsCloseError(err, websocket.ClosePolicyViolation) {
						t.Fatalf("want authorization close 1008, got %v", err)
					}
					break
				}
			}
		})
	}
}
