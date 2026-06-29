// Package store is the database layer. The database is Quetzal's source of
// truth; the controller reconciles its contents into Kubernetes objects.
package store

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"math/rand"
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
//
// The apiserver and controller each run Migrate at startup against the same
// database, so a schema change (a new column) can race: both AutoMigrate calls
// see the column missing and both issue ALTER TABLE ADD COLUMN, and the loser
// fails with "duplicate column"/"already exists". That's benign — the schema is
// correct either way — so retry on exactly that error: once the other process
// finishes, AutoMigrate is a no-op and succeeds.
func (s *Store) Migrate() error {
	var err error
	for attempt := 0; attempt < 5; attempt++ {
		if err = s.autoMigrate(); err == nil || !isConcurrentMigrationError(err) {
			return err
		}
		// Back off with a little jitter so the two processes don't lock-step.
		time.Sleep(time.Duration(100*(attempt+1))*time.Millisecond + time.Duration(rand.Intn(50))*time.Millisecond)
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
	if err := s.db.Save(t).Error; err != nil {
		return nil, err
	}
	return t, nil
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

// UpdateServerHibernation persists a server's hibernation policy.
func (s *Store) UpdateServerHibernation(id uint, h models.Hibernation) error {
	return s.db.Model(&models.Server{}).Where("id = ?", id).
		Select("hibernation").Updates(models.Server{Hibernation: h}).Error
}

// UpdateServerEnv persists the (re-resolved) plain env and sealed secret env,
// e.g. when a user edits the server's startup variables.
func (s *Store) UpdateServerEnv(id uint, env map[string]string, secretEnc string) error {
	return s.db.Model(&models.Server{}).Where("id = ?", id).
		Select("env", "secret_env_enc").Updates(models.Server{Env: env, SecretEnvEnc: secretEnc}).Error
}

// UpdateServerResources persists only the CPU/memory limits.
func (s *Store) UpdateServerResources(id uint, r models.Resources) error {
	return s.db.Model(&models.Server{}).Where("id = ?", id).
		Select("resources").Updates(models.Server{Resources: r}).Error
}

// BumpInstallGeneration increments a server's install generation (triggering a
// reinstall on the next start/reconcile) and sets the one-shot wipe flag. The
// increment is done in SQL so concurrent reinstalls don't lose a bump.
func (s *Store) BumpInstallGeneration(id uint, wipe bool) error {
	return s.db.Model(&models.Server{}).Where("id = ?", id).
		Updates(map[string]any{
			"install_generation": gorm.Expr("install_generation + 1"),
			"install_wipe":       wipe,
		}).Error
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

// ReleaseNodePort frees a single named node-port allocation for a server (e.g.
// the SFTP port when SFTP is disabled). A no-op if it isn't held.
func (s *Store) ReleaseNodePort(serverID uint, name string) error {
	return s.db.Where("server_id = ? AND port_name = ?", serverID, name).Delete(&models.PortAllocation{}).Error
}

// ReleaseServerPorts frees every node port held by a server.
func (s *Store) ReleaseServerPorts(serverID uint) error {
	return s.db.Where("server_id = ?", serverID).Delete(&models.PortAllocation{}).Error
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

// ListBackupsForServer returns a server's operations, newest first.
func (s *Store) ListBackupsForServer(serverID uint) ([]models.Backup, error) {
	var bs []models.Backup
	if err := s.db.Where("server_id = ?", serverID).Order("id desc").Find(&bs).Error; err != nil {
		return nil, err
	}
	return bs, nil
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
	var n int64
	err := s.db.Model(&models.Backup{}).
		Where("server_id = ? AND direction = ? AND phase IN ?",
			serverID, models.DirRestore,
			[]models.BackupPhase{models.BackupPending, models.BackupRunning}).
		Count(&n).Error
	return n > 0, err
}

// UpdateBackup persists the mutable fields of an operation.
func (s *Store) UpdateBackup(b *models.Backup) error {
	return s.db.Model(&models.Backup{}).Where("id = ?", b.ID).
		Updates(map[string]any{
			"phase": b.Phase, "size_bytes": b.SizeBytes, "message": b.Message,
			"job_name": b.JobName, "completed_at": b.CompletedAt,
		}).Error
}

// DeleteBackup removes a backup record.
func (s *Store) DeleteBackup(id uint) error {
	return s.db.Delete(&models.Backup{}, id).Error
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
		Select("name", "cron", "tasks", "action", "payload", "enabled", "next_run").
		Updates(models.Schedule{
			Name: sc.Name, Cron: sc.Cron, Tasks: sc.Tasks, Action: sc.Action,
			Payload: sc.Payload, Enabled: sc.Enabled, NextRun: sc.NextRun,
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
func (s *Store) DeleteUser(id uint) error {
	return s.db.Transaction(func(tx *gorm.DB) error {
		if err := tx.Where("user_id = ?", id).Delete(&models.ServerAccess{}).Error; err != nil {
			return err
		}
		if err := tx.Where("user_id = ?", id).Delete(&models.APIKey{}).Error; err != nil {
			return err
		}
		if err := tx.Where("user_id = ?", id).Delete(&models.Session{}).Error; err != nil {
			return err
		}
		return tx.Delete(&models.User{}, id).Error
	})
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
	var srvs []models.Server
	err := s.db.Where("owner_id = ? OR id IN (?)",
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

// ListAuditForServer returns recent audit entries for a server, newest first.
func (s *Store) ListAuditForServer(serverID uint, limit int) ([]models.AuditEntry, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	var es []models.AuditEntry
	err := s.db.Where("server_id = ?", serverID).Order("id desc").Limit(limit).Find(&es).Error
	return es, err
}

// ListAudit returns recent panel-wide audit entries (admin), newest first.
func (s *Store) ListAudit(limit int) ([]models.AuditEntry, error) {
	if limit <= 0 || limit > 500 {
		limit = 200
	}
	var es []models.AuditEntry
	err := s.db.Order("id desc").Limit(limit).Find(&es).Error
	return es, err
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

// GetUserByUsername returns a user by username, with admin permissions resolved.
func (s *Store) GetUserByUsername(username string) (*models.User, error) {
	var u models.User
	if err := s.db.Where("username = ?", username).First(&u).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, ErrNotFound
		}
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
