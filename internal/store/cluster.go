package store

import (
	"errors"
	"time"

	"gorm.io/gorm"

	"github.com/lolozini/quetzal/internal/models"
)

// EnsureLocalCluster makes sure a row exists for the control plane's own
// cluster and adopts any legacy servers (ClusterID 0) onto it. It is idempotent
// and safe to call from both the apiserver and controller at startup.
func (s *Store) EnsureLocalCluster() (*models.Cluster, error) {
	var c models.Cluster
	err := s.db.Where("in_cluster = ?", true).First(&c).Error
	switch {
	case errors.Is(err, gorm.ErrRecordNotFound):
		c = models.Cluster{Slug: "local", Name: "local (in-cluster)", InCluster: true, Reachable: true}
		if err := s.db.Create(&c).Error; err != nil {
			return nil, err
		}
	case err != nil:
		return nil, err
	}
	// Adopt pre-multi-cluster servers onto the local cluster so every server has
	// a concrete cluster (grouping by ClusterID then maps 1:1 to a cluster).
	if err := s.db.Model(&models.Server{}).Where("cluster_id = ? OR cluster_id IS NULL", 0).
		Update("cluster_id", c.ID).Error; err != nil {
		return nil, err
	}
	return &c, nil
}

// ListClusters returns all registered clusters.
func (s *Store) ListClusters() ([]models.Cluster, error) {
	var cs []models.Cluster
	if err := s.db.Order("id asc").Find(&cs).Error; err != nil {
		return nil, err
	}
	return cs, nil
}

// GetCluster returns a cluster by ID.
func (s *Store) GetCluster(id uint) (*models.Cluster, error) {
	var c models.Cluster
	if err := s.db.First(&c, id).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	return &c, nil
}

// GetClusterBySlug returns a cluster by slug.
func (s *Store) GetClusterBySlug(slug string) (*models.Cluster, error) {
	var c models.Cluster
	if err := s.db.Where("slug = ?", slug).First(&c).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	return &c, nil
}

// CreateCluster stores a remote cluster, encrypting its kubeconfig.
func (s *Store) CreateCluster(c *models.Cluster, kubeconfig string) error {
	enc, err := s.sealValue(kubeconfig)
	if err != nil {
		return err
	}
	c.KubeconfigEnc = enc
	return s.db.Create(c).Error
}

// UpdateCluster persists a cluster's name and, when a non-empty kubeconfig is
// given, re-encrypts and replaces its credentials.
func (s *Store) UpdateCluster(id uint, name, kubeconfig string) error {
	fields := map[string]any{"name": name}
	if kubeconfig != "" {
		enc, err := s.sealValue(kubeconfig)
		if err != nil {
			return err
		}
		fields["kubeconfig_enc"] = enc
	}
	return s.db.Model(&models.Cluster{}).Where("id = ?", id).Updates(fields).Error
}

// SetClusterStatus records a connectivity probe result.
func (s *Store) SetClusterStatus(id uint, reachable bool, version string, nodes int, msg string) error {
	now := time.Now()
	return s.db.Model(&models.Cluster{}).Where("id = ?", id).
		Updates(map[string]any{
			"reachable": reachable, "version": version, "node_count": nodes,
			"status_message": msg, "last_checked_at": now,
		}).Error
}

// ClusterKubeconfig returns the decrypted kubeconfig for a cluster ("" for an
// in-cluster cluster).
func (s *Store) ClusterKubeconfig(c *models.Cluster) (string, error) {
	return s.openValue(c.KubeconfigEnc)
}

// DeleteCluster removes a cluster row (callers must ensure it holds no servers
// and is not the local cluster).
func (s *Store) DeleteCluster(id uint) error {
	return s.db.Delete(&models.Cluster{}, id).Error
}

// CountServersByCluster returns how many servers target a cluster.
func (s *Store) CountServersByCluster(id uint) (int64, error) {
	var n int64
	err := s.db.Model(&models.Server{}).Where("cluster_id = ?", id).Count(&n).Error
	return n, err
}
