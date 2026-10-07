package store

import (
	"errors"
	"time"

	"gorm.io/gorm"

	"github.com/lolozini/quetzal/internal/models"
)

var ErrTransferActive = errors.New("a cluster transfer is in progress")
var ErrTransferChanged = errors.New("transfer state changed")
var ErrTransferOperationActive = errors.New("a backup operation is still active")

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

// BeginServerTransfer freezes power and records the source in the same
// transaction used by starts and offline operations.
func (s *Store) BeginServerTransfer(id, target uint, when time.Time) (*models.TransferState, error) {
	var transfer *models.TransferState
	err := s.db.Transaction(func(tx *gorm.DB) error {
		srv, err := lockServer(tx, id)
		if err != nil {
			return err
		}
		if srv.Transfer != nil {
			return ErrTransferActive
		}
		if srv.ClusterID == target {
			return ErrTransferChanged
		}
		var active int64
		if err := tx.Model(&models.Backup{}).Where("server_id = ? AND phase IN ?", id,
			[]models.BackupPhase{models.BackupPending, models.BackupRunning, models.BackupDeleting}).Count(&active).Error; err != nil {
			return err
		}
		if active != 0 {
			return ErrTransferOperationActive
		}
		transfer = &models.TransferState{Phase: models.TransferBackingUp, SourceCluster: srv.ClusterID,
			TargetCluster: target, PrevState: srv.DesiredState, StartedAt: when}
		return tx.Model(&models.Server{}).Where("id = ?", id).
			Select("transfer", "desired_state", "restart_requested_at").
			Updates(models.Server{Transfer: transfer, DesiredState: models.StateStopped}).Error
	})
	return transfer, err
}

// CreateTransferOperation publishes the operation and its link together. A
// cancellation cannot leave an untracked Pending restore behind.
func (s *Store) CreateTransferOperation(id uint, direction models.BackupDirection) error {
	return s.db.Transaction(func(tx *gorm.DB) error {
		srv, err := lockServer(tx, id)
		if err != nil {
			return err
		}
		t := srv.Transfer
		if t == nil || t.Cancelled {
			return ErrTransferChanged
		}
		b := &models.Backup{ServerID: id, Direction: direction, Phase: models.BackupPending}
		clusterID := t.SourceCluster
		switch direction {
		case models.DirBackup:
			if t.Phase != models.TransferBackingUp || t.BackupID != 0 {
				return ErrTransferChanged
			}
			b.Full = true
		case models.DirRestore:
			if t.Phase != models.TransferRestoring || t.RestoreID != 0 {
				return ErrTransferChanged
			}
			b.SourceID = t.BackupID
			clusterID = t.TargetCluster
		default:
			return ErrTransferChanged
		}
		b.ClusterID = &clusterID
		if err := tx.Create(b).Error; err != nil {
			return err
		}
		if direction == models.DirBackup {
			t.BackupID = b.ID
		} else {
			t.RestoreID = b.ID
		}
		return tx.Model(&models.Server{}).Where("id = ?", id).Select("transfer").
			Updates(models.Server{Transfer: t}).Error
	})
}

// AdvanceServerTransfer atomically flips placement and phase after the snapshot.
func (s *Store) AdvanceServerTransfer(id uint) error {
	return s.db.Transaction(func(tx *gorm.DB) error {
		srv, err := lockServer(tx, id)
		if err != nil {
			return err
		}
		t := srv.Transfer
		if t == nil || t.Cancelled || t.Phase != models.TransferBackingUp {
			return ErrTransferChanged
		}
		var b models.Backup
		if err := tx.First(&b, t.BackupID).Error; err != nil {
			return err
		}
		if b.Phase != models.BackupSucceeded {
			return ErrTransferChanged
		}
		// Retention can enqueue forget Jobs when this snapshot completes.
		// Let them finish on the source before changing namespace placement.
		var active int64
		if err := tx.Model(&models.Backup{}).Where("server_id = ? AND id <> ? AND phase IN ?", id, t.BackupID,
			[]models.BackupPhase{models.BackupPending, models.BackupRunning, models.BackupDeleting}).Count(&active).Error; err != nil {
			return err
		}
		if active != 0 {
			return ErrTransferOperationActive
		}
		t.Phase = models.TransferRestoring
		return tx.Model(&models.Server{}).Where("id = ?", id).Select("transfer", "cluster_id").
			Updates(models.Server{Transfer: t, ClusterID: t.TargetCluster}).Error
	})
}

