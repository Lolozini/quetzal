package store

import "github.com/lolozini/quetzal/internal/models"

// SetServerTransfer sets (or clears, when t is nil) a server's transfer state.
// The Select(...).Updates(struct) form is required so the serializer applies and
// a nil pointer persists as NULL (rather than being skipped as a zero value).
func (s *Store) SetServerTransfer(id uint, t *models.TransferState) error {
	return s.db.Model(&models.Server{}).Where("id = ?", id).
		Select("transfer").Updates(models.Server{Transfer: t}).Error
}

// SetServerCluster flips a server's target cluster (used mid-transfer). It
// updates only the column, leaving controller-written status untouched.
func (s *Store) SetServerCluster(id, clusterID uint) error {
	return s.db.Model(&models.Server{}).Where("id = ?", id).
		Update("cluster_id", clusterID).Error
}

// ListServersWithTransfer returns servers with an in-progress transfer.
func (s *Store) ListServersWithTransfer() ([]models.Server, error) {
	var srvs []models.Server
	if err := s.db.
		Where("transfer IS NOT NULL AND transfer != '' AND transfer != 'null'").
		Find(&srvs).Error; err != nil {
		return nil, err
	}
	return srvs, nil
}
