package store

import (
	"errors"
	"fmt"
	"sort"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"

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
	if h.Kind != models.DBHostManaged {
		return s.db.Create(h).Error
	}
	return s.db.Transaction(func(tx *gorm.DB) error {
		if err := lockManagedDatabaseNamespaces(tx); err != nil {
			return err
		}
		// The generated ID determines the default namespace. Inserting and
		// checking in the same transaction rolls back conflicting defaults too.
		if err := tx.Create(h).Error; err != nil {
			return err
		}
		return checkManagedDatabaseNamespace(tx, h)
	})
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
	if h.Kind != models.DBHostManaged {
		return s.db.Save(h).Error
	}
	return s.db.Transaction(func(tx *gorm.DB) error {
		if err := lockManagedDatabaseNamespaces(tx); err != nil {
			return err
		}
		if err := checkManagedDatabaseNamespace(tx, h); err != nil {
			return err
		}
		return tx.Save(h).Error
	})
}

// ErrDatabaseHostNamespaceInUse refuses two hosts that would reconcile or
// garbage-collect the same namespace, including derived defaults.
var ErrDatabaseHostNamespaceInUse = errors.New("managed database namespace is already assigned to another host")

func lockManagedDatabaseNamespaces(tx *gorm.DB) error {
	if tx.Dialector.Name() != "postgres" {
		return nil // SQLite write transactions already exclude each other.
	}
	const managedNamespaceLock = 0x5155444e // "QUDN", distinct from node ports.
	return tx.Exec("SELECT pg_advisory_xact_lock(?)", managedNamespaceLock).Error
}

func checkManagedDatabaseNamespace(tx *gorm.DB, host *models.DatabaseHost) error {
	var hosts []models.DatabaseHost
	if err := tx.Select("id", "namespace").Where("kind = ? AND id <> ?", models.DBHostManaged, host.ID).Find(&hosts).Error; err != nil {
		return err
	}
	namespace := host.ManagedNamespace()
	for i := range hosts {
		if hosts[i].ManagedNamespace() == namespace {
			return ErrDatabaseHostNamespaceInUse
		}
	}
	return nil
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

// ErrDatabaseHostFull means a host has no unreserved database capacity.
var ErrDatabaseHostFull = errors.New("database host is at capacity")

// CreateServerDatabase reserves a host slot and stores the sealed credentials
// before remote provisioning. The host row serializes capacity decisions across
// API replicas; SQLite already serializes write transactions.
func (s *Store) CreateServerDatabase(d *models.ServerDatabase, password string) error {
	enc, err := s.sealValue(password)
	if err != nil {
		return err
	}
	d.PasswordEnc = enc
	return s.db.Transaction(func(tx *gorm.DB) error {
		var host models.DatabaseHost
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&host, d.HostID).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return ErrNotFound
			}
			return err
		}
		if host.MaxDatabases > 0 {
			var count int64
			if err := tx.Model(&models.ServerDatabase{}).Where("host_id = ?", host.ID).Count(&count).Error; err != nil {
				return err
			}
			if count >= int64(host.MaxDatabases) {
				return ErrDatabaseHostFull
			}
		}
		return tx.Create(d).Error
	})
}

// ErrDatabaseRotationRemote distinguishes remote failures from persistence
// failures. Compensation failures remain explicit in the returned error.
var ErrDatabaseRotationRemote = errors.New("could not rotate database password")

// RotateServerDatabasePassword serializes the remote mutation and local secret
// across replicas. The local write runs first, so a persistence failure cannot
// change the remote account. On a remote error, restore the previous secret
// while still holding the lock; rotate must permit bounded recovery even after
// the original request is canceled.
func (s *Store) RotateServerDatabasePassword(id uint, password string, rotate func(*models.ServerDatabase, string) error) error {
	enc, err := s.sealValue(password)
	if err != nil {
		return err
	}
	remoteChanged := false
	err = s.db.Transaction(func(tx *gorm.DB) error {
		var current models.ServerDatabase
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&current, id).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return ErrNotFound
			}
			return err
		}
		oldPassword, err := s.ServerDatabasePassword(&current)
		if err != nil {
			return err
		}
		if err := tx.Model(&current).Select("password_enc").
			Updates(models.ServerDatabase{PasswordEnc: enc}).Error; err != nil {
			return err
		}
		if err := rotate(&current, password); err != nil {
			if restoreErr := rotate(&current, oldPassword); restoreErr != nil {
				return fmt.Errorf("%w: %v; restoring the previous password failed: %v; remote credentials require reconciliation", ErrDatabaseRotationRemote, err, restoreErr)
			}
			return fmt.Errorf("%w: %w", ErrDatabaseRotationRemote, err)
		}
		remoteChanged = true
		return nil
	})
	if err != nil && remoteChanged {
		// COMMIT may have succeeded despite a lost response. The row lock is
		// already gone: compensating now could overwrite a later rotation.
		return fmt.Errorf("password rotation commit outcome is uncertain; remote credentials require reconciliation: %w", err)
	}
	return err
}

// DeleteServerDatabase removes a database row.
func (s *Store) DeleteServerDatabase(id uint) error {
	return s.db.Delete(&models.ServerDatabase{}, id).Error
}

// ServerDatabasePassword returns the decrypted user password.
func (s *Store) ServerDatabasePassword(d *models.ServerDatabase) (string, error) {
	return s.openValue(d.PasswordEnc)
}
