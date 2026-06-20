// Package store is the database layer. The database is Quetzal's source of
// truth; the controller reconciles its contents into Kubernetes objects.
package store

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/glebarez/sqlite"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"

	"github.com/lolozini/quetzal/internal/crypto"
	"github.com/lolozini/quetzal/internal/models"
)

// ErrNotFound is returned when a record does not exist.
var ErrNotFound = errors.New("not found")

// Driver enumerates supported database engines.
type Driver string

const (
	DriverSQLite   Driver = "sqlite"   // default, zero-config homelab
	DriverPostgres Driver = "postgres" // recommended for production
)

// Config configures the database connection.
type Config struct {
	Driver Driver
	// DSN is the data source name. For sqlite this is a file path (default
	// "quetzal.db"); for postgres a libpq/gorm connection string.
	DSN    string
	Silent bool
	// SecretKey (32 bytes) encrypts sensitive server values at rest. When empty,
	// such values are stored obfuscated-but-unencrypted (dev only) with a warning.
	SecretKey []byte
}

// Store wraps the database handle and exposes typed operations.
type Store struct {
	db  *gorm.DB
	key []byte
}

// Open opens (and pings) the database for the given config.
func Open(cfg Config) (*Store, error) {
	gcfg := &gorm.Config{}
	if cfg.Silent {
		gcfg.Logger = logger.Default.LogMode(logger.Silent)
	}

	var dialector gorm.Dialector
	switch cfg.Driver {
	case "", DriverSQLite:
		dsn := cfg.DSN
		if dsn == "" {
			dsn = "quetzal.db"
		}
		dialector = sqlite.Open(dsn)
	case DriverPostgres:
		dialector = postgres.Open(cfg.DSN)
	default:
		return nil, fmt.Errorf("unsupported db driver %q", cfg.Driver)
	}

	db, err := gorm.Open(dialector, gcfg)
	if err != nil {
		return nil, fmt.Errorf("open database: %w", err)
	}
	if len(cfg.SecretKey) == 0 {
		log.Printf("warning: QUETZAL_SECRET_KEY not set; server secrets will NOT be encrypted at rest")
	}
	return &Store{db: db, key: cfg.SecretKey}, nil
}

const (
	secretPrefixEnc   = "enc:"
	secretPrefixPlain = "plain:"
)

// SealSecrets serializes and (when a key is configured) encrypts a secret env
// map for storage. Returns "" for an empty map.
func (s *Store) SealSecrets(m map[string]string) (string, error) {
	if len(m) == 0 {
		return "", nil
	}
	b, err := json.Marshal(m)
	if err != nil {
		return "", err
	}
	if len(s.key) == 0 {
		return secretPrefixPlain + base64.StdEncoding.EncodeToString(b), nil
	}
	ct, err := crypto.Seal(s.key, b)
	if err != nil {
		return "", err
	}
	return secretPrefixEnc + ct, nil
}

// OpenSecrets reverses SealSecrets.
func (s *Store) OpenSecrets(blob string) (map[string]string, error) {
	m := map[string]string{}
	if blob == "" {
		return m, nil
	}
	switch {
	case strings.HasPrefix(blob, secretPrefixEnc):
		if len(s.key) == 0 {
			return nil, errors.New("encrypted secrets present but no key configured")
		}
		pt, err := crypto.Open(s.key, strings.TrimPrefix(blob, secretPrefixEnc))
		if err != nil {
			return nil, err
		}
		return m, json.Unmarshal(pt, &m)
	case strings.HasPrefix(blob, secretPrefixPlain):
		b, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(blob, secretPrefixPlain))
		if err != nil {
			return nil, err
		}
		return m, json.Unmarshal(b, &m)
	default:
		return m, json.Unmarshal([]byte(blob), &m)
	}
}

// Migrate creates/updates the schema.
func (s *Store) Migrate() error {
	return s.db.AutoMigrate(
		&models.Template{}, &models.Server{}, &models.User{},
		&models.Session{}, &models.PortAllocation{}, &models.Schedule{},
	)
}

// DB exposes the underlying handle (for advanced/transactional use).
func (s *Store) DB() *gorm.DB { return s.db }

// ---- Templates ----

// UpsertTemplate inserts or updates a template by slug, bumping its version on
// change. Returns the stored template.
func (s *Store) UpsertTemplate(t *models.Template) (*models.Template, error) {
	var existing models.Template
	err := s.db.Where("slug = ?", t.Slug).First(&existing).Error
	switch {
	case errors.Is(err, gorm.ErrRecordNotFound):
		if t.Version == 0 {
			t.Version = 1
		}
		if err := s.db.Create(t).Error; err != nil {
			return nil, err
		}
		return t, nil
	case err != nil:
		return nil, err
	}
	t.ID = existing.ID
	t.Version = existing.Version + 1
	if err := s.db.Save(t).Error; err != nil {
		return nil, err
	}
	return t, nil
}

