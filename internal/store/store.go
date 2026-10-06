// Package store is the database layer. The database is Quetzal's source of
// truth; the controller reconciles its contents into Kubernetes objects.
package store

import (
	crand "crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"math/big"
	"math/rand"
	"strings"
	"time"

	"github.com/glebarez/sqlite"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
	"gorm.io/gorm/logger"

	"github.com/lolozini/quetzal/internal/crypto"
	"github.com/lolozini/quetzal/internal/models"
)

// ErrNotFound is returned when a record does not exist.
var ErrNotFound = errors.New("not found")

// ErrDuplicate is returned when an insert violates a unique constraint (e.g. a
// server slug collision). Relies on gorm's TranslateError.
var ErrDuplicate = errors.New("duplicate")

// ErrNoFreeNodePort means the node-port range is used up.
var ErrNoFreeNodePort = errors.New("no free node port")

// ErrRestoreActive is a server that cannot be started, or given another
// restore, because a restore is waiting for its data volume or writing it.
var ErrRestoreActive = errors.New("a restore of this server's data is waiting or running")

// ErrDatabaseImportActive is a server that cannot be started, or given a
// restore or another import, because an SQL file is being loaded into one of
// its databases: the game would write to the database while it is replaced.
var ErrDatabaseImportActive = errors.New("a database import of this server is waiting or running")

// ErrServerRunning is a restore refused because the server is meant to run.
var ErrServerRunning = errors.New("the server is running")

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
	// TranslateError maps driver-specific errors (e.g. a unique-constraint
	// violation) to gorm sentinels like ErrDuplicatedKey, so callers can detect
	// them portably across SQLite and Postgres.
	gcfg := &gorm.Config{TranslateError: true}
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
		dialector = sqlite.Open(withImmediateWrites(withBusyTimeout(dsn)))
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

// withBusyTimeout gives a SQLite DSN a busy timeout when it names none. The
// apiserver and the controller are two processes writing one file, and without
// a timeout the loser of a concurrent write fails at once with "database is
// locked" -- a status update dropped, a user's request answered 500. The Helm
// chart sets one; a DSN written by hand, or the default, did not.
func withBusyTimeout(dsn string) string {
	if strings.Contains(dsn, "busy_timeout") || strings.Contains(dsn, ":memory:") {
		return dsn
	}
	sep := "?"
	if strings.Contains(dsn, "?") {
		sep = "&"
	}
	return dsn + sep + "_pragma=busy_timeout(5000)"
}

// withImmediateWrites makes a SQLite DSN begin every write transaction with
// the write lock (BEGIN IMMEDIATE) unless it says otherwise. A transaction that
// reads first and writes afterwards, as allocating a node port does, takes the
// lock only at its first write; if another write lands in between, SQLite
// fails it at once with SQLITE_BUSY rather than wait, since its reads are
// stale, and the busy timeout never comes into play. Parallel server creations
// failed that way, 7 in 15. Taken at BEGIN, the lock is waited for like any
// other. Reads outside a transaction are unaffected.
func withImmediateWrites(dsn string) string {
	if strings.Contains(dsn, "_txlock") {
		return dsn
	}
	sep := "?"
	if strings.Contains(dsn, "?") {
		sep = "&"
	}
	return dsn + sep + "_txlock=immediate"
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
//
// The apiserver and controller each run Migrate at startup against the same
// database, so a schema change (a new column) can race: both AutoMigrate calls
// see the column missing and both issue ALTER TABLE ADD COLUMN, and the loser
// fails with "duplicate column"/"already exists". That's benign — the schema is
// correct either way — so retry on exactly that error: once the other process
// finishes, AutoMigrate is a no-op and succeeds.
func (s *Store) Migrate() error {
	if err := s.dedupSSHKeys(); err != nil {
		return err
	}
	var err error
	for attempt := 0; attempt < 5; attempt++ {
		if err = s.autoMigrate(); err == nil || !isConcurrentMigrationError(err) {
			break
		}
		// Back off with a little jitter so the two processes don't lock-step.
		time.Sleep(time.Duration(100*(attempt+1))*time.Millisecond + time.Duration(rand.Intn(50))*time.Millisecond)
	}
	if err != nil {
		return err
	}
	return s.migrateUnlimitedQuotas()
}

// settingQuotaSemantics marks that quotas read 0 as none and QuotaUnlimited
// as no bound, after migrateUnlimitedQuotas has run.
const settingQuotaSemantics = "quota_semantics"

// errMigrated ends a one-time migration that has already run.
var errMigrated = errors.New("already migrated")

// migrateUnlimitedQuotas moves accounts from the old reading of a quota, where
// 0 meant unlimited, to the new one, where it means none: each 0 becomes
// QuotaUnlimited, so no existing account loses a right it had. It runs once
// per database, which its marker records in the same transaction. Two
// processes migrating at the same moment both try to write it; the second is
// refused by the key and rolls back (on PostgreSQL a failed statement spoils
// the transaction, so it must not commit).
func (s *Store) migrateUnlimitedQuotas() error {
	err := s.db.Transaction(func(tx *gorm.DB) error {
		var n int64
		if err := tx.Model(&models.Setting{}).Where("key = ?", settingQuotaSemantics).Count(&n).Error; err != nil {
			return err
		}
		if n > 0 {
			return errMigrated
		}
		if err := tx.Create(&models.Setting{Key: settingQuotaSemantics, Value: "0-is-none"}).Error; err != nil {
			if errors.Is(err, gorm.ErrDuplicatedKey) {
				return errMigrated
			}
			return err
		}
		for _, col := range []string{"max_servers", "max_memory_mb", "max_cpu_milli"} {
			if err := tx.Model(&models.User{}).Where(col+" = ?", 0).Update(col, models.QuotaUnlimited).Error; err != nil {
				return err
			}
		}
		return nil
	})
	if errors.Is(err, errMigrated) {
		return nil
	}
	return err
}

func (s *Store) autoMigrate() error {
	return s.db.AutoMigrate(
		&models.Template{}, &models.Server{}, &models.User{},
		&models.Session{}, &models.PortAllocation{}, &models.Schedule{},
		&models.BackupConfig{}, &models.Backup{},
		&models.ServerAccess{}, &models.AuditEntry{}, &models.APIKey{},
		&models.AdminRole{}, &models.Cluster{},
		&models.NotificationChannel{}, &models.Event{}, &models.Setting{},
		&models.SSHKey{}, &models.PasswordReset{},
		&models.DatabaseHost{}, &models.ServerDatabase{},
		&models.RateCounter{}, &models.ServerInvite{}, &models.FileUpload{},
		&models.TemplateRevision{}, &models.SFTPCursor{}, &models.EmailConfirmation{},
	)
}

// isConcurrentMigrationError reports whether err is the artifact of two
// processes adding the same column/table at once (SQLite: "duplicate column
// name"; Postgres: "already exists"). Matching is by message since the drivers
// surface different error types.
func isConcurrentMigrationError(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "duplicate column") || strings.Contains(msg, "already exists")
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
	// Save writes every column, and a re-imported egg carries no creation time:
	// without this its template's createdAt became year 1.
	t.CreatedAt = existing.CreatedAt
	err = s.db.Transaction(func(tx *gorm.DB) error {
		// A running server keeps the version it started with until it stops:
		// keep this one while a server is on it.
		if err := keepRevision(tx, &existing); err != nil {
			return err
		}
		if err := tx.Save(t).Error; err != nil {
			return err
		}
		return pruneRevisions(tx, existing.ID)
	})
	if err != nil {
		return nil, err
	}
	return t, nil
}

