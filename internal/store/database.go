package store

import (
	"errors"
	"sort"
	"time"

	"gorm.io/gorm"

	"github.com/lolozini/quetzal/internal/models"
)

// ---- database hosts (admin-managed registry) ----

// ListDatabaseHosts returns all registered hosts.
func (s *Store) ListDatabaseHosts() ([]models.DatabaseHost, error) {
	var hs []models.DatabaseHost
	err := s.db.Order("id asc").Find(&hs).Error
	return hs, err
}

// GetDatabaseHost returns a host by ID, or ErrNotFound.
func (s *Store) GetDatabaseHost(id uint) (*models.DatabaseHost, error) {
	var h models.DatabaseHost
	if err := s.db.First(&h, id).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	return &h, nil
}

// CreateDatabaseHost stores a host, sealing the admin password.
func (s *Store) CreateDatabaseHost(h *models.DatabaseHost, adminPassword string) error {
	enc, err := s.sealValue(adminPassword)
	if err != nil {
		return err
	}
	h.AdminPasswordEnc = enc
	return s.db.Create(h).Error
}

// UpdateDatabaseHost updates mutable fields. A non-nil adminPassword replaces the
// stored secret; nil keeps it.
func (s *Store) UpdateDatabaseHost(h *models.DatabaseHost, adminPassword *string) error {
	if adminPassword != nil {
		enc, err := s.sealValue(*adminPassword)
		if err != nil {
			return err
		}
		h.AdminPasswordEnc = enc
	}
	return s.db.Save(h).Error
}

// SetDatabaseHostStatus records the result of a connectivity probe.
func (s *Store) SetDatabaseHostStatus(id uint, reachable bool, msg string) error {
	now := time.Now()
	return s.db.Model(&models.DatabaseHost{ID: id}).
		Select("reachable", "status_message", "last_checked_at").
		Updates(models.DatabaseHost{Reachable: reachable, StatusMessage: msg, LastCheckedAt: &now}).Error
}

// DeleteDatabaseHost removes a host row.
func (s *Store) DeleteDatabaseHost(id uint) error {
	return s.db.Delete(&models.DatabaseHost{}, id).Error
}

// DatabaseHostAdminPassword returns the decrypted admin password for a host.
func (s *Store) DatabaseHostAdminPassword(h *models.DatabaseHost) (string, error) {
	return s.openValue(h.AdminPasswordEnc)
}

// CountDatabasesOnHost counts databases provisioned on a host (for quotas and to
// guard deletion).
func (s *Store) CountDatabasesOnHost(hostID uint) (int64, error) {
	var n int64
	err := s.db.Model(&models.ServerDatabase{}).Where("host_id = ?", hostID).Count(&n).Error
	return n, err
}

// ---- per-server databases ----

// ListServerDatabases returns a server's databases.
// ServerNamespacesUsingHost returns the namespaces of the servers holding a
// database on a host, deduplicated. It is what the managed database's ingress
// policy is built from: exactly the tenants that have a reason to reach it.
func (s *Store) ServerNamespacesUsingHost(hostID uint) ([]string, error) {
	var serverIDs []uint
	if err := s.db.Model(&models.ServerDatabase{}).
		Where("host_id = ?", hostID).Pluck("server_id", &serverIDs).Error; err != nil {
		return nil, err
	}
	if len(serverIDs) == 0 {
		return nil, nil
	}
	var namespaces []string
	if err := s.db.Model(&models.Server{}).
		Where("id IN ?", serverIDs).Pluck("namespace", &namespaces).Error; err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	out := make([]string, 0, len(namespaces))
	for _, ns := range namespaces {
		if ns == "" || seen[ns] {
			continue
		}
		seen[ns] = true
		out = append(out, ns)
	}
	sort.Strings(out) // stable order, so the policy does not churn between resyncs
	return out, nil
}

func (s *Store) ListServerDatabases(serverID uint) ([]models.ServerDatabase, error) {
	var ds []models.ServerDatabase
	err := s.db.Where("server_id = ?", serverID).Order("id asc").Find(&ds).Error
	return ds, err
}

// GetServerDatabase returns one database by ID, or ErrNotFound.
func (s *Store) GetServerDatabase(id uint) (*models.ServerDatabase, error) {
	var d models.ServerDatabase
	if err := s.db.First(&d, id).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	return &d, nil
}

// CreateServerDatabase stores a provisioned database, sealing the user password.
func (s *Store) CreateServerDatabase(d *models.ServerDatabase, password string) error {
	enc, err := s.sealValue(password)
	if err != nil {
		return err
	}
	d.PasswordEnc = enc
	return s.db.Create(d).Error
}

// UpdateServerDatabasePassword reseals a rotated user password.
func (s *Store) UpdateServerDatabasePassword(id uint, password string) error {
	enc, err := s.sealValue(password)
	if err != nil {
		return err
	}
	return s.db.Model(&models.ServerDatabase{ID: id}).Select("password_enc").
		Updates(models.ServerDatabase{PasswordEnc: enc}).Error
}

// DeleteServerDatabase removes a database row.
func (s *Store) DeleteServerDatabase(id uint) error {
	return s.db.Delete(&models.ServerDatabase{}, id).Error
}

// ServerDatabasePassword returns the decrypted user password.
func (s *Store) ServerDatabasePassword(d *models.ServerDatabase) (string, error) {
	return s.openValue(d.PasswordEnc)
}
