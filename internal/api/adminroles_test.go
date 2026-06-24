package api_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/lolozini/quetzal/internal/models"
)

// createRole creates an admin role as the superadmin and returns its ID.
func createRole(t *testing.T, admin *http.Client, ts, name string, perms []string) uint {
	t.Helper()
	r := post(t, admin, ts+"/api/admin-roles", map[string]any{"name": name, "permissions": perms})
	if r.StatusCode != http.StatusCreated {
		t.Fatalf("create role %s = %d", name, r.StatusCode)
	}
	var role struct{ ID uint }
	json.NewDecoder(r.Body).Decode(&role)
	return role.ID
}

// setUserRole assigns a role to a user (PUT, expects 200) as the superadmin.
func setUserRole(t *testing.T, admin *http.Client, ts string, uid uint, roleID *uint) *http.Response {
	t.Helper()
	return doPut(t, admin, ts+"/api/users/"+itoa(uid)+"/admin-role", map[string]any{"roleId": roleID})
}

func doPut(t *testing.T, c *http.Client, url string, body any) *http.Response {
	return doMethod(t, c, http.MethodPut, url, body)
}

func doPatch(t *testing.T, c *http.Client, url string, body any) *http.Response {
	return doMethod(t, c, http.MethodPatch, url, body)
}

func doMethod(t *testing.T, c *http.Client, method, url string, body any) *http.Response {
	t.Helper()
	var b []byte
	if body != nil {
		b, _ = json.Marshal(body)
	}
	req, _ := http.NewRequest(method, url, bytes.NewReader(b))
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, url, err)
	}
	return resp
}

// userID looks up a user's ID by username via the admin user list.
func userID(t *testing.T, admin *http.Client, ts, username string) uint {
	t.Helper()
	var users []models.User
	getJSON(t, admin, ts+"/api/users", &users)
	for _, u := range users {
		if u.Username == username {
			return u.ID
		}
	}
	t.Fatalf("user %q not found", username)
	return 0
}

func TestScopedAdminCapabilityGating(t *testing.T) {
	srv, admin := newTestServer(t)
	post(t, admin, srv.URL+"/api/setup", map[string]string{"username": "admin", "password": "supersecret"})

	// A role granting only users+audit.
	roleID := createRole(t, admin, srv.URL, "ops", []string{models.AdminPermUsers, models.AdminPermAudit})
	createUser(t, admin, srv.URL, map[string]any{"username": "olivia", "password": "oliviapw1"})
	oid := userID(t, admin, srv.URL, "olivia")
	if r := setUserRole(t, admin, srv.URL, oid, &roleID); r.StatusCode != http.StatusOK {
		t.Fatalf("assign role = %d", r.StatusCode)
	}

	olivia := loginAs(t, srv.URL, "olivia", "oliviapw1")

	// /api/me reflects resolved admin perms.
	var me models.User
	getJSON(t, olivia, srv.URL+"/api/me", &me)
	if !me.HasAdminPerm(models.AdminPermUsers) || !me.HasAdminPerm(models.AdminPermAudit) {
		t.Errorf("me.adminPerms = %v, want users+audit", me.AdminPerms)
	}
	if me.IsAdmin {
		t.Error("scoped admin must not be superadmin")
	}

	// Granted capabilities → 200.
	for _, path := range []string{"/api/users", "/api/audit"} {
		if r, _ := olivia.Get(srv.URL + path); r.StatusCode != http.StatusOK {
			t.Errorf("GET %s = %d, want 200", path, r.StatusCode)
		}
	}
	// Ungranted capabilities → 403.
	for _, path := range []string{"/api/email-settings", "/api/database-hosts"} {
		if r, _ := olivia.Get(srv.URL + path); r.StatusCode != http.StatusForbidden {
			t.Errorf("GET %s = %d, want 403", path, r.StatusCode)
		}
	}
	// Managing the admin-role system itself is superadmin-only.
	if r, _ := olivia.Get(srv.URL + "/api/admin-roles"); r.StatusCode != http.StatusForbidden {
		t.Errorf("scoped admin GET /api/admin-roles = %d, want 403", r.StatusCode)
	}
}