// GetTemplate returns a template by ID.
func (s *Store) GetTemplate(id uint) (*models.Template, error) {
	var t models.Template
	if err := s.db.First(&t, id).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	return &t, nil
}

// GetTemplateBySlug returns a template by slug.
func (s *Store) GetTemplateBySlug(slug string) (*models.Template, error) {
	var t models.Template
	if err := s.db.Where("slug = ?", slug).First(&t).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	return &t, nil
}

// ListTemplates returns all templates.
func (s *Store) ListTemplates() ([]models.Template, error) {
	var ts []models.Template
	if err := s.db.Order("name asc").Find(&ts).Error; err != nil {
		return nil, err
	}
	return ts, nil
}

// ---- Servers ----

// CreateServer inserts a new server.
func (s *Store) CreateServer(srv *models.Server) error {
	return s.db.Create(srv).Error
}

// GetServer returns a server by ID.
func (s *Store) GetServer(id uint) (*models.Server, error) {
	var srv models.Server
	if err := s.db.First(&srv, id).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	return &srv, nil
}

// GetServerBySlug returns a server by slug.
func (s *Store) GetServerBySlug(slug string) (*models.Server, error) {
	var srv models.Server
	if err := s.db.Where("slug = ?", slug).First(&srv).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	return &srv, nil
}

// ListServers returns all servers.
func (s *Store) ListServers() ([]models.Server, error) {
	var srvs []models.Server
	if err := s.db.Order("created_at asc").Find(&srvs).Error; err != nil {
		return nil, err
	}
	return srvs, nil
}

// UpdateServer persists the full server record.
func (s *Store) UpdateServer(srv *models.Server) error {
	return s.db.Save(srv).Error
}

// SetDesiredState updates only the power state, avoiding a full-row Save that
// could clobber the controller-written status.
func (s *Store) SetDesiredState(id uint, state models.DesiredState) error {
	return s.db.Model(&models.Server{}).Where("id = ?", id).
		Update("desired_state", string(state)).Error
}

// UpdateServerStatus persists only the status field. It uses Updates with a
// typed struct (not Update with a raw value) so GORM applies the JSON
// serializer registered on the Status field.
func (s *Store) UpdateServerStatus(id uint, st models.Status) error {
	return s.db.Model(&models.Server{}).Where("id = ?", id).
		Select("status").Updates(models.Server{Status: st}).Error
}

// UpdateServerNetworking persists only the exposure config and the (re)computed
// port list, leaving controller-written status untouched.
func (s *Store) UpdateServerNetworking(id uint, expose models.Expose, ports []models.PortSpec) error {
	return s.db.Model(&models.Server{}).Where("id = ?", id).
		Select("expose", "ports").
		Updates(models.Server{Expose: expose, Ports: ports}).Error
}

// DeleteServer removes a server record and frees any node ports it held.
func (s *Store) DeleteServer(id uint) error {
	return s.db.Transaction(func(tx *gorm.DB) error {
		if err := tx.Where("server_id = ?", id).Delete(&models.PortAllocation{}).Error; err != nil {
			return err
		}
		return tx.Delete(&models.Server{}, id).Error
	})
}

// ---- Node port pool ----

// Default node port range mirrors Kubernetes' default service node port range.
const (
	DefaultNodePortMin int32 = 30000
	DefaultNodePortMax int32 = 32767
)

// AllocateNodePort reserves the lowest free node port in [min,max] for a named
// server port, persisting it so it stays stable across reconciles. If the
// (server, name) pair already holds an allocation it is returned unchanged.
// A min/max of 0 falls back to the Kubernetes default range.
func (s *Store) AllocateNodePort(serverID uint, name string, min, max int32) (int32, error) {
	if min <= 0 {
		min = DefaultNodePortMin
	}
	if max <= 0 {
		max = DefaultNodePortMax
	}
	if min > max {
		return 0, fmt.Errorf("invalid node port range %d-%d", min, max)
	}
	var alloc models.PortAllocation
	err := s.db.Transaction(func(tx *gorm.DB) error {
		err := tx.Where("server_id = ? AND port_name = ?", serverID, name).First(&alloc).Error
		switch {
		case err == nil:
			return nil // reuse existing allocation
		case !errors.Is(err, gorm.ErrRecordNotFound):
			return err
		}
		used := map[int32]bool{}
		var rows []models.PortAllocation
		if err := tx.Find(&rows).Error; err != nil {
			return err
		}
		for _, r := range rows {
			used[r.NodePort] = true
		}
		for p := min; p <= max; p++ {
			if used[p] {
				continue
			}
			alloc = models.PortAllocation{NodePort: p, ServerID: serverID, PortName: name}
			return tx.Create(&alloc).Error
		}
		return fmt.Errorf("no free node port in range %d-%d", min, max)
	})
	if err != nil {
		return 0, err
	}
	return alloc.NodePort, nil
}

