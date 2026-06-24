package models

import "testing"

func TestValidAdminPermission(t *testing.T) {
	for _, p := range AllAdminPermissions {
		if !ValidAdminPermission(p) {
			t.Errorf("%q should be valid", p)
		}
	}
	for _, p := range []string{"", "root", "delete", "view", "Servers"} {
		if ValidAdminPermission(p) {
			t.Errorf("%q should be invalid", p)
		}
	}
}

func TestAdminRoleHas(t *testing.T) {
	r := &AdminRole{Permissions: []string{AdminPermTemplates, AdminPermAudit}}
	if !r.Has(AdminPermTemplates) || !r.Has(AdminPermAudit) {
		t.Error("role should hold its granted permissions")
	}
	if r.Has(AdminPermUsers) || r.Has(AdminPermServers) {
		t.Error("role should not hold ungranted permissions")
	}
}

func TestUserHasAdminPerm(t *testing.T) {
	// Superadmin holds everything regardless of AdminPerms.
	super := &User{IsAdmin: true}
	for _, p := range AllAdminPermissions {
		if !super.HasAdminPerm(p) {
			t.Errorf("superadmin missing %q", p)
		}
	}

	// Scoped admin holds only the resolved subset.
	scoped := &User{AdminPerms: []string{AdminPermUsers}}
	if !scoped.HasAdminPerm(AdminPermUsers) {
		t.Error("scoped admin should hold users")
	}
	if scoped.HasAdminPerm(AdminPermServers) {
		t.Error("scoped admin should not hold servers")
	}

	// Plain user holds nothing; nil is safe.
	if (&User{}).HasAdminPerm(AdminPermUsers) {
		t.Error("plain user should hold no admin perms")
	}
	var nilUser *User
	if nilUser.HasAdminPerm(AdminPermUsers) {
		t.Error("nil user should hold no admin perms")
	}
}

func TestUserIsAnyAdmin(t *testing.T) {
	cases := []struct {
		name string
		u    *User
		want bool
	}{
		{"super", &User{IsAdmin: true}, true},
		{"scoped", &User{AdminPerms: []string{AdminPermAudit}}, true},
		{"plain", &User{}, false},
		{"nil", nil, false},
	}
	for _, c := range cases {
		if got := c.u.IsAnyAdmin(); got != c.want {
			t.Errorf("%s: IsAnyAdmin = %v, want %v", c.name, got, c.want)
		}
	}
}
