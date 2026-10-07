package store

import (
	"encoding/json"
	"errors"
	"fmt"

	"github.com/lolozini/quetzal/internal/models"
	"gorm.io/gorm"
)

// migratePublicSecrets repairs old CLI-created environments before the API can
// serve them. It is idempotent, also considers a server's pinned revision, and
// never drops unknown public overrides or overwrites an already sealed value.
func (s *Store) migratePublicSecrets() error {
	var batch []models.Server
	return s.db.Select("id").Where("env IS NOT NULL AND env <> ? AND env <> ? AND env <> ?", "{}", "null", "").
		FindInBatches(&batch, 100, func(_ *gorm.DB, _ int) error {
			for _, row := range batch {
				if err := s.migrateServerSecrets(row.ID); err != nil {
					return fmt.Errorf("protect existing secrets for server %d: %w", row.ID, err)
				}
			}
			return nil
		}).Error
}

func (s *Store) migrateServerSecrets(id uint) error {
	return s.db.Transaction(func(tx *gorm.DB) error {
		srv, err := lockServer(tx, id)
		if errors.Is(err, ErrNotFound) {
			return nil
		}
		if err != nil {
			return err
		}
		if srv.TemplateID == 0 || len(srv.Env) == 0 {
			return nil
		}
		var current models.Template
		err = tx.Select("id", "variables").First(&current, srv.TemplateID).Error
		if err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
			return err
		}
		var revision models.TemplateRevision
		err = tx.Where("template_id = ? AND version = ?", srv.TemplateID, srv.TemplateVersion).First(&revision).Error
		if err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
			return err
		}
		var pinned models.Template
		if revision.Data != "" {
			if err := json.Unmarshal([]byte(revision.Data), &pinned); err != nil {
				return err
			}
		}
		var secrets map[string]string
		changed := false
		for _, variables := range [][]models.TemplateVariable{current.Variables, pinned.Variables} {
			for _, variable := range variables {
				value, exists := srv.Env[variable.EnvVariable]
				if !variable.Secret || !exists {
					continue
				}
				if !changed {
					secrets, err = s.OpenSecrets(srv.SecretEnvEnc)
					if err != nil {
						return err
					}
					if secrets == nil {
						secrets = make(map[string]string)
					}
				}
				if _, sealed := secrets[variable.EnvVariable]; !sealed {
					secrets[variable.EnvVariable] = value
				}
				delete(srv.Env, variable.EnvVariable)
				changed = true
			}
		}
		if !changed {
			return nil
		}
		sealed, err := s.SealSecrets(secrets)
		if err != nil {
			return err
		}
		return tx.Model(&models.Server{}).Where("id = ?", id).
			Select("env", "secret_env_enc").Updates(models.Server{Env: srv.Env, SecretEnvEnc: sealed}).Error
	})
}
