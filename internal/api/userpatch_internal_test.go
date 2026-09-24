package api

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/lolozini/quetzal/internal/models"
)

func patchUser(t *testing.T, s *Server, caller *models.User, id uint, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := asUser(httptest.NewRequest(http.MethodPatch, "/", strings.NewReader(body)), caller)
	req.SetPathValue("uid", fmt.Sprint(id))
	rr := httptest.NewRecorder()
	s.handleUpdateUser(rr, req)
	return rr
}

// A PATCH that only resets a password leaves the rest of the account alone. The
// admin flag and quotas used to be read as zero when omitted: the account lost
// its quotas (0 = unlimited) and an admin was demoted.
func TestPatchUserKeepsWhatIsNotSent(t *testing.T) {
	s := eggTestServer(t)
	root := &models.User{Username: "root", PasswordHash: "x", IsAdmin: true}
	other := &models.User{Username: "other", PasswordHash: "x", IsAdmin: true}
	player := &models.User{Username: "player", PasswordHash: "x", MaxServers: 2, MaxMemoryMB: 4096, MaxCPUMilli: 2000}
	for _, u := range []*models.User{root, other, player} {
		if err := s.Store.CreateUser(u); err != nil {
			t.Fatal(err)
		}
	}
	root.AdminPerms = models.AllAdminPermissions

	if rr := patchUser(t, s, root, player.ID, `{"password":"a-new-password"}`); rr.Code != http.StatusOK {
		t.Fatalf("password reset = %d %s", rr.Code, rr.Body)
	}
	got, _ := s.Store.GetUser(player.ID)
	if got.MaxServers != 2 || got.MaxMemoryMB != 4096 || got.MaxCPUMilli != 2000 {
		t.Errorf("quotas after a password reset = %d/%d/%d, want 2/4096/2000", got.MaxServers, got.MaxMemoryMB, got.MaxCPUMilli)
	}
	if got.PasswordHash == "x" {
		t.Error("the password was not changed")
	}

	if rr := patchUser(t, s, root, other.ID, `{"email":"other@example.com"}`); rr.Code != http.StatusOK {
		t.Fatalf("email change = %d %s", rr.Code, rr.Body)
	}
	if got, _ := s.Store.GetUser(other.ID); !got.IsAdmin || got.Email != "other@example.com" {
		t.Errorf("after an email change: admin=%v email=%q", got.IsAdmin, got.Email)
	}

	// Changing one quota keeps the others.
	if rr := patchUser(t, s, root, player.ID, `{"maxServers":5}`); rr.Code != http.StatusOK {
		t.Fatalf("quota change = %d %s", rr.Code, rr.Body)
	}
	if got, _ := s.Store.GetUser(player.ID); got.MaxServers != 5 || got.MaxMemoryMB != 4096 {
		t.Errorf("after a quota change: %d/%d", got.MaxServers, got.MaxMemoryMB)
	}

	// A refused field leaves the others unwritten.
	if rr := patchUser(t, s, root, player.ID, `{"maxServers":9,"password":"short"}`); rr.Code != http.StatusBadRequest {
		t.Fatalf("short password = %d", rr.Code)
	}
	if got, _ := s.Store.GetUser(player.ID); got.MaxServers != 5 {
		t.Errorf("a refused request still wrote maxServers=%d", got.MaxServers)
	}
}

// Every user may learn whether backups are configured -- the Backups tab needs
// it -- but only settings admins see the target.
func TestBackupConfigForNonAdmins(t *testing.T) {
	s := eggTestServer(t)
	if err := s.Store.SaveBackupConfig(&models.BackupConfig{Endpoint: "s3.example.com", Bucket: "b", KeepLast: 5}, "ak", "sk", "pw"); err != nil {
		t.Fatal(err)
	}
	player := &models.User{ID: 42, Username: "player"}
	rr := httptest.NewRecorder()
	s.handleGetBackupConfig(rr, asUser(httptest.NewRequest(http.MethodGet, "/", nil), player))
	if rr.Code != http.StatusOK {
		t.Fatalf("non-admin read = %d %s", rr.Code, rr.Body)
	}
	body := rr.Body.String()
	if !strings.Contains(body, `"configured":true`) || !strings.Contains(body, `"keepLast":5`) || !strings.Contains(body, `"editable":false`) {
		t.Errorf("non-admin view = %s", body)
	}
	if strings.Contains(body, "s3.example.com") || strings.Contains(body, `"bucket":"b"`) {
		t.Errorf("the target leaked to a non-admin: %s", body)
	}

	adminUser := &models.User{ID: 1, Username: "root", IsAdmin: true, AdminPerms: models.AllAdminPermissions}
	rr = httptest.NewRecorder()
	s.handleGetBackupConfig(rr, asUser(httptest.NewRequest(http.MethodGet, "/", nil), adminUser))
	if !strings.Contains(rr.Body.String(), "s3.example.com") || !strings.Contains(rr.Body.String(), `"editable":true`) {
		t.Errorf("admin view = %s", rr.Body)
	}
}