// CancelServerTransfer blocks claims and terminates Pending linked operations
// under the same server lock as ClaimBackup. Running jobs remain active until
// the controller has observed their pods gone.
func (s *Store) CancelServerTransfer(id uint, message string) (*models.Server, error) {
	var srv *models.Server
	err := s.db.Transaction(func(tx *gorm.DB) error {
		var err error
		srv, err = lockServer(tx, id)
		if err != nil {
			return err
		}
		t := srv.Transfer
		if t == nil || t.Phase == models.TransferCommitting {
			return ErrTransferChanged
		}
		t.Cancelled, t.Message = true, message
		if err := tx.Model(&models.Backup{}).
			Where("server_id = ? AND id IN ? AND phase = ?", id, []uint{t.BackupID, t.RestoreID}, models.BackupPending).
			Updates(map[string]any{"phase": models.BackupFailed, "message": message, "completed_at": time.Now()}).Error; err != nil {
			return err
		}
		return tx.Model(&models.Server{}).Where("id = ?", id).Select("transfer").
			Updates(models.Server{Transfer: t}).Error
	})
	return srv, err
}

// FinishCancelledTransferOperation is called only after the job and its pods
// have gone. It cannot modify an operation outside this cancelled transfer.
func (s *Store) FinishCancelledTransferOperation(serverID, operationID uint) error {
	return s.db.Transaction(func(tx *gorm.DB) error {
		srv, err := lockServer(tx, serverID)
		if err != nil {
			return err
		}
		t := srv.Transfer
		if t == nil || !t.Cancelled || (t.BackupID != operationID && t.RestoreID != operationID) {
			return ErrTransferChanged
		}
		return tx.Model(&models.Backup{}).Where("id = ? AND server_id = ? AND phase = ?", operationID, serverID, models.BackupRunning).
			Updates(map[string]any{"phase": models.BackupFailed, "message": t.Message, "completed_at": time.Now()}).Error
	})
}

// FinishServerTransfer releases the freeze only once no linked operation can
// still claim or write a volume. Placement, power and transfer clear together.
func (s *Store) FinishServerTransfer(id uint, rollback bool) error {
	return s.db.Transaction(func(tx *gorm.DB) error {
		srv, err := lockServer(tx, id)
		if err != nil {
			return err
		}
		t := srv.Transfer
		if t == nil || t.Cancelled != rollback {
			return ErrTransferChanged
		}
		var active int64
		if err := tx.Model(&models.Backup{}).Where("server_id = ? AND id IN ? AND phase IN ?", id,
			[]uint{t.BackupID, t.RestoreID}, []models.BackupPhase{models.BackupPending, models.BackupRunning}).Count(&active).Error; err != nil {
			return err
		}
		if active != 0 {
			return ErrTransferOperationActive
		}
		clusterID := t.SourceCluster
		if !rollback {
			if t.Phase != models.TransferCommitting {
				return ErrTransferChanged
			}
			var restore models.Backup
			if t.RestoreID == 0 {
				return ErrTransferChanged
			}
			if err := tx.First(&restore, t.RestoreID).Error; err != nil {
				return err
			}
			if restore.Phase != models.BackupSucceeded {
				return ErrTransferChanged
			}
			clusterID = t.TargetCluster
		}
		return tx.Model(&models.Server{}).Where("id = ?", id).
			Select("transfer", "cluster_id", "desired_state").
			Updates(models.Server{ClusterID: clusterID, DesiredState: t.PrevState}).Error
	})
}

// CommitServerTransfer closes cancellation before the first destructive source
// cleanup request. A failed cleanup remains retryable, never rollbackable.
func (s *Store) CommitServerTransfer(id uint) error {
	return s.db.Transaction(func(tx *gorm.DB) error {
		srv, err := lockServer(tx, id)
		if err != nil {
			return err
		}
		t := srv.Transfer
		if t == nil || t.Cancelled || t.Phase != models.TransferRestoring || t.RestoreID == 0 {
			return ErrTransferChanged
		}
		var restore models.Backup
		if err := tx.First(&restore, t.RestoreID).Error; err != nil {
			return err
		}
		if restore.Phase != models.BackupSucceeded {
			return ErrTransferChanged
		}
		t.Phase = models.TransferCommitting
		return tx.Model(&models.Server{}).Where("id = ?", id).Select("transfer").
			Updates(models.Server{Transfer: t}).Error
	})
}
