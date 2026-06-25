package models

import "time"

// User is a panel account. Admins have unrestricted access; regular users own
// their servers and may be granted scoped access to others as subusers.
type User struct {
	ID           uint   `gorm:"primaryKey" json:"id"`
	Username     string `gorm:"uniqueIndex;size:190" json:"username"`
	PasswordHash string `json:"-"` // argon2id encoded string, never serialized
	// Email is optional and used for self-service password reset. It is not
	// verified (no confirmation flow); login is always by username.
	Email   string `gorm:"index;size:190" json:"email,omitempty"`
	IsAdmin bool   `json:"isAdmin"` // superadmin: holds every admin permission implicitly

	// AdminRoleID grants a scoped, non-superadmin a bundle of admin permissions
	// (see AdminRole). Nil means no delegated admin access. Ignored when IsAdmin.
	AdminRoleID *uint     `gorm:"index" json:"adminRoleId,omitempty"`
	CreatedAt   time.Time `json:"createdAt"`
	UpdatedAt   time.Time `json:"updatedAt"`

	// AdminPerms is the resolved set of admin permissions this user holds (from
	// IsAdmin or their AdminRole). It is populated at load time, not stored, and
	// drives admin authorization decisions.
	AdminPerms []string `gorm:"-" json:"adminPerms,omitempty"`

	// Per-user quotas (0 = unlimited), enforced at server creation. Admins are
	// exempt.
	MaxServers  int   `json:"maxServers"`
	MaxMemoryMB int64 `json:"maxMemoryMB"`
	MaxCPUMilli int64 `json:"maxCpuMilli"`

	// Two-factor authentication (opt-in TOTP). TOTPSecretEnc holds the encrypted
	// base32 secret (reversible: needed to compute codes). TOTPEnabled is set
	// once the user confirms enrollment with a valid code. RecoveryCodes are
	// SHA-256 hashes of the still-unused single-use codes. None are serialized.
	TOTPSecretEnc string   `json:"-"`
	TOTPEnabled   bool     `json:"twoFactorEnabled"`
	RecoveryCodes []string `gorm:"serializer:json" json:"-"`
}

// HasAdminPerm reports whether the user holds admin permission p. Superadmins
// hold all of them; scoped admins hold the resolved set from their role.
func (u *User) HasAdminPerm(p string) bool {
	if u == nil {
		return false
	}
	if u.IsAdmin {
		return true
	}
	for _, x := range u.AdminPerms {
		if x == p {
			return true
		}
	}
	return false
}

// IsAnyAdmin reports whether the user has any administrative access at all
// (superadmin or a non-empty scoped role). Useful to decide whether to surface
// the admin UI.
func (u *User) IsAnyAdmin() bool {
	return u != nil && (u.IsAdmin || len(u.AdminPerms) > 0)
}

// Session is a server-side bearer session. Token holds the SHA-256 hash of the
// opaque random token (the plaintext lives only in the client's cookie), so a
// read-only store leak can't be replayed.
type Session struct {
	Token     string    `gorm:"primaryKey;size:128" json:"-"`
	UserID    uint      `gorm:"index" json:"userId"`
	ExpiresAt time.Time `json:"expiresAt"`
	CreatedAt time.Time `json:"createdAt"`
}