// keepRevision stores t as a TemplateRevision if a server is on its version.
func keepRevision(tx *gorm.DB, t *models.Template) error {
	var n int64
	if err := tx.Model(&models.Server{}).Where("template_id = ? AND template_version = ?", t.ID, t.Version).
		Count(&n).Error; err != nil || n == 0 {
		return err
	}
	data, err := json.Marshal(t)
	if err != nil {
		return err
	}
	return tx.Clauses(clause.OnConflict{DoNothing: true}).
		Create(&models.TemplateRevision{TemplateID: t.ID, Version: t.Version, Data: string(data)}).Error
}

// pruneRevisions drops the revisions of a template no server is on any more.
func pruneRevisions(tx *gorm.DB, templateID uint) error {
	pinned := tx.Model(&models.Server{}).Select("template_version").Where("template_id = ?", templateID)
	return tx.Where("template_id = ? AND version NOT IN (?)", templateID, pinned).
		Delete(&models.TemplateRevision{}).Error
}

// GetTemplateRevision returns an earlier version of a template, as it was,
// while a server is still on it (ErrNotFound otherwise).
func (s *Store) GetTemplateRevision(templateID uint, version int) (*models.Template, error) {
	var rev models.TemplateRevision
	if err := s.db.Where("template_id = ? AND version = ?", templateID, version).First(&rev).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	var t models.Template
	if err := json.Unmarshal([]byte(rev.Data), &t); err != nil {
		return nil, fmt.Errorf("template %d version %d: %w", templateID, version, err)
	}
	return &t, nil
}

// SetServerTemplateVersion moves a server onto a version of its template, the
// one it starts with from now on, and drops the revisions nobody is on any
// more. A server switched to another template meanwhile is left alone.
func (s *Store) SetServerTemplateVersion(serverID, templateID uint, version int) error {
	return s.db.Transaction(func(tx *gorm.DB) error {
		if err := tx.Model(&models.Server{}).Where("id = ? AND template_id = ?", serverID, templateID).
			Update("template_version", version).Error; err != nil {
			return err
		}
		return pruneRevisions(tx, templateID)
	})
}

// DeleteTemplate removes a template row.
func (s *Store) DeleteTemplate(id uint) error {
	return s.db.Delete(&models.Template{}, id).Error
}

// CountServersByTemplate counts servers created from a template (guards deletion
// and destructive edits).
func (s *Store) CountServersByTemplate(templateID uint) (int64, error) {
	var n int64
	err := s.db.Model(&models.Server{}).Where("template_id = ?", templateID).Count(&n).Error
	return n, err
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
	if err := s.db.Create(srv).Error; err != nil {
		if errors.Is(err, gorm.ErrDuplicatedKey) {
			return ErrDuplicate
		}
		return err
	}
	return nil
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
	return s.SearchServers("")
}

// SearchServers lists every server, optionally narrowed to those whose slug or
// display name contains q (case-insensitive). An empty q lists all of them.
//
// The filter runs in the database rather than the browser: a panel with a few
// hundred servers was shipping the whole list on every dashboard load, and
// filtering what has already been transferred does not help.
func (s *Store) SearchServers(q string) ([]models.Server, error) {
	var srvs []models.Server
	err := serverSearch(s.db, q).Order("created_at asc").Find(&srvs).Error
	return srvs, err
}

// serverSearch applies the name filter to a query. LIKE with lowered operands
// is the portable option: SQLite's LIKE is ASCII-case-insensitive by default and
// Postgres' is not, so neither is relied on.
func serverSearch(db *gorm.DB, q string) *gorm.DB {
	q = strings.TrimSpace(q)
	if q == "" {
		return db.Session(&gorm.Session{})
	}
	// The wildcards are ours; the user's own % and _ are escaped so a search for
	// "100%" does not match everything.
	pattern := "%" + likeEscape(strings.ToLower(q)) + "%"
	return db.Where("LOWER(slug) LIKE ? ESCAPE '\\' OR LOWER(display_name) LIKE ? ESCAPE '\\'", pattern, pattern)
}

