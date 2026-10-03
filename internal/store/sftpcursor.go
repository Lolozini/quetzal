package store

import (
	"errors"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"github.com/lolozini/quetzal/internal/models"
)

// SFTPCursor returns how far a server's SFTP log has been read: the zero time
// when it never has.
func (s *Store) SFTPCursor(serverID uint) (time.Time, error) {
	var c models.SFTPCursor
	err := s.db.First(&c, "server_id = ?", serverID).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return time.Time{}, nil
	}
	return c.Seen, err
}

// SetSFTPCursor records how far a server's SFTP log has been read.
func (s *Store) SetSFTPCursor(serverID uint, seen time.Time) error {
	return s.db.Clauses(clause.OnConflict{
		Columns:   []clause.Column{{Name: "server_id"}},
		DoUpdates: clause.AssignmentColumns([]string{"seen"}),
	}).Create(&models.SFTPCursor{ServerID: serverID, Seen: seen}).Error
}
