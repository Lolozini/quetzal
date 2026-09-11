package store

import (
	"errors"

	"github.com/lolozini/quetzal/internal/models"
	"gorm.io/gorm"
)

// CreateAdminRole persists a new admin role.
func (s *Store) CreateAdminRole(r *models.AdminRole) error {
	return s.db.Create(r).Error
}

// ListAdminRoles returns all admin roles ordered by name.
// UsersWithAdminPerm returns the ids of every account holding an admin
// permission: superadmins, who hold all of them, plus the holders of a role
// that grants it.
//
// The roles are filtered in Go rather than in SQL because a role's permissions
// are a JSON column, and SQLite and Postgres do not query one the same way.
// There are only ever a handful of roles, so the cost is a single small read.
func (s *Store) UsersWithAdminPerm(perm string) ([]uint, error) {
	roles, err := s.ListAdminRoles()
	if err != nil {
		return nil, err
	}
	var roleIDs []uint
	for i := range roles {
		if roles[i].Has(perm) {
			roleIDs = append(roleIDs, roles[i].ID)
		}
	}
	q := s.db.Model(&models.User{})
	if len(roleIDs) > 0 {
		q = q.Where("is_admin = ? OR admin_role_id IN ?", true, roleIDs)
	} else {
		q = q.Where("is_admin = ?", true)
	}
	var ids []uint
	return ids, q.Pluck("id", &ids).Error
}

func (s *Store) ListAdminRoles() ([]models.AdminRole, error) {
	var rs []models.AdminRole
	if err := s.db.Order("name asc").Find(&rs).Error; err != nil {
		return nil, err
	}
	return rs, nil
}

// GetAdminRole returns an admin role by ID.
func (s *Store) GetAdminRole(id uint) (*models.AdminRole, error) {
	var r models.AdminRole
	if err := s.db.First(&r, id).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	return &r, nil
}

// UpdateAdminRole persists name, description and permissions for a role.
func (s *Store) UpdateAdminRole(id uint, name, description string, perms []string) error {
	return s.db.Model(&models.AdminRole{}).Where("id = ?", id).
		Select("name", "description", "permissions").
		Updates(models.AdminRole{Name: name, Description: description, Permissions: perms}).Error
}

// DeleteAdminRole removes a role. Callers must ensure no users are assigned.
func (s *Store) DeleteAdminRole(id uint) error {
	return s.db.Delete(&models.AdminRole{}, id).Error
}

// CountUsersByAdminRole reports how many users are assigned a given role.
func (s *Store) CountUsersByAdminRole(id uint) (int64, error) {
	var n int64
	err := s.db.Model(&models.User{}).Where("admin_role_id = ?", id).Count(&n).Error
	return n, err
}

// SetUserAdminRole assigns (or clears, when roleID is nil) a user's admin role.
func (s *Store) SetUserAdminRole(userID uint, roleID *uint) error {
	return s.db.Model(&models.User{}).Where("id = ?", userID).
		Update("admin_role_id", roleID).Error
}