// ReleaseServerPorts frees every node port held by a server.
func (s *Store) ReleaseServerPorts(serverID uint) error {
	return s.db.Where("server_id = ?", serverID).Delete(&models.PortAllocation{}).Error
}

// ---- Schedules ----

// CreateSchedule inserts a schedule.
func (s *Store) CreateSchedule(sc *models.Schedule) error {
	return s.db.Create(sc).Error
}

// GetSchedule returns a schedule by ID.
func (s *Store) GetSchedule(id uint) (*models.Schedule, error) {
	var sc models.Schedule
	if err := s.db.First(&sc, id).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	return &sc, nil
}

// ListSchedulesForServer returns a server's schedules.
func (s *Store) ListSchedulesForServer(serverID uint) ([]models.Schedule, error) {
	var scs []models.Schedule
	if err := s.db.Where("server_id = ?", serverID).Order("id asc").Find(&scs).Error; err != nil {
		return nil, err
	}
	return scs, nil
}

// ListEnabledSchedules returns all enabled schedules (used by the scheduler).
func (s *Store) ListEnabledSchedules() ([]models.Schedule, error) {
	var scs []models.Schedule
	if err := s.db.Where("enabled = ?", true).Find(&scs).Error; err != nil {
		return nil, err
	}
	return scs, nil
}

// UpdateSchedule persists user-editable fields of a schedule.
func (s *Store) UpdateSchedule(sc *models.Schedule) error {
	return s.db.Model(&models.Schedule{}).Where("id = ?", sc.ID).
		Select("name", "cron", "action", "payload", "enabled", "next_run").
		Updates(map[string]any{
			"name": sc.Name, "cron": sc.Cron, "action": sc.Action,
			"payload": sc.Payload, "enabled": sc.Enabled, "next_run": sc.NextRun,
		}).Error
}

// MarkScheduleRun records a schedule's execution outcome and its next due time.
func (s *Store) MarkScheduleRun(id uint, lastRun time.Time, nextRun *time.Time, status string) error {
	return s.db.Model(&models.Schedule{}).Where("id = ?", id).
		Updates(map[string]any{"last_run": lastRun, "next_run": nextRun, "last_status": status}).Error
}

// SetScheduleNextRun stores only the computed next run (e.g. on create/enable).
func (s *Store) SetScheduleNextRun(id uint, nextRun *time.Time) error {
	return s.db.Model(&models.Schedule{}).Where("id = ?", id).
		Update("next_run", nextRun).Error
}

// DeleteSchedule removes a schedule.
func (s *Store) DeleteSchedule(id uint) error {
	return s.db.Delete(&models.Schedule{}, id).Error
}

// DeleteSchedulesForServer removes all schedules of a server (used on teardown).
func (s *Store) DeleteSchedulesForServer(serverID uint) error {
	return s.db.Where("server_id = ?", serverID).Delete(&models.Schedule{}).Error
}

// ---- Users & sessions ----

// CountUsers returns the number of user accounts (used by the setup wizard).
func (s *Store) CountUsers() (int64, error) {
	var n int64
	if err := s.db.Model(&models.User{}).Count(&n).Error; err != nil {
		return 0, err
	}
	return n, nil
}

// CreateUser inserts a new user.
func (s *Store) CreateUser(u *models.User) error {
	return s.db.Create(u).Error
}

// GetUser returns a user by ID.
func (s *Store) GetUser(id uint) (*models.User, error) {
	var u models.User
	if err := s.db.First(&u, id).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	return &u, nil
}

// GetUserByUsername returns a user by username.
func (s *Store) GetUserByUsername(username string) (*models.User, error) {
	var u models.User
	if err := s.db.Where("username = ?", username).First(&u).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	return &u, nil
}

// CreateSession stores a session.
func (s *Store) CreateSession(sess *models.Session) error {
	return s.db.Create(sess).Error
}

// GetSession returns a session by token (does not check expiry).
func (s *Store) GetSession(token string) (*models.Session, error) {
	var sess models.Session
	if err := s.db.Where("token = ?", token).First(&sess).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	return &sess, nil
}

// DeleteSession removes a session (logout).
func (s *Store) DeleteSession(token string) error {
	return s.db.Where("token = ?", token).Delete(&models.Session{}).Error
}

// DeleteExpiredSessions removes all sessions past their expiry. Returns the
// number deleted.
func (s *Store) DeleteExpiredSessions() (int64, error) {
	res := s.db.Where("expires_at < ?", time.Now()).Delete(&models.Session{})
	return res.RowsAffected, res.Error
}
