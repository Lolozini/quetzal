package store

import (
	"testing"

	"github.com/lolozini/quetzal/internal/models"
)

func TestAdminRoleResolutionViaGetUser(t *testing.T) {
	s := newTestStore(t)

	role := &models.AdminRole{Name: "tpl", Permissions: []string{models.AdminPermTemplates, models.AdminPermAudit}}
	if err := s.CreateAdminRole(role); err != nil {
		t.Fatalf("create role: %v", err)
	}
	u := &models.User{Username: "scoped", PasswordHash: "x"}
	if err := s.CreateUser(u); err != nil {
		t.Fatalf("create user: %v", err)
	}

	// No role yet → no admin perms.
	got, err := s.GetUser(u.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if len(got.AdminPerms) != 0 {
		t.Errorf("unassigned user has perms: %v", got.AdminPerms)
	}

	// Assign the role → GetUser resolves the role's perms.
	if err := s.SetUserAdminRole(u.ID, &role.ID); err != nil {
		t.Fatalf("assign: %v", err)
	}
	got, _ = s.GetUser(u.ID)
	if !got.HasAdminPerm(models.AdminPermTemplates) || !got.HasAdminPerm(models.AdminPermAudit) {
		t.Errorf("resolved perms = %v, want templates+audit", got.AdminPerms)
	}
	if got.HasAdminPerm(models.AdminPermUsers) {
		t.Error("should not hold ungranted users perm")
	}

	// ListUsers resolves perms in bulk too.
	us, err := s.ListUsers()
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	var found bool
	for _, x := range us {
		if x.ID == u.ID {
			found = true
			if !x.HasAdminPerm(models.AdminPermTemplates) {
				t.Errorf("ListUsers perms = %v, want templates", x.AdminPerms)
			}
		}
	}
	if !found {
		t.Fatal("user not in ListUsers")
	}

	// Clearing the role removes the perms.
	if err := s.SetUserAdminRole(u.ID, nil); err != nil {
		t.Fatalf("clear: %v", err)
	}
	got, _ = s.GetUser(u.ID)
	if len(got.AdminPerms) != 0 {
		t.Errorf("after clear, perms = %v", got.AdminPerms)
	}
}

func TestSuperadminResolvesAllPerms(t *testing.T) {
	s := newTestStore(t)
	u := &models.User{Username: "root", PasswordHash: "x", IsAdmin: true}
	if err := s.CreateUser(u); err != nil {
		t.Fatalf("create: %v", err)
	}
	got, _ := s.GetUser(u.ID)
	for _, p := range models.AllAdminPermissions {
		if !got.HasAdminPerm(p) {
			t.Errorf("superadmin missing resolved perm %q (AdminPerms=%v)", p, got.AdminPerms)
		}
	}
}

func TestCountUsersByAdminRole(t *testing.T) {
	s := newTestStore(t)
	role := &models.AdminRole{Name: "ops"}
	if err := s.CreateAdminRole(role); err != nil {
		t.Fatalf("create role: %v", err)
	}
	for _, name := range []string{"a", "b"} {
		u := &models.User{Username: name, PasswordHash: "x"}
		if err := s.CreateUser(u); err != nil {
			t.Fatalf("create %s: %v", name, err)
		}
		if err := s.SetUserAdminRole(u.ID, &role.ID); err != nil {
			t.Fatalf("assign %s: %v", name, err)
		}
	}
	n, err := s.CountUsersByAdminRole(role.ID)
	if err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 2 {
		t.Errorf("count = %d, want 2", n)
	}
}

func TestUpdateAdminRolePersistsEmptyPerms(t *testing.T) {
	s := newTestStore(t)
	role := &models.AdminRole{Name: "r", Permissions: []string{models.AdminPermUsers}}
	if err := s.CreateAdminRole(role); err != nil {
		t.Fatalf("create: %v", err)
	}
	// Updating to an empty permission set must actually clear them (the JSON
	// serializer + Select pattern persists the zero value).
	if err := s.UpdateAdminRole(role.ID, "r2", "desc", nil); err != nil {
		t.Fatalf("update: %v", err)
	}
	got, err := s.GetAdminRole(role.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Name != "r2" || got.Description != "desc" {
		t.Errorf("update didn't persist name/desc: %+v", got)
	}
	if len(got.Permissions) != 0 {
		t.Errorf("permissions = %v, want empty", got.Permissions)
	}
}
