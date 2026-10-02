package store

import (
	"errors"
	"time"

	"gorm.io/gorm"

	"github.com/lolozini/quetzal/internal/models"
)

// CreateUpload records an upload that is starting.
func (s *Store) CreateUpload(u *models.FileUpload) error {
	return s.db.Create(u).Error
}

// GetUpload returns one of a user's uploads to a server, or ErrNotFound. An
// upload is reachable only by the server and the account it was started with.
func (s *Store) GetUpload(id string, serverID, userID uint) (*models.FileUpload, error) {
	var u models.FileUpload
	err := s.db.Where("id = ? AND server_id = ? AND user_id = ?", id, serverID, userID).First(&u).Error
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	return &u, nil
}

// ListUploads returns a user's unfinished uploads to a server, oldest first.
func (s *Store) ListUploads(serverID, userID uint) ([]models.FileUpload, error) {
	var out []models.FileUpload
	err := s.db.Where("server_id = ? AND user_id = ? AND expires_at > ?", serverID, userID, time.Now()).
		Order("created_at asc").Find(&out).Error
	return out, err
}

// CountUploadsByUser counts a user's unfinished uploads, across servers.
func (s *Store) CountUploadsByUser(userID uint) (int64, error) {
	var n int64
	err := s.db.Model(&models.FileUpload{}).Where("user_id = ? AND expires_at > ?", userID, time.Now()).Count(&n).Error
	return n, err
}

// LockUpload claims an upload for one request until the given time, and
// reports whether it got it. It is a single conditional update, so two
// requests — on one process or two — cannot both get it.
func (s *Store) LockUpload(id string, until time.Time) (bool, error) {
	res := s.db.Model(&models.FileUpload{}).
		Where("id = ? AND locked_until < ?", id, time.Now()).
		Update("locked_until", until)
	return res.RowsAffected == 1, res.Error
}

// TouchUpload releases an upload's claim and moves its expiry forward.
func (s *Store) TouchUpload(id string, expires time.Time) error {
	return s.db.Model(&models.FileUpload{}).Where("id = ?", id).
		Updates(map[string]any{"locked_until": time.Time{}, "expires_at": expires}).Error
}

// DeleteUpload forgets an upload.
func (s *Store) DeleteUpload(id string) error {
	return s.db.Where("id = ?", id).Delete(&models.FileUpload{}).Error
}

// ExpiredUploads lists the uploads left alone past their expiry, for
// collection.
func (s *Store) ExpiredUploads() ([]models.FileUpload, error) {
	var out []models.FileUpload
	err := s.db.Where("expires_at < ?", time.Now()).Find(&out).Error
	return out, err
}