// likeEscape neutralizes the LIKE metacharacters in a user's search text.
func likeEscape(s string) string {
	r := strings.NewReplacer(`\`, `\\`, "%", `\%`, "_", `\_`)
	return r.Replace(s)
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

// SetHibernated flips the system hibernation flag (scale-to-zero on idle).
func (s *Store) SetHibernated(id uint, hibernated bool) error {
	return s.db.Model(&models.Server{}).Where("id = ?", id).
		Update("hibernated", hibernated).Error
}

// UpdateLastActive records the last time a server saw activity.
func (s *Store) UpdateLastActive(id uint, when time.Time) error {
	return s.db.Model(&models.Server{}).Where("id = ?", id).
		Update("last_active_at", when).Error
}

// Wake clears hibernation and resets the idle timer (manual wake / start).
func (s *Store) Wake(id uint, when time.Time) error {
	return s.db.Model(&models.Server{}).Where("id = ?", id).
		Updates(map[string]any{"hibernated": false, "last_active_at": when}).Error
}

// StartServer powers a server on: it sets the desired state to Running and, in
// the same write, clears hibernation and rearms the idle timer. Both belong
// together — Replicas() stays 0 while Hibernated is set, so a caller that only
// set the desired state would silently fail to start a hibernated server, and a
// stale LastActiveAt would let the very next hibernation tick put it straight
// back to sleep. Every start path (API power action, scheduled task) goes
// through here so they cannot drift apart again.
//
// A server with a restore waiting or running is not started (ErrRestoreActive).
// A restore waits for the server to be down, then writes over its data, and a
// start under it used to be accepted: the restore waited -- days, if need be --
// and rolled the world back the next time the server stopped. The server's row
// is locked as CreateRestore locks it, so the two cannot both go through. A
// database import holds it down the same way (ErrDatabaseImportActive).
func (s *Store) StartServer(id uint, when time.Time) error {
	return s.db.Transaction(func(tx *gorm.DB) error {
		if _, err := lockServer(tx, id); err != nil {
			return err
		}
		if err := offlineOperationActive(tx, id); err != nil {
			return err
		}
		return tx.Model(&models.Server{}).Where("id = ?", id).
			Updates(map[string]any{
				"desired_state":  string(models.StateRunning),
				"hibernated":     false,
				"last_active_at": when,
			}).Error
	})
}

// lockServer reads a server's row and locks it for the rest of tx (SQLite
// already runs one write transaction at a time).
func lockServer(tx *gorm.DB, id uint) (*models.Server, error) {
	var srv models.Server
	if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&srv, id).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	return &srv, nil
}

// RequestRestart asks for a running server to be stopped -- its stop command
// first, as a stop does -- and started again once its pod is gone; the
// controller carries it out (FinishRestart). It reports false, and changes
// nothing, for a server that is not running.
func (s *Store) RequestRestart(id uint, when time.Time) (bool, error) {
	res := s.db.Model(&models.Server{}).
		Where("id = ? AND desired_state = ? AND hibernated = ?", id, string(models.StateRunning), false).
		Update("restart_requested_at", when)
	return res.RowsAffected == 1, res.Error
}

// FinishRestart ends a restart once the game's pod is gone: the server is
// started again, if it is still meant to run.
func (s *Store) FinishRestart(id uint) error {
	return s.db.Model(&models.Server{}).Where("id = ?", id).
		Update("restart_requested_at", nil).Error
}

// UpdateServerHibernation persists a server's hibernation policy. A non-nil
// rearmAt also restarts the idle timer there, in the same write.
func (s *Store) UpdateServerHibernation(id uint, h models.Hibernation, rearmAt *time.Time) error {
	fields := []any{"hibernation"}
	if rearmAt != nil {
		fields = append(fields, "last_active_at")
	}
	return s.db.Model(&models.Server{}).Where("id = ?", id).
		Select(fields[0], fields[1:]...).Updates(models.Server{Hibernation: h, LastActiveAt: rearmAt}).Error
}

// UpdateServerEnv persists the (re-resolved) plain env and sealed secret env,
// e.g. when a user edits the server's startup variables.
func (s *Store) UpdateServerEnv(id uint, env map[string]string, secretEnc string) error {
	return s.db.Model(&models.Server{}).Where("id = ?", id).
		Select("env", "secret_env_enc").Updates(models.Server{Env: env, SecretEnvEnc: secretEnc}).Error
}

// UpdateServerImage switches the image a server runs.
func (s *Store) UpdateServerImage(id uint, image string) error {
	return s.db.Model(&models.Server{}).Where("id = ?", id).Update("image", image).Error
}

// UpdateServerStartup persists a server's own startup command ("" for its
// template's).
func (s *Store) UpdateServerStartup(id uint, startup string) error {
	return s.db.Model(&models.Server{}).Where("id = ?", id).Update("startup", startup).Error
}

// UpdateServerResources persists only the CPU/memory limits.
func (s *Store) UpdateServerResources(id uint, r models.Resources) error {
	return s.db.Model(&models.Server{}).Where("id = ?", id).
		Select("resources").Updates(models.Server{Resources: r}).Error
}

// CreateServerChecked inserts srv once check approves of the servers its owner
// already has; a nil check is a plain CreateServer. Check and insert share one
// transaction that holds the owner's row, so creations racing for the last
// place in a quota cannot all find it free: five sent at once by an account
// allowed one server used to make two. check's error is returned as is.
func (s *Store) CreateServerChecked(srv *models.Server, check func(owned []models.Server) error) error {
	if check == nil {
		return s.CreateServer(srv)
	}
	err := s.db.Transaction(func(tx *gorm.DB) error {
		owned, err := lockOwnerServers(tx, srv.OwnerID)
		if err != nil {
			return err
		}
		if err := check(owned); err != nil {
			return err
		}
		return tx.Create(srv).Error
	})
	if errors.Is(err, gorm.ErrDuplicatedKey) {
		return ErrDuplicate
	}
	return err
}

// UpdateServerResourcesChecked is UpdateServerResources for a server whose
// owner has a quota: check sees the owner's other servers, in the transaction
// that writes the new limits, for the same reason as CreateServerChecked.
func (s *Store) UpdateServerResourcesChecked(id, ownerID uint, r models.Resources, check func(others []models.Server) error) error {
	return s.db.Transaction(func(tx *gorm.DB) error {
		owned, err := lockOwnerServers(tx, ownerID)
		if err != nil {
			return err
		}
		others := owned[:0]
		for _, o := range owned {
			if o.ID != id {
				others = append(others, o)
			}
		}
		if err := check(others); err != nil {
			return err
		}
		return tx.Model(&models.Server{}).Where("id = ?", id).
			Select("resources").Updates(models.Server{Resources: r}).Error
	})
}

// lockOwnerServers locks a user's row for the rest of tx, which serializes
// every quota decision about them (SQLite already runs one write transaction at
// a time), and returns the servers they own.
func lockOwnerServers(tx *gorm.DB, ownerID uint) ([]models.Server, error) {
	var u models.User
	if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&u, ownerID).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	var owned []models.Server
	if err := tx.Where("owner_id = ?", ownerID).Find(&owned).Error; err != nil {
		return nil, err
	}
	return owned, nil
}

// ClearInstallWipe retires a reinstall's wipe, and what it was to keep, once
// that reinstall has run, so it cannot fire again on a later install of the
// same generation (after a restore rolled the volume back, or a deleted
// install marker).
func (s *Store) ClearInstallWipe(id uint) error {
	// Struct-shaped, with the columns named, so the zero values are written
	// and the list goes through its JSON serializer.
	return s.db.Model(&models.Server{}).Where("id = ?", id).
		Select("install_wipe", "install_keep").Updates(models.Server{}).Error
}

// ServerReinstall is what a reinstall may change on a server besides the install
// generation: the template it runs, the image, and the environment the new
// template resolves to.
type ServerReinstall struct {
	TemplateID      uint
	TemplateVersion int
	Image           string
	Env             map[string]string
	SecretEnvEnc    string
	// Install bumps the install generation, so the (new) template's install
	// script runs at the next start; Wipe has it empty the volume first. A
	// template with no install script has nothing to run, so it leaves both off.
	Install bool
	Wipe    bool
	// Keep spares these paths from the wipe: a clean reinstall. The list is
	// also remembered as the server's ReinstallKeep, for its next one.
	Keep []string
	// DropStartup takes the server back to its template's startup command.
	DropStartup bool
}

// ReinstallServer applies a reinstall in one transaction. The controller must
// never see a new template with the old install generation -- it would start
// the new game on files nobody installed for it -- nor the reverse.
func (s *Store) ReinstallServer(id uint, r ServerReinstall) error {
	return s.db.Transaction(func(tx *gorm.DB) error {
		// Struct-shaped, with the columns named, so Env goes through its JSON
		// serializer and a false InstallWipe is still written.
		wipe := r.Install && r.Wipe
		var keep []string
		if wipe {
			keep = r.Keep
		}
		if err := tx.Model(&models.Server{}).Where("id = ?", id).
			Select("template_id", "template_version", "image", "env", "secret_env_enc", "install_wipe", "install_keep").
			Updates(models.Server{
				TemplateID: r.TemplateID, TemplateVersion: r.TemplateVersion, Image: r.Image,
				Env: r.Env, SecretEnvEnc: r.SecretEnvEnc, InstallWipe: wipe, InstallKeep: keep,
			}).Error; err != nil {
			return err
		}
		if len(keep) > 0 {
			if err := tx.Model(&models.Server{}).Where("id = ?", id).
				Select("reinstall_keep").Updates(models.Server{ReinstallKeep: keep}).Error; err != nil {
				return err
			}
		}
		if r.DropStartup {
			if err := tx.Model(&models.Server{}).Where("id = ?", id).Update("startup", "").Error; err != nil {
				return err
			}
		}
		if !r.Install {
			return nil
		}
		return tx.Model(&models.Server{}).Where("id = ?", id).
			UpdateColumn("install_generation", gorm.Expr("install_generation + 1")).Error
	})
}

// UpdateServerStatus persists only the status field. It uses Updates with a
// typed struct (not Update with a raw value) so GORM applies the JSON
// serializer registered on the Status field.
func (s *Store) UpdateServerStatus(id uint, st models.Status) error {
	return s.db.Model(&models.Server{}).Where("id = ?", id).
		Select("status").Updates(models.Server{Status: st}).Error
}

// UpdateServerName persists only a server's display name.
func (s *Store) UpdateServerName(id uint, name string) error {
	return s.db.Model(&models.Server{}).Where("id = ?", id).
		Select("display_name").Updates(models.Server{DisplayName: name}).Error
}

// UpdateServerNetworking persists only the exposure config and the (re)computed
// port list, leaving controller-written status untouched.
func (s *Store) UpdateServerNetworking(id uint, expose models.Expose, ports []models.PortSpec) error {
	return s.db.Model(&models.Server{}).Where("id = ?", id).
		Select("expose", "ports").
		Updates(models.Server{Expose: expose, Ports: ports}).Error
}

// DeleteServer removes a server record and frees any node ports it held. Its
// open invitations go with it: an ID can be given again to a later server,
// which an old link must not open. So do its unfinished uploads.
func (s *Store) DeleteServer(id uint) error {
	return s.db.Transaction(func(tx *gorm.DB) error {
		if err := tx.Where("server_id = ?", id).Delete(&models.PortAllocation{}).Error; err != nil {
			return err
		}
		if err := tx.Where("server_id = ?", id).Delete(&models.ServerInvite{}).Error; err != nil {
			return err
		}
		// Unfinished uploads went with the volume their pieces were in.
		if err := tx.Where("server_id = ?", id).Delete(&models.FileUpload{}).Error; err != nil {
			return err
		}
		if err := tx.Where("server_id = ?", id).Delete(&models.SFTPCursor{}).Error; err != nil {
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

// AllocateNodePort reserves a free node port in [min,max] for a named server
// port, persisting it so it stays stable across reconciles. The range is
// scanned from a random start, so allocations are scattered rather than
// consecutive. If the (server, name) pair already holds an allocation it is
// returned unchanged.
// A min/max of 0 falls back to the Kubernetes default range.
func (s *Store) AllocateNodePort(serverID uint, name string, min, max int32) (int32, error) {
	var port int32
	err := s.db.Transaction(func(tx *gorm.DB) error {
		var err error
		port, err = allocateNodePort(tx, serverID, name, min, max)
		return err
	})
	if err != nil {
		return 0, err
	}
	return port, nil
}

// nodePortLock is the PostgreSQL advisory lock that keeps changes to the node
// port pool one at a time. SQLite needs none: it runs one write transaction at
// a time. PostgreSQL runs them side by side, and two read the same free port
// and both take it -- the unique index then fails the second, and the
// creation of a server with it.
const nodePortLock = 0x51554e50 // "QUNP"

// lockNodePorts holds nodePortLock until tx ends. Taking it again in the same
// transaction is harmless.
func lockNodePorts(tx *gorm.DB) error {
	if tx.Dialector.Name() != "postgres" {
		return nil
	}
	return tx.Exec("SELECT pg_advisory_xact_lock(?)", nodePortLock).Error
}

func allocateNodePort(tx *gorm.DB, serverID uint, name string, min, max int32) (int32, error) {
	if err := lockNodePorts(tx); err != nil {
		return 0, err
	}
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
	err := tx.Where("server_id = ? AND port_name = ?", serverID, name).First(&alloc).Error
	switch {
	case err == nil:
		return alloc.NodePort, nil // reuse existing allocation
	case !errors.Is(err, gorm.ErrRecordNotFound):
		return 0, err
	}
	used := map[int32]bool{}
	var rows []models.PortAllocation
	if err := tx.Find(&rows).Error; err != nil {
		return 0, err
	}
	for _, r := range rows {
		used[r.NodePort] = true
	}
	// Scan the range from a random start (wrapping) rather than always taking
	// the lowest free port: this scatters allocations so a server's ports can't
	// be guessed from its neighbours', while still finding a free port whenever
	// one exists.
	span := int(max-min) + 1
	start := randRange(span)
	for i := 0; i < span; i++ {
		p := min + int32((start+i)%span)
		if used[p] {
			continue
		}
		alloc = models.PortAllocation{NodePort: p, ServerID: serverID, PortName: name}
		if err := tx.Create(&alloc).Error; err != nil {
			return 0, err
		}
		return p, nil
	}
	return 0, fmt.Errorf("%w in range %d-%d", ErrNoFreeNodePort, min, max)
}

// NodePortKey is the pool key of a server's port: its number, never its name.
// A number exposed on both TCP and UDP then shares one entry (and one node
// port, as Kubernetes allows for such a pair), and adding or removing a
// protocol never renames the entry, so the address players use stays put.
func NodePortKey(port int32) string {
	return fmt.Sprintf("p%d", port)
}

// TakenNodePort is the name of a pool entry set aside because the cluster
// gave the port to something else (see SetAsideNodePort). It belongs to no
// server.
const TakenNodePort = "taken-outside"

// SetAsideNodePort records that the cluster refused node port taken for a
// server, because a Service Quetzal does not manage already holds it, and
// gives the server's port named key another one, which it returns. The pool
// only knows Quetzal's own allocations, while the cluster's range is shared
// with every NodePort and LoadBalancer Service in it: left in the pool, the
// port would be handed out again, to this server or the next. It stays set
// aside, since nothing says when the other Service lets go of it.
//
// A port the pool gave to another of its servers, or to another port of this
// one, is theirs, and is left to them.
func (s *Store) SetAsideNodePort(serverID uint, key string, taken, min, max int32) (int32, error) {
	var port int32
	err := s.db.Transaction(func(tx *gorm.DB) error {
		if err := lockNodePorts(tx); err != nil {
			return err
		}
		var held models.PortAllocation
		err := tx.Where("node_port = ?", taken).First(&held).Error
		switch {
		case errors.Is(err, gorm.ErrRecordNotFound):
			if err := tx.Create(&models.PortAllocation{NodePort: taken, PortName: TakenNodePort}).Error; err != nil {
				return err
			}
		case err != nil:
			return err
		case held.ServerID == serverID && held.PortName == key:
			if err := tx.Model(&held).Updates(map[string]any{"server_id": 0, "port_name": TakenNodePort}).Error; err != nil {
				return err
			}
		}
		port, err = allocateNodePort(tx, serverID, key, min, max)
		return err
	})
	if err != nil {
		return 0, err
	}
	return port, nil
}

// RepointServerNodePort moves a server's ports published on node port from
// to node port to, leaving the rest of its row alone.
func (s *Store) RepointServerNodePort(serverID uint, from, to int32) error {
	return s.db.Transaction(func(tx *gorm.DB) error {
		var srv models.Server
		if err := tx.Select("id", "ports").First(&srv, serverID).Error; err != nil {
			return err
		}
		moved := false
		for i := range srv.Ports {
			if srv.Ports[i].NodePort == from {
				srv.Ports[i].NodePort = to
				moved = true
			}
		}
		if !moved {
			return nil
		}
		return tx.Model(&models.Server{}).Where("id = ?", serverID).
			Select("ports").Updates(models.Server{Ports: srv.Ports}).Error
	})
}

// ReleaseNodePort frees a single named node-port allocation for a server (e.g.
// the SFTP port when SFTP is disabled). A no-op if it isn't held.
func (s *Store) ReleaseNodePort(serverID uint, name string) error {
	return s.db.Where("server_id = ? AND port_name = ?", serverID, name).Delete(&models.PortAllocation{}).Error
}

// RenameNodePort moves a server's allocation from one key to another, keeping
// the reserved port. Used to adopt an entry held under an older key so the
// published port survives a change in how keys are derived. A no-op when the
// old key isn't held or the new one already is.
func (s *Store) RenameNodePort(serverID uint, from, to string) error {
	if from == "" || to == "" || from == to {
		return nil
	}
	return s.db.Transaction(func(tx *gorm.DB) error {
		var existing int64
		if err := tx.Model(&models.PortAllocation{}).
			Where("server_id = ? AND port_name = ?", serverID, to).Count(&existing).Error; err != nil {
			return err
		}
		if existing > 0 {
			return nil // the destination already holds a port; nothing to adopt
		}
		return tx.Model(&models.PortAllocation{}).
			Where("server_id = ? AND port_name = ?", serverID, from).
			Update("port_name", to).Error
	})
}

// ReleaseServerPorts frees every node port held by a server.
func (s *Store) ReleaseServerPorts(serverID uint) error {
	return s.db.Where("server_id = ?", serverID).Delete(&models.PortAllocation{}).Error
}

// randRange returns a uniform random int in [0,n) using crypto/rand (so node
// port placement is unpredictable), falling back to math/rand if the system
// source is unavailable. n <= 1 always yields 0.
func randRange(n int) int {
	if n <= 1 {
		return 0
	}
	if bn, err := crand.Int(crand.Reader, big.NewInt(int64(n))); err == nil {
		return int(bn.Int64())
	}
	return rand.Intn(n)
}

// ---- single-value secret helpers ----

// sealValue encrypts a single secret string for storage ("" stays "").
func (s *Store) sealValue(v string) (string, error) {
	if v == "" {
		return "", nil
	}
	if len(s.key) == 0 {
		return secretPrefixPlain + base64.StdEncoding.EncodeToString([]byte(v)), nil
	}
	ct, err := crypto.Seal(s.key, []byte(v))
	if err != nil {
		return "", err
	}
	return secretPrefixEnc + ct, nil
}

// openValue reverses sealValue.
func (s *Store) openValue(blob string) (string, error) {
	if blob == "" {
		return "", nil
	}
	switch {
	case strings.HasPrefix(blob, secretPrefixEnc):
		if len(s.key) == 0 {
			return "", errors.New("encrypted value present but no key configured")
		}
		pt, err := crypto.Open(s.key, strings.TrimPrefix(blob, secretPrefixEnc))
		return string(pt), err
	case strings.HasPrefix(blob, secretPrefixPlain):
		b, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(blob, secretPrefixPlain))
		return string(b), err
	default:
		return blob, nil
	}
}

// ---- Backup configuration ----

const backupConfigID = 1

// GetBackupConfig returns the single backup configuration row, or ErrNotFound.
func (s *Store) GetBackupConfig() (*models.BackupConfig, error) {
	var c models.BackupConfig
	if err := s.db.First(&c, backupConfigID).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	return &c, nil
}

// SaveBackupConfig upserts the backup configuration. Plaintext secrets are
// encrypted; an empty secret keeps the previously stored value (so the API can
// update non-secret fields without resubmitting credentials).
func (s *Store) SaveBackupConfig(cfg *models.BackupConfig, accessKey, secretKey, repoPassword string) error {
	prev, _ := s.GetBackupConfig()
	cfg.ID = backupConfigID

	set := func(plain, existing string) (string, error) {
		if plain == "" {
			return existing, nil
		}
		return s.sealValue(plain)
	}
	var err error
	var pa, ps, pr string
	if prev != nil {
		pa, ps, pr = prev.AccessKeyEnc, prev.SecretKeyEnc, prev.RepoPasswordEnc
	}
	if cfg.AccessKeyEnc, err = set(accessKey, pa); err != nil {
		return err
	}
	if cfg.SecretKeyEnc, err = set(secretKey, ps); err != nil {
		return err
	}
	if cfg.RepoPasswordEnc, err = set(repoPassword, pr); err != nil {
		return err
	}
	return s.db.Save(cfg).Error
}

// BackupSecrets returns the decrypted credentials for the backup config.
func (s *Store) BackupSecrets(cfg *models.BackupConfig) (accessKey, secretKey, repoPassword string, err error) {
	if accessKey, err = s.openValue(cfg.AccessKeyEnc); err != nil {
		return
	}
	if secretKey, err = s.openValue(cfg.SecretKeyEnc); err != nil {
		return
	}
	repoPassword, err = s.openValue(cfg.RepoPasswordEnc)
	return
}

// ---- Backups ----

// CreateBackup inserts a backup/restore operation record.
func (s *Store) CreateBackup(b *models.Backup) error {
	return s.db.Create(b).Error
}

// CreateRestore queues a restore of a server that is not meant to run
// (ErrServerRunning otherwise). It locks the server's row as StartServer does,
// so a start and a restore sent together cannot both be accepted. A restore
// already waiting or running is ErrRestoreActive: a second one would only
// replace the first, whichever finished last. A database import waiting or
// running is ErrDatabaseImportActive: the restore would load the same
// database under it.
func (s *Store) CreateRestore(b *models.Backup) error {
	return s.createWhileStopped(b)
}

// CreateDatabaseImport queues the load of an SQL file into one of a server's
// databases, under CreateRestore's rules: the server must not be meant to run,
// it cannot be started until the import is done, and only one restore or
// import runs at a time.
func (s *Store) CreateDatabaseImport(b *models.Backup) error {
	return s.createWhileStopped(b)
}

func (s *Store) createWhileStopped(b *models.Backup) error {
	return s.db.Transaction(func(tx *gorm.DB) error {
		srv, err := lockServer(tx, b.ServerID)
		if err != nil {
			return err
		}
		if srv.DesiredState == models.StateRunning {
			return ErrServerRunning
		}
		if err := offlineOperationActive(tx, b.ServerID); err != nil {
			return err
		}
		return tx.Create(b).Error
	})
}

// CancelPendingBackup drops an operation nothing has started: no Job exists
// for it, and the volume is untouched. It reports false when the operation
// left Pending meanwhile (the controller took it up), and drops nothing then.
func (s *Store) CancelPendingBackup(id uint) (bool, error) {
	res := s.db.Where("id = ? AND phase = ?", id, models.BackupPending).Delete(&models.Backup{})
	return res.RowsAffected == 1, res.Error
}

// ClaimBackup moves a pending operation to Running under jobName -- and, for a
// backup, the target it goes to -- before its Job is created. It reports false
// when the operation is no longer pending, cancelled meanwhile: it must not get
// a Job nobody tracks, which for a restore would write over the volume while
// the file manager is back on it.
//
// databases records, for a backup, the databases its Job dumps and, for a
// restore, those it loads back; nil leaves the column alone.
func (s *Store) ClaimBackup(id uint, jobName, target string, databases []string) (bool, error) {
	// Struct-shaped, with the columns named, so Databases goes through its
	// JSON serializer.
	cols := []string{"phase", "job_name"}
	upd := models.Backup{Phase: models.BackupRunning, JobName: jobName}
	if target != "" {
		cols = append(cols, "target")
		upd.Target = target
	}
	if databases != nil {
		cols = append(cols, "databases")
		upd.Databases = databases
	}
	res := s.db.Model(&models.Backup{}).Where("id = ? AND phase = ?", id, models.BackupPending).
		Select(cols).Updates(upd)
	return res.RowsAffected == 1, res.Error
}

// GetBackup returns a backup by ID.
func (s *Store) GetBackup(id uint) (*models.Backup, error) {
	var b models.Backup
	if err := s.db.First(&b, id).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	return &b, nil
}

// ListBackupsForServer returns a server's backups and restores, newest first.
// Its database imports are listed with its databases (LatestDatabaseImports).
func (s *Store) ListBackupsForServer(serverID uint) ([]models.Backup, error) {
	var bs []models.Backup
	if err := s.db.Where("server_id = ? AND direction <> ?", serverID, models.DirDatabaseImport).
		Order("id desc").Find(&bs).Error; err != nil {
		return nil, err
	}
	return bs, nil
}

// LatestDatabaseImports returns, for each of a server's databases that has
// one, its most recent import.
func (s *Store) LatestDatabaseImports(serverID uint) (map[uint]models.Backup, error) {
	var bs []models.Backup
	if err := s.db.Where("server_id = ? AND direction = ?", serverID, models.DirDatabaseImport).
		Order("id asc").Find(&bs).Error; err != nil {
		return nil, err
	}
	out := map[uint]models.Backup{}
	for _, b := range bs {
		out[b.DatabaseID] = b // ascending: the last one wins
	}
	return out, nil
}

// DatabaseImportActive reports whether an import into a database is waiting
// or running.
func (s *Store) DatabaseImportActive(databaseID uint) (bool, error) {
	var n int64
	err := s.db.Model(&models.Backup{}).
		Where("database_id = ? AND direction = ? AND phase IN ?", databaseID, models.DirDatabaseImport,
			[]models.BackupPhase{models.BackupPending, models.BackupRunning}).
		Count(&n).Error
	return n > 0, err
}

// DeleteDatabaseImports drops the import records of a database (when the
// database itself goes).
func (s *Store) DeleteDatabaseImports(databaseID uint) error {
	return s.db.Where("database_id = ? AND direction = ?", databaseID, models.DirDatabaseImport).
		Delete(&models.Backup{}).Error
}

// ListBackupsByPhase returns all operations in a phase (used by the controller).
func (s *Store) ListBackupsByPhase(phase models.BackupPhase) ([]models.Backup, error) {
	var bs []models.Backup
	if err := s.db.Where("phase = ?", phase).Order("id asc").Find(&bs).Error; err != nil {
		return nil, err
	}
	return bs, nil
}

// HasActiveRestore reports whether a restore is pending or running for a server.
// A restore overwrites the data volume in place, so it needs exclusive write
// access; the reconciler scales the data-manager pod down while one is active.
func (s *Store) HasActiveRestore(serverID uint) (bool, error) {
	return hasActiveRestore(s.db, serverID)
}

func hasActiveRestore(tx *gorm.DB, serverID uint) (bool, error) {
	var n int64
	err := tx.Model(&models.Backup{}).
		Where("server_id = ? AND direction = ? AND phase IN ?",
			serverID, models.DirRestore,
			[]models.BackupPhase{models.BackupPending, models.BackupRunning}).
		Count(&n).Error
	return n > 0, err
}

// HasActiveDatabaseImport reports whether an import into one of a server's
// databases is waiting or running.
func (s *Store) HasActiveDatabaseImport(serverID uint) (bool, error) {
	var n int64
	err := s.db.Model(&models.Backup{}).
		Where("server_id = ? AND direction = ? AND phase IN ?",
			serverID, models.DirDatabaseImport,
			[]models.BackupPhase{models.BackupPending, models.BackupRunning}).
		Count(&n).Error
	return n > 0, err
}

// offlineOperationActive refuses what an operation that needs the server down
// forbids while it waits or runs: ErrRestoreActive for a restore,
// ErrDatabaseImportActive for a database import.
func offlineOperationActive(tx *gorm.DB, serverID uint) error {
	var dirs []models.BackupDirection
	if err := tx.Model(&models.Backup{}).
		Where("server_id = ? AND direction IN ? AND phase IN ?",
			serverID, []models.BackupDirection{models.DirRestore, models.DirDatabaseImport},
			[]models.BackupPhase{models.BackupPending, models.BackupRunning}).
		Pluck("direction", &dirs).Error; err != nil {
		return err
	}
	for _, d := range dirs {
		if d == models.DirRestore {
			return ErrRestoreActive
		}
	}
	if len(dirs) > 0 {
		return ErrDatabaseImportActive
	}
	return nil
}

// UpdateBackup persists the mutable fields of an operation.
func (s *Store) UpdateBackup(b *models.Backup) error {
	return s.db.Model(&models.Backup{}).Where("id = ?", b.ID).
		Updates(map[string]any{
			"phase": b.Phase, "size_bytes": b.SizeBytes, "message": b.Message,
			"job_name": b.JobName, "completed_at": b.CompletedAt, "target": b.Target,
		}).Error
}

// StampBackupTargets records target on every succeeded backup that has none,
// before the backup target changes: those were made to the target being left.
func (s *Store) StampBackupTargets(target string) error {
	return s.db.Model(&models.Backup{}).
		Where("target = '' OR target IS NULL").
		Where("direction = ? AND phase = ?", models.DirBackup, models.BackupSucceeded).
		Update("target", target).Error
}

// DeleteBackup removes a backup record.
func (s *Store) DeleteBackup(id uint) error {
	return s.db.Delete(&models.Backup{}, id).Error
}

// MarkBackupDeleting moves a succeeded backup into the Deleting phase, where the
// controller forgets its restic snapshot before the row is finally removed. The
// job name is cleared so the delete gets a fresh Job of its own.
func (s *Store) MarkBackupDeleting(id uint) error {
	return s.db.Model(&models.Backup{}).Where("id = ?", id).
		Updates(map[string]any{"phase": string(models.BackupDeleting), "job_name": "", "message": ""}).Error
}

// DeleteBackupsForServer removes a server's backup records (used on teardown).
func (s *Store) DeleteBackupsForServer(serverID uint) error {
	return s.db.Where("server_id = ?", serverID).Delete(&models.Backup{}).Error
}

// PruneBackups deletes succeeded backup records for a server beyond keepLast
// (newest kept), mirroring restic's retention so the UI history stays in sync.
func (s *Store) PruneBackups(serverID uint, keepLast int) error {
	if keepLast <= 0 {
		return nil
	}
	var old []models.Backup
	err := s.db.Where("server_id = ? AND direction = ? AND phase = ?",
		serverID, models.DirBackup, models.BackupSucceeded).
		Order("id desc").Offset(keepLast).Find(&old).Error
	if err != nil {
		return err
	}
	for i := range old {
		if err := s.db.Delete(&models.Backup{}, old[i].ID).Error; err != nil {
			return err
		}
	}
	return nil
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

// UpdateSchedule persists user-editable fields of a schedule. The struct-based
// Select(...).Updates pattern (not a map) is required so the Tasks JSON
// serializer applies and selected zero values (disabled, cleared next_run,
// empty chain) still persist.
func (s *Store) UpdateSchedule(sc *models.Schedule) error {
	return s.db.Model(&models.Schedule{}).Where("id = ?", sc.ID).
		Select("name", "cron", "timezone", "tasks", "action", "payload", "enabled", "next_run").
		Updates(models.Schedule{
			Name: sc.Name, Cron: sc.Cron, Timezone: sc.Timezone, Tasks: sc.Tasks,
			Action: sc.Action, Payload: sc.Payload, Enabled: sc.Enabled, NextRun: sc.NextRun,
		}).Error
}

// MarkScheduleRun records a schedule's execution outcome and its next due time.
func (s *Store) MarkScheduleRun(id uint, lastRun time.Time, nextRun *time.Time, status string) error {
	return s.db.Model(&models.Schedule{}).Where("id = ?", id).
		Updates(map[string]any{"last_run": lastRun, "next_run": nextRun, "last_status": status}).Error
}

// MarkScheduleResult records only a run's outcome (last_run + last_status),
// leaving next_run untouched. Used when next_run is advanced up front (before an
// async chain runs) so a long chain's completion can't overwrite it with a
// now-stale value.
func (s *Store) MarkScheduleResult(id uint, lastRun time.Time, status string) error {
	return s.db.Model(&models.Schedule{}).Where("id = ?", id).
		Updates(map[string]any{"last_run": lastRun, "last_status": status}).Error
}

// SetScheduleRun records how far a schedule's chain has got; nil ends it.
func (s *Store) SetScheduleRun(id uint, run *models.ScheduleRun) error {
	return s.db.Model(&models.Schedule{}).Where("id = ?", id).
		Select("run").Updates(models.Schedule{Run: run}).Error
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

// CreateUser inserts a new user. A name another account already has, case
// aside, is ErrDuplicate: Lolozini and lolozini are one name to whoever reads
// it, and login finds an account whatever the case typed.
func (s *Store) CreateUser(u *models.User) error {
	return s.db.Transaction(func(tx *gorm.DB) error {
		var n int64
		if err := tx.Model(&models.User{}).Where("lower(username) = lower(?)", u.Username).Count(&n).Error; err != nil {
			return err
		}
		if n > 0 {
			return ErrDuplicate
		}
		return tx.Create(u).Error
	})
}

// ListUsers returns all users (admin view), with admin permissions resolved.
// Roles are loaded once in bulk to avoid a per-user query.
func (s *Store) ListUsers() ([]models.User, error) {
	var us []models.User
	if err := s.db.Order("id asc").Find(&us).Error; err != nil {
		return nil, err
	}
	roles, err := s.ListAdminRoles()
	if err != nil {
		return nil, err
	}
	byID := make(map[uint][]string, len(roles))
	for _, r := range roles {
		byID[r.ID] = r.Permissions
	}
	for i := range us {
		if us[i].IsAdmin {
			us[i].AdminPerms = append([]string(nil), models.AllAdminPermissions...)
		} else if us[i].AdminRoleID != nil {
			us[i].AdminPerms = byID[*us[i].AdminRoleID]
		}
	}
	return us, nil
}

// UpdateUserAdminFields persists admin-editable fields (role + quotas).
func (s *Store) UpdateUserAdminFields(id uint, isAdmin bool, maxServers int, maxMemoryMB, maxCPUMilli int64) error {
	fields := map[string]any{
		"is_admin": isAdmin, "max_servers": maxServers,
		"max_memory_mb": maxMemoryMB, "max_cpu_milli": maxCPUMilli,
	}
	// A superadmin and a scoped role are mutually exclusive: promoting clears
	// any leftover role so it can't silently resurface on a later demotion.
	if isAdmin {
		fields["admin_role_id"] = nil
	}
	return s.db.Model(&models.User{}).Where("id = ?", id).Updates(fields).Error
}

// UpdateUserPassword sets a new password hash.
func (s *Store) UpdateUserPassword(id uint, hash string) error {
	return s.db.Model(&models.User{}).Where("id = ?", id).Update("password_hash", hash).Error
}

// DeleteUser removes a user and their access grants + API keys.
// DeleteUser removes a user and every credential attached to them. Servers they
// own are handed to reassignTo rather than left behind: a deleted owner used to
// leave running workloads pointing at an account that no longer exists, with no
// one to answer for them and the owner-based resource quota silently skipped.
func (s *Store) DeleteUser(id, reassignTo uint) error {
	return s.db.Transaction(func(tx *gorm.DB) error {
		if reassignTo == id {
			return errors.New("store: cannot reassign a deleted user's servers to themselves")
		}
		if err := tx.Model(&models.Server{}).Where("owner_id = ?", id).
			Update("owner_id", reassignTo).Error; err != nil {
			return err
		}
		// The new owner holds every permission by ownership, so a subuser grant
		// they may already have had on one of those servers is now noise — and it
		// would list the owner among their own server's subusers.
		if err := tx.Where("user_id = ? AND server_id IN (?)", reassignTo,
			tx.Model(&models.Server{}).Select("id").Where("owner_id = ?", reassignTo)).
			Delete(&models.ServerAccess{}).Error; err != nil {
			return err
		}
		if err := tx.Where("user_id = ?", id).Delete(&models.ServerAccess{}).Error; err != nil {
			return err
		}
		// Invitations speak for whoever sent them. Once the account is gone,
		// its servers belong to someone else, who never offered that access.
		if err := tx.Where("invited_by = ?", id).Delete(&models.ServerInvite{}).Error; err != nil {
			return err
		}
		if err := tx.Where("user_id = ?", id).Delete(&models.APIKey{}).Error; err != nil {
			return err
		}
		if err := tx.Where("user_id = ?", id).Delete(&models.Session{}).Error; err != nil {
			return err
		}
		// SSH keys are credentials too: a server's authorized_keys is built from
		// its owner and file-permitted users, so a leftover key would keep a
		// deleted account able to log in over SFTP to the servers it owned.
		if err := tx.Where("user_id = ?", id).Delete(&models.SSHKey{}).Error; err != nil {
			return err
		}
		if err := tx.Where("user_id = ?", id).Delete(&models.PasswordReset{}).Error; err != nil {
			return err
		}
		if err := tx.Where("user_id = ?", id).Delete(&models.EmailConfirmation{}).Error; err != nil {
			return err
		}
		return tx.Delete(&models.User{}, id).Error
	})
}

// ConsumeTOTPStep records step as the last one accepted for a user, and reports
// whether it was still unused. A TOTP code is valid across its whole window, so
// accepting one without burning its step lets a code seen once be replayed for
// the rest of that window.
//
// The comparison and the write are a single statement on purpose: two requests
// arriving with the same code would otherwise both read the old high-water mark
// and both be let through.
func (s *Store) ConsumeTOTPStep(userID uint, step uint64) (bool, error) {
	res := s.db.Model(&models.User{}).
		Where("id = ? AND (last_totp_step IS NULL OR last_totp_step < ?)", userID, step).
		Update("last_totp_step", step)
	if res.Error != nil {
		return false, res.Error
	}
	return res.RowsAffected == 1, nil
}

// CountServersOwnedBy returns how many servers a user owns (guards account
// deletion, which hands them to someone else).
func (s *Store) CountServersOwnedBy(userID uint) (int64, error) {
	var n int64
	err := s.db.Model(&models.Server{}).Where("owner_id = ?", userID).Count(&n).Error
	return n, err
}

// CountServersByOwner returns how many servers each account owns.
func (s *Store) CountServersByOwner() (map[uint]int64, error) {
	var rows []struct {
		OwnerID uint
		N       int64
	}
	err := s.db.Model(&models.Server{}).Select("owner_id, count(*) AS n").Group("owner_id").Scan(&rows).Error
	out := make(map[uint]int64, len(rows))
	for _, r := range rows {
		out[r.OwnerID] = r.N
	}
	return out, err
}

// CountAdmins returns the number of admin users (to protect the last admin).
func (s *Store) CountAdmins() (int64, error) {
	var n int64
	err := s.db.Model(&models.User{}).Where("is_admin = ?", true).Count(&n).Error
	return n, err
}

// ListServersByOwner returns servers owned by a user (for quota accounting).
func (s *Store) ListServersByOwner(ownerID uint) ([]models.Server, error) {
	var srvs []models.Server
	if err := s.db.Where("owner_id = ?", ownerID).Find(&srvs).Error; err != nil {
		return nil, err
	}
	return srvs, nil
}

// ListAccessibleServers returns servers a user owns or has been granted access to.
func (s *Store) ListAccessibleServers(userID uint) ([]models.Server, error) {
	return s.SearchAccessibleServers(userID, "")
}

// SearchAccessibleServers is ListAccessibleServers narrowed by the same name
// filter as SearchServers.
func (s *Store) SearchAccessibleServers(userID uint, q string) ([]models.Server, error) {
	var srvs []models.Server
	err := serverSearch(s.db, q).Where("owner_id = ? OR id IN (?)",
		userID, s.db.Model(&models.ServerAccess{}).Select("server_id").Where("user_id = ?", userID),
	).Order("created_at asc").Find(&srvs).Error
	return srvs, err
}

// ---- Server access (subusers) ----

// GrantAccess creates or updates a subuser grant.
func (s *Store) GrantAccess(serverID, userID uint, perms []string) error {
	var existing models.ServerAccess
	err := s.db.Where("server_id = ? AND user_id = ?", serverID, userID).First(&existing).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return s.db.Create(&models.ServerAccess{ServerID: serverID, UserID: userID, Permissions: perms}).Error
	}
	if err != nil {
		return err
	}
	existing.Permissions = perms
	return s.db.Save(&existing).Error
}

// GetServerAccess returns a user's grant on a server, or ErrNotFound.
func (s *Store) GetServerAccess(serverID, userID uint) (*models.ServerAccess, error) {
	var a models.ServerAccess
	if err := s.db.Where("server_id = ? AND user_id = ?", serverID, userID).First(&a).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	return &a, nil
}

// ListAccessForServer returns a server's subuser grants, with usernames filled.
func (s *Store) ListAccessForServer(serverID uint) ([]models.ServerAccess, error) {
	var as []models.ServerAccess
	if err := s.db.Where("server_id = ?", serverID).Order("id asc").Find(&as).Error; err != nil {
		return nil, err
	}
	for i := range as {
		if u, err := s.GetUser(as[i].UserID); err == nil {
			as[i].Username = u.Username
		}
	}
	return as, nil
}

// RevokeAccess removes a subuser grant.
func (s *Store) RevokeAccess(serverID, userID uint) error {
	return s.db.Where("server_id = ? AND user_id = ?", serverID, userID).Delete(&models.ServerAccess{}).Error
}

// DeleteAccessForServer removes all grants on a server (on teardown).
func (s *Store) DeleteAccessForServer(serverID uint) error {
	return s.db.Where("server_id = ?", serverID).Delete(&models.ServerAccess{}).Error
}

// ---- Audit log ----

// AddAudit appends an audit entry (best-effort; never blocks the action).
func (s *Store) AddAudit(e *models.AuditEntry) error {
	return s.db.Create(e).Error
}

// ListAuditForServer returns a page of a server's audit entries, newest first.
// before is a cursor: pass the id of the oldest entry already seen to get the
// page after it, or 0 for the newest page. Without it only the most recent
// entries were ever reachable, which makes an audit log useless for the thing it
// is for — looking up what happened last week.
func (s *Store) ListAuditForServer(serverID, before uint, limit int) ([]models.AuditEntry, error) {
	q := s.db.Where("server_id = ?", serverID)
	if before > 0 {
		q = q.Where("id < ?", before)
	}
	var es []models.AuditEntry
	err := q.Order("id desc").Limit(auditPageSize(limit)).Find(&es).Error
	return es, err
}

// ListAudit returns a page of panel-wide audit entries (admin), newest first.
// See ListAuditForServer for the cursor.
func (s *Store) ListAudit(before uint, limit int) ([]models.AuditEntry, error) {
	q := s.db.Session(&gorm.Session{})
	if before > 0 {
		q = q.Where("id < ?", before)
	}
	var es []models.AuditEntry
	err := q.Order("id desc").Limit(auditPageSize(limit)).Find(&es).Error
	return es, err
}

// auditPageSize clamps a requested page size. The ceiling keeps one request from
// pulling a whole history into memory.
func auditPageSize(limit int) int {
	switch {
	case limit <= 0:
		return 100
	case limit > 500:
		return 500
	}
	return limit
}

// DeleteAuditBefore prunes audit entries older than cutoff. Unlike events this
// is opt-in (see the retention setting): an audit trail is an accountability
// record, and quietly deleting one because a default said so is not a decision
// to make on an operator's behalf.
func (s *Store) DeleteAuditBefore(cutoff time.Time) (int64, error) {
	res := s.db.Where("created_at < ?", cutoff).Delete(&models.AuditEntry{})
	return res.RowsAffected, res.Error
}

// CountAudit returns how many audit entries exist, panel-wide or for one server
// (serverID 0 = panel-wide). The UI shows it so "100 of 4812" reads as a page
// rather than as the whole log.
func (s *Store) CountAudit(serverID uint) (int64, error) {
	q := s.db.Model(&models.AuditEntry{})
	if serverID > 0 {
		q = q.Where("server_id = ?", serverID)
	}
	var n int64
	return n, q.Count(&n).Error
}

// ServerIdentity returns a server's display name and slug (both empty if the
// server no longer exists). Used to label notifications with the friendly name
// rather than the slug.
func (s *Store) ServerIdentity(id uint) (name, slug string, err error) {
	if id == 0 {
		return "", "", nil
	}
	var row struct {
		DisplayName string
		Slug        string
	}
	err = s.db.Model(&models.Server{}).Select("display_name", "slug").Where("id = ?", id).First(&row).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return "", "", nil
	}
	if err != nil {
		return "", "", err
	}
	return row.DisplayName, row.Slug, nil
}

// ServerSlugsByID maps the given server IDs to their slugs (id 0 and deleted
// servers are simply absent). Used to label global audit entries with the
// server they concern without loading whole server rows.
func (s *Store) ServerSlugsByID(ids []uint) (map[uint]string, error) {
	out := map[uint]string{}
	if len(ids) == 0 {
		return out, nil
	}
	var rows []struct {
		ID   uint
		Slug string
	}
	if err := s.db.Model(&models.Server{}).Select("id", "slug").Where("id IN ?", ids).Find(&rows).Error; err != nil {
		return nil, err
	}
	for _, r := range rows {
		out[r.ID] = r.Slug
	}
	return out, nil
}

// ---- API keys ----

// CreateAPIKey stores an API key (hash only).
func (s *Store) CreateAPIKey(k *models.APIKey) error {
	return s.db.Create(k).Error
}

// GetAPIKeyByHash looks up an API key by its token hash.
func (s *Store) GetAPIKeyByHash(hash string) (*models.APIKey, error) {
	var k models.APIKey
	if err := s.db.Where("hash = ?", hash).First(&k).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	return &k, nil
}

// ListAPIKeysForUser returns a user's API keys (without secrets).
func (s *Store) ListAPIKeysForUser(userID uint) ([]models.APIKey, error) {
	var ks []models.APIKey
	err := s.db.Where("user_id = ?", userID).Order("id asc").Find(&ks).Error
	return ks, err
}

// CountAPIKeysByUser returns how many API keys each user holds.
func (s *Store) CountAPIKeysByUser() (map[uint]int, error) {
	var rows []struct {
		UserID uint
		N      int
	}
	if err := s.db.Model(&models.APIKey{}).Select("user_id, count(*) AS n").Group("user_id").Scan(&rows).Error; err != nil {
		return nil, err
	}
	out := make(map[uint]int, len(rows))
	for _, r := range rows {
		out[r.UserID] = r.N
	}
	return out, nil
}

// GetAPIKey returns an API key by ID.
func (s *Store) GetAPIKey(id uint) (*models.APIKey, error) {
	var k models.APIKey
	if err := s.db.First(&k, id).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	return &k, nil
}

// TouchAPIKey records last use (best-effort).
func (s *Store) TouchAPIKey(id uint, when time.Time) error {
	return s.db.Model(&models.APIKey{}).Where("id = ?", id).Update("last_used_at", when).Error
}

// DeleteAPIKey removes an API key.
func (s *Store) DeleteAPIKey(id uint) error {
	return s.db.Delete(&models.APIKey{}, id).Error
}

// GetUser returns a user by ID, with admin permissions resolved.
func (s *Store) GetUser(id uint) (*models.User, error) {
	var u models.User
	if err := s.db.First(&u, id).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	s.resolveAdminPerms(&u)
	return &u, nil
}

// resolveAdminPerms populates u.AdminPerms from the user's superadmin flag or
// their assigned admin role. Best-effort: a missing/deleted role yields no
// permissions rather than an error. Superadmins always get the full set.
func (s *Store) resolveAdminPerms(u *models.User) {
	if u.IsAdmin {
		u.AdminPerms = append([]string(nil), models.AllAdminPermissions...)
		return
	}
	u.AdminPerms = nil
	if u.AdminRoleID == nil {
		return
	}
	var role models.AdminRole
	if err := s.db.First(&role, *u.AdminRoleID).Error; err == nil {
		u.AdminPerms = role.Permissions
	}
}

// GetUserByUsername returns a user by username, with admin permissions
// resolved. The name is matched as typed first, then case aside, which finds
// the account when only one has that name: accounts made before names were
// told apart case aside may share one, and are then found as typed only.
func (s *Store) GetUserByUsername(username string) (*models.User, error) {
	var u models.User
	err := s.db.Where("username = ?", username).First(&u).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		var us []models.User
		if err := s.db.Where("lower(username) = lower(?)", username).Limit(2).Find(&us).Error; err != nil {
			return nil, err
		}
		if len(us) != 1 {
			return nil, ErrNotFound
		}
		u, err = us[0], nil
	}
	if err != nil {
		return nil, err
	}
	s.resolveAdminPerms(&u)
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
