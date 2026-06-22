package store

import (
	"errors"

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
	fields := map[string]any{
		"name":      c.Name,
		"type":      c.Type,
		"enabled":   c.Enabled,
		"server_id": c.ServerID,
		"events":    c.Events,
	}
	if config != nil {
		enc, err := s.SealSecrets(config)
		if err != nil {
			return err
		}
		fields["config_enc"] = enc
	}
	return s.db.Model(&models.NotificationChannel{}).Where("id = ?", c.ID).
		Updates(fields).Error
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
func (s *Store) ListEventsForServer(serverID uint, limit int) ([]models.Event, error) {
	var es []models.Event
	err := s.db.Where("server_id = ?", serverID).Order("id desc").Limit(limit).Find(&es).Error
	return es, err
}

// ListEvents returns recent panel-wide events, newest first.
func (s *Store) ListEvents(limit int) ([]models.Event, error) {
	var es []models.Event
	err := s.db.Order("id desc").Limit(limit).Find(&es).Error
	return es, err
}

// DeleteEventsForServer removes a server's events (called on server delete).
func (s *Store) DeleteEventsForServer(serverID uint) error {
	return s.db.Where("server_id = ?", serverID).Delete(&models.Event{}).Error
}

// ---- Settings (key/value) ----

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
