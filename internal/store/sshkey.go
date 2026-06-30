package store

import (
	"errors"

	"gorm.io/gorm"

	"github.com/lolozini/quetzal/internal/models"
)

// AddSSHKey stores a user's SSH public key.
func (s *Store) AddSSHKey(k *models.SSHKey) error {
	return s.db.Create(k).Error
}

// ListSSHKeysForUser returns a user's keys.
func (s *Store) ListSSHKeysForUser(userID uint) ([]models.SSHKey, error) {
	var ks []models.SSHKey
	err := s.db.Where("user_id = ?", userID).Order("id asc").Find(&ks).Error
	return ks, err
}

// GetSSHKey returns a key by ID.
func (s *Store) GetSSHKey(id uint) (*models.SSHKey, error) {
	var k models.SSHKey
	if err := s.db.First(&k, id).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	return &k, nil
}

// DeleteSSHKey removes a key.
func (s *Store) DeleteSSHKey(id uint) error {
	return s.db.Delete(&models.SSHKey{}, id).Error
}

// ListAuthorizedSSHKeys returns the SSH keys allowed to access a server's files
// over SFTP: the owner's, all admins', and any subuser granted the files
// permission. Used by the controller to build the authorized_keys file.
func (s *Store) ListAuthorizedSSHKeys(serverID uint) ([]models.SSHKey, error) {
	srv, err := s.GetServer(serverID)
	if err != nil {
		return nil, err
	}
	userIDs := map[uint]bool{srv.OwnerID: true}

	var admins []models.User
	if err := s.db.Where("is_admin = ?", true).Find(&admins).Error; err != nil {
		return nil, err
	}
	for _, a := range admins {
		userIDs[a.ID] = true
	}

	accesses, err := s.ListAccessForServer(serverID)
	if err != nil {
		return nil, err
	}
	for i := range accesses {
		if accesses[i].Has(models.PermFiles) {
			userIDs[accesses[i].UserID] = true
		}
	}

	ids := make([]uint, 0, len(userIDs))
	for id := range userIDs {
		ids = append(ids, id)
	}
	var keys []models.SSHKey
	if err := s.db.Where("user_id IN ?", ids).Find(&keys).Error; err != nil {
		return nil, err
	}
	return keys, nil
}

// UpdateServerSFTP persists a server's SFTP configuration.
func (s *Store) UpdateServerSFTP(id uint, cfg models.SFTPConfig) error {
	return s.db.Model(&models.Server{ID: id}).Select("sftp").
		Updates(models.Server{SFTP: cfg}).Error
}

// UpdateServerEULA persists a server's Minecraft EULA acceptance. The column is
// selected explicitly so a false value is written (GORM skips zero-value struct
// fields otherwise).
func (s *Store) UpdateServerEULA(id uint, accepted bool) error {
	return s.db.Model(&models.Server{ID: id}).Select("eula_accepted").
		Updates(models.Server{EULAAccepted: accepted}).Error
}
