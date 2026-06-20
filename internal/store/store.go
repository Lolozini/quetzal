// Package store is the database layer. The database is Quetzal's source of
// truth; the controller reconciles its contents into Kubernetes objects.
package store

import (
	"errors"
	"fmt"

	"github.com/glebarez/sqlite"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"

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
}

// Store wraps the database handle and exposes typed operations.
type Store struct {
	db *gorm.DB
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
	return &Store{db: db}, nil
}

// Migrate creates/updates the schema.
func (s *Store) Migrate() error {
	return s.db.AutoMigrate(&models.Template{}, &models.Server{})
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

// UpdateServerStatus persists only the status field. It uses Updates with a
// typed struct (not Update with a raw value) so GORM applies the JSON
// serializer registered on the Status field.
func (s *Store) UpdateServerStatus(id uint, st models.Status) error {
	return s.db.Model(&models.Server{}).Where("id = ?", id).
		Select("status").Updates(models.Server{Status: st}).Error
}

// DeleteServer removes a server record.
func (s *Store) DeleteServer(id uint) error {
	return s.db.Delete(&models.Server{}, id).Error
}