func TestUsersAdminCannotEscalate(t *testing.T) {
	srv, admin := newTestServer(t)
	post(t, admin, srv.URL+"/api/setup", map[string]string{"username": "admin", "password": "supersecret"})

	roleID := createRole(t, admin, srv.URL, "user-mgr", []string{models.AdminPermUsers})
	createUser(t, admin, srv.URL, map[string]any{"username": "uma", "password": "umapw1234"})
	uid := userID(t, admin, srv.URL, "uma")
	setUserRole(t, admin, srv.URL, uid, &roleID)
	uma := loginAs(t, srv.URL, "uma", "umapw1234")

	// Can create a regular user.
	if r := post(t, uma, srv.URL+"/api/users", map[string]any{"username": "reg", "password": "regpw1234"}); r.StatusCode != http.StatusCreated {
		t.Errorf("users-admin create regular user = %d, want 201", r.StatusCode)
	}
	// Cannot mint an admin.
	if r := post(t, uma, srv.URL+"/api/users", map[string]any{"username": "sneak", "password": "sneakpw12", "isAdmin": true}); r.StatusCode != http.StatusForbidden {
		t.Errorf("users-admin create admin = %d, want 403", r.StatusCode)
	}
	// Cannot promote an existing user to admin.
	regID := userID(t, admin, srv.URL, "reg")
	if r := doPatch(t, uma, srv.URL+"/api/users/"+itoa(regID), map[string]any{"isAdmin": true}); r.StatusCode != http.StatusForbidden {
		t.Errorf("users-admin promote = %d, want 403", r.StatusCode)
	}
	// Cannot modify the existing superadmin.
	adminID := userID(t, admin, srv.URL, "admin")
	if r := doPatch(t, uma, srv.URL+"/api/users/"+itoa(adminID), map[string]any{"isAdmin": true}); r.StatusCode != http.StatusForbidden {
		t.Errorf("users-admin modify superadmin = %d, want 403", r.StatusCode)
	}
	// Cannot delete the superadmin.
	delReq, _ := http.NewRequest(http.MethodDelete, srv.URL+"/api/users/"+itoa(adminID), nil)
	if dr, _ := uma.Do(delReq); dr.StatusCode != http.StatusForbidden {
		t.Errorf("users-admin delete superadmin = %d, want 403", dr.StatusCode)
	}
	// Cannot assign admin roles (superadmin territory).
	if r := setUserRole(t, uma, srv.URL, regID, &roleID); r.StatusCode != http.StatusForbidden {
		t.Errorf("users-admin assign role = %d, want 403", r.StatusCode)
	}

	// Cannot touch a SCOPED admin either: resetting that account's password
	// would let uma log in as it and inherit its permissions. Give a victim the
	// settings role and confirm uma can neither reset its password nor delete it.
	settingsRole := createRole(t, admin, srv.URL, "settings-only", []string{models.AdminPermSettings})
	createUser(t, admin, srv.URL, map[string]any{"username": "vic", "password": "vicpw1234"})
	vicID := userID(t, admin, srv.URL, "vic")
	setUserRole(t, admin, srv.URL, vicID, &settingsRole)
	if r := doPatch(t, uma, srv.URL+"/api/users/"+itoa(vicID), map[string]any{"password": "hijacked1"}); r.StatusCode != http.StatusForbidden {
		t.Errorf("users-admin reset scoped-admin password = %d, want 403", r.StatusCode)
	}
	delVic, _ := http.NewRequest(http.MethodDelete, srv.URL+"/api/users/"+itoa(vicID), nil)
	if dr, _ := uma.Do(delVic); dr.StatusCode != http.StatusForbidden {
		t.Errorf("users-admin delete scoped admin = %d, want 403", dr.StatusCode)
	}
}

