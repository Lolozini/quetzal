package store

import (
	crand "crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"gorm.io/gorm"

	"github.com/lolozini/quetzal/internal/models"
)

// ---- Notification channels ----

// CreateChannel stores a channel, encrypting its config map into ConfigEnc.
func (s *Store) CreateChannel(c *models.NotificationChannel, config map[string]string) error {
	enc, err := s.SealSecrets(config)
	if err != nil {
		return err
	}
	c.ConfigEnc = enc
	return s.db.Create(c).Error
}

// UpdateChannel persists a channel's metadata and, when config is non-nil,
// re-encrypts and replaces its settings. A nil config leaves credentials intact.
func (s *Store) UpdateChannel(c *models.NotificationChannel, config map[string]string) error {
	// The update is struct-shaped, with the columns named explicitly so zero
	// values (Enabled=false, an emptied event list) are still written. A
	// map-shaped Updates() would be simpler but bypasses the Events column's
	// encoding, storing a raw Go slice the next read cannot decode.
	cols := []string{"name", "type", "enabled", "server_id", "events"}
	upd := models.NotificationChannel{
		Name:     c.Name,
		Type:     c.Type,
		Enabled:  c.Enabled,
		ServerID: c.ServerID,
		Events:   c.Events,
	}
	if config != nil {
		enc, err := s.SealSecrets(config)
		if err != nil {
			return err
		}
		upd.ConfigEnc = enc
		cols = append(cols, "config_enc")
	}
	return s.db.Model(&models.NotificationChannel{}).Where("id = ?", c.ID).
		Select(cols).Updates(upd).Error
}

// GetChannel returns a channel by ID.
func (s *Store) GetChannel(id uint) (*models.NotificationChannel, error) {
	var c models.NotificationChannel
	if err := s.db.First(&c, id).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	return &c, nil
}

// ChannelConfig returns the decrypted settings map for a channel.
func (s *Store) ChannelConfig(c *models.NotificationChannel) (map[string]string, error) {
	return s.OpenSecrets(c.ConfigEnc)
}

// ListChannels returns every channel (admin/global view).
func (s *Store) ListChannels() ([]models.NotificationChannel, error) {
	var cs []models.NotificationChannel
	if err := s.db.Order("id asc").Find(&cs).Error; err != nil {
		return nil, err
	}
	return cs, nil
}

// ListChannelsForServer returns channels scoped to a server.
func (s *Store) ListChannelsForServer(serverID uint) ([]models.NotificationChannel, error) {
	var cs []models.NotificationChannel
	if err := s.db.Where("server_id = ?", serverID).Order("id asc").Find(&cs).Error; err != nil {
		return nil, err
	}
	return cs, nil
}

// EnabledChannels returns all enabled channels, used by the dispatcher to match
// against an event.
func (s *Store) EnabledChannels() ([]models.NotificationChannel, error) {
	var cs []models.NotificationChannel
	if err := s.db.Where("enabled = ?", true).Find(&cs).Error; err != nil {
		return nil, err
	}
	return cs, nil
}

// DeleteChannel removes a channel.
func (s *Store) DeleteChannel(id uint) error {
	return s.db.Delete(&models.NotificationChannel{}, id).Error
}

// DeleteChannelsForServer removes a server's channels (called on server delete).
func (s *Store) DeleteChannelsForServer(serverID uint) error {
	return s.db.Where("server_id = ?", serverID).Delete(&models.NotificationChannel{}).Error
}

// ---- Events (notification outbox + activity feed) ----

// AddEvent appends an event (best-effort source for notifications; callers must
// not let a failure block the underlying action).
func (s *Store) AddEvent(e *models.Event) error {
	return s.db.Create(e).Error
}

// EventsAfter returns events with ID greater than after, oldest first, capped at
// limit. The dispatcher uses this to drain the outbox in order.
func (s *Store) EventsAfter(after uint, limit int) ([]models.Event, error) {
	var es []models.Event
	err := s.db.Where("id > ?", after).Order("id asc").Limit(limit).Find(&es).Error
	return es, err
}

// LatestEventID returns the highest event ID (0 if none). The dispatcher seeds
// its cursor with this on first start so it never replays historical events.
func (s *Store) LatestEventID() (uint, error) {
	var e models.Event
	err := s.db.Order("id desc").First(&e).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return 0, nil
	}
	return e.ID, err
}

// ListEventsForServer returns recent events for a server, newest first.
func (s *Store) ListEventsForServer(serverID, before uint, limit int) ([]models.Event, error) {
	q := s.db.Where("server_id = ?", serverID)
	if before > 0 {
		q = q.Where("id < ?", before)
	}
	var es []models.Event
	err := q.Order("id desc").Limit(limit).Find(&es).Error
	return es, err
}