func TestServersAdminScopeSeesAllServers(t *testing.T) {
	srv, admin := newTestServer(t)
	post(t, admin, srv.URL+"/api/setup", map[string]string{"username": "admin", "password": "supersecret"})

	// alice owns a server.
	createUser(t, admin, srv.URL, map[string]any{"username": "alice", "password": "alicepw12"})
	alice := loginAs(t, srv.URL, "alice", "alicepw12")
	var created struct{ ID uint }
	r := post(t, alice, srv.URL+"/api/servers", map[string]any{"name": "alice srv", "template": "generic-process"})
	if r.StatusCode != http.StatusCreated {
		t.Fatalf("alice create = %d", r.StatusCode)
	}
	json.NewDecoder(r.Body).Decode(&created)
	url := srv.URL + "/api/servers/" + itoa(created.ID)

	// A servers-admin sees and can power any server, and can suspend it.
	srvRole := createRole(t, admin, srv.URL, "srv-admin", []string{models.AdminPermServers})
	createUser(t, admin, srv.URL, map[string]any{"username": "sam", "password": "sampw1234"})
	setUserRole(t, admin, srv.URL, userID(t, admin, srv.URL, "sam"), &srvRole)
	sam := loginAs(t, srv.URL, "sam", "sampw1234")

	var list []map[string]any
	getJSON(t, sam, srv.URL+"/api/servers", &list)
	if len(list) != 1 {
		t.Errorf("servers-admin should see 1 server, saw %d", len(list))
	}
	if rr, _ := sam.Get(url); rr.StatusCode != http.StatusOK {
		t.Errorf("servers-admin GET server = %d, want 200", rr.StatusCode)
	}
	if rr := post(t, sam, url+"/suspend", nil); rr.StatusCode != http.StatusOK {
		t.Errorf("servers-admin suspend = %d, want 200", rr.StatusCode)
	}

	// A users-admin (no servers scope) sees no servers and gets 404 on alice's.
	uRole := createRole(t, admin, srv.URL, "u-only", []string{models.AdminPermUsers})
	createUser(t, admin, srv.URL, map[string]any{"username": "ned", "password": "nedpw1234"})
	setUserRole(t, admin, srv.URL, userID(t, admin, srv.URL, "ned"), &uRole)
	ned := loginAs(t, srv.URL, "ned", "nedpw1234")
	getJSON(t, ned, srv.URL+"/api/servers", &list)
	if len(list) != 0 {
		t.Errorf("users-admin should see 0 servers, saw %d", len(list))
	}
	if rr, _ := ned.Get(url); rr.StatusCode != http.StatusNotFound {
		t.Errorf("users-admin GET other's server = %d, want 404", rr.StatusCode)
	}
}

func TestAdminRoleCRUDValidation(t *testing.T) {
	srv, admin := newTestServer(t)
	post(t, admin, srv.URL+"/api/setup", map[string]string{"username": "admin", "password": "supersecret"})

	// Invalid permission → 400.
	if r := post(t, admin, srv.URL+"/api/admin-roles", map[string]any{"name": "bad", "permissions": []string{"root"}}); r.StatusCode != http.StatusBadRequest {
		t.Errorf("invalid perm = %d, want 400", r.StatusCode)
	}
	// Missing name → 400.
	if r := post(t, admin, srv.URL+"/api/admin-roles", map[string]any{"permissions": []string{"users"}}); r.StatusCode != http.StatusBadRequest {
		t.Errorf("missing name = %d, want 400", r.StatusCode)
	}
	// Create, then duplicate name → 409.
	id := createRole(t, admin, srv.URL, "dup", []string{models.AdminPermUsers})
	if r := post(t, admin, srv.URL+"/api/admin-roles", map[string]any{"name": "dup", "permissions": []string{"audit"}}); r.StatusCode != http.StatusConflict {
		t.Errorf("duplicate name = %d, want 409", r.StatusCode)
	}

	// Deleting a role assigned to a user → 409; after clearing → 204.
	createUser(t, admin, srv.URL, map[string]any{"username": "dia", "password": "diapw1234"})
	did := userID(t, admin, srv.URL, "dia")
	setUserRole(t, admin, srv.URL, did, &id)
	delReq, _ := http.NewRequest(http.MethodDelete, srv.URL+"/api/admin-roles/"+itoa(id), nil)
	if dr, _ := admin.Do(delReq); dr.StatusCode != http.StatusConflict {
		t.Errorf("delete assigned role = %d, want 409", dr.StatusCode)
	}
	setUserRole(t, admin, srv.URL, did, nil)
	delReq2, _ := http.NewRequest(http.MethodDelete, srv.URL+"/api/admin-roles/"+itoa(id), nil)
	if dr, _ := admin.Do(delReq2); dr.StatusCode != http.StatusNoContent {
		t.Errorf("delete cleared role = %d, want 204", dr.StatusCode)
	}
}

func TestAssignRoleToSuperadminRejected(t *testing.T) {
	srv, admin := newTestServer(t)
	post(t, admin, srv.URL+"/api/setup", map[string]string{"username": "admin", "password": "supersecret"})
	roleID := createRole(t, admin, srv.URL, "r", []string{models.AdminPermUsers})
	adminID := userID(t, admin, srv.URL, "admin")
	if r := setUserRole(t, admin, srv.URL, adminID, &roleID); r.StatusCode != http.StatusConflict {
		t.Errorf("assign role to superadmin = %d, want 409", r.StatusCode)
	}
}