// ListEvents returns recent panel-wide events, newest first.
func (s *Store) ListEvents(before uint, limit int) ([]models.Event, error) {
	q := s.db.Session(&gorm.Session{})
	if before > 0 {
		q = q.Where("id < ?", before)
	}
	var es []models.Event
	err := q.Order("id desc").Limit(limit).Find(&es).Error
	return es, err
}

// notifyCursorSetting mirrors notify.CursorSetting. It is duplicated rather than
// imported because the dispatcher depends on the store, not the other way round.
const notifyCursorSetting = "notify.cursor"

// NotifyCursor returns the id of the last event the dispatcher delivered, or 0
// when it has not run yet. Pruning must not go past it.
func (s *Store) NotifyCursor() (uint, error) {
	v, err := s.GetSetting(notifyCursorSetting)
	if err != nil || strings.TrimSpace(v) == "" {
		return 0, err
	}
	n, err := strconv.ParseUint(strings.TrimSpace(v), 10, 0)
	if err != nil {
		return 0, fmt.Errorf("delivery cursor %q is not a number: %w", v, err)
	}
	return uint(n), nil
}

// DeleteEventsBefore prunes delivered events older than cutoff. The event table
// is an outbox the dispatcher reads with a cursor and never emptied, so it only
// ever grew: a power action, a crash, a restart each add a row, for the life of
// the install.
//
// deliveredThrough is the dispatcher's cursor, and nothing at or past it is
// touched — pruning an event that has not gone out yet would drop a notification
// silently, which is worse than the disk it saves.
func (s *Store) DeleteEventsBefore(cutoff time.Time, deliveredThrough uint) (int64, error) {
	if deliveredThrough == 0 {
		return 0, nil // nothing delivered yet; nothing is safe to drop
	}
	res := s.db.Where("created_at < ? AND id <= ?", cutoff, deliveredThrough).Delete(&models.Event{})
	return res.RowsAffected, res.Error
}

// DeleteEventsForServer removes a server's events (called on server delete).
func (s *Store) DeleteEventsForServer(serverID uint) error {
	return s.db.Where("server_id = ?", serverID).Delete(&models.Event{}).Error
}

// ---- Settings (key/value) ----

// EndpointHostFor returns the hostname to publish for servers on a cluster: the
// cluster's own override when set, otherwise the panel-wide setting, otherwise
// "" (the caller falls back to the detected node address). Each cluster fronts
// its own nodes, so a single global name would send players to the wrong one.
func EndpointHostFor(s *Store, clusterID uint) string {
	if clusterID != 0 {
		if c, err := s.GetCluster(clusterID); err == nil {
			if h := strings.TrimSpace(c.EndpointHost); h != "" {
				return h
			}
		}
	}
	h, _ := s.GetSetting(SettingEndpointHost)
	return strings.TrimSpace(h)
}

// SettingEndpointHost is an admin-configured hostname (DNS) published to players
// in a server's external endpoints instead of the raw node IP. When set it is
// used for NodePort game endpoints and the SFTP connection string; when blank
// the detected node address is used.
const SettingEndpointHost = "endpoint_host"

// SettingInstanceID identifies this control plane among any others that share a
// cluster. It is generated once and never changes.
const SettingInstanceID = "instance_id"

// InstanceID returns this control plane's stable identifier, creating it on
// first call. The identity that matters is the database's: "orphan" means "no
// server row in *my* database", so two control planes pointed at one cluster
// each consider the other's namespaces orphaned. Namespaces are labelled with
// this id so an instance only ever reclaims what it owns.
//
// Generation is transactional: a concurrent caller re-reads the row rather than
// racing to overwrite it, so the id can never differ between callers.
func (s *Store) InstanceID() (string, error) {
	var id string
	err := s.db.Transaction(func(tx *gorm.DB) error {
		var v models.Setting
		err := tx.First(&v, "key = ?", SettingInstanceID).Error
		if err == nil && strings.TrimSpace(v.Value) != "" {
			id = strings.TrimSpace(v.Value)
			return nil
		}
		if err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
			return err
		}
		buf := make([]byte, 8)
		if _, err := crand.Read(buf); err != nil {
			return err
		}
		id = hex.EncodeToString(buf)
		return tx.Save(&models.Setting{Key: SettingInstanceID, Value: id}).Error
	})
	if err != nil {
		return "", err
	}
	return id, nil
}

// GetSetting returns a setting value, or "" if absent.
func (s *Store) GetSetting(key string) (string, error) {
	var v models.Setting
	err := s.db.First(&v, "key = ?", key).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return "", nil
	}
	return v.Value, err
}

// SetSetting upserts a setting.
func (s *Store) SetSetting(key, value string) error {
	return s.db.Save(&models.Setting{Key: key, Value: value}).Error
}
