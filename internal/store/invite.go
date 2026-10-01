package store

import (
	"errors"
	"time"

	"gorm.io/gorm"

	"github.com/lolozini/quetzal/internal/models"
)

// SettingInviteSignup says whether an invitation may create the account it is
// accepted from: "off" refuses, anything else (unset included) allows. Turning
// it off leaves invitations to people who already have an account.
const SettingInviteSignup = "invite_signup"

// SetServerInvite stores an invitation, replacing any earlier one to the same
// address for the same server: inviting again sends a fresh link and the old
// one stops working.
func (s *Store) SetServerInvite(inv *models.ServerInvite) error {
	return s.db.Transaction(func(tx *gorm.DB) error {
		if err := tx.Where("server_id = ? AND email = ?", inv.ServerID, inv.Email).
			Delete(&models.ServerInvite{}).Error; err != nil {
			return err
		}
		return tx.Create(inv).Error
	})
}

// ListServerInvites returns a server's invitations still open, oldest first,
// with the inviter's name filled.
func (s *Store) ListServerInvites(serverID uint) ([]models.ServerInvite, error) {
	var out []models.ServerInvite
	err := s.db.Where("server_id = ? AND expires_at > ?", serverID, time.Now()).
		Order("id asc").Find(&out).Error
	if err != nil {
		return nil, err
	}
	for i := range out {
		if u, err := s.GetUser(out[i].InvitedBy); err == nil {
			out[i].InvitedByName = u.Username
		}
	}
	return out, nil
}

// CountServerInvites counts a server's open invitations.
func (s *Store) CountServerInvites(serverID uint) (int64, error) {
	var n int64
	err := s.db.Model(&models.ServerInvite{}).
		Where("server_id = ? AND expires_at > ?", serverID, time.Now()).Count(&n).Error
	return n, err
}

// GetServerInvite returns one of a server's invitations, or ErrNotFound.
func (s *Store) GetServerInvite(serverID, id uint) (*models.ServerInvite, error) {
	var inv models.ServerInvite
	if err := s.db.Where("id = ? AND server_id = ?", id, serverID).First(&inv).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	return &inv, nil
}

// GetServerInviteByHash returns the open invitation a token stands for, or
// ErrNotFound when there is none or it has expired.
func (s *Store) GetServerInviteByHash(hash string) (*models.ServerInvite, error) {
	var inv models.ServerInvite
	err := s.db.Where("token_hash = ? AND expires_at > ?", hash, time.Now()).First(&inv).Error
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	if u, err := s.GetUser(inv.InvitedBy); err == nil {
		inv.InvitedByName = u.Username
	}
	return &inv, nil
}

// DeleteServerInvite withdraws an invitation.
func (s *Store) DeleteServerInvite(id uint) error {
	return s.db.Delete(&models.ServerInvite{}, id).Error
}

// AcceptServerInvite turns an invitation into a grant for userID, and uses it
// up. The two happen together or not at all, and only once: of two requests
// carrying the same link, one finds the invitation already gone.
func (s *Store) AcceptServerInvite(inv *models.ServerInvite, userID uint) error {
	return s.db.Transaction(func(tx *gorm.DB) error {
		return acceptInvite(tx, inv, userID)
	})
}

// RegisterFromInvite creates an account and accepts an invitation with it, in
// one transaction: an invitation already used leaves no account behind.
func (s *Store) RegisterFromInvite(inv *models.ServerInvite, u *models.User) error {
	return s.db.Transaction(func(tx *gorm.DB) error {
		if err := tx.Create(u).Error; err != nil {
			return err
		}
		return acceptInvite(tx, inv, u.ID)
	})
}

func acceptInvite(tx *gorm.DB, inv *models.ServerInvite, userID uint) error {
	res := tx.Where("id = ? AND expires_at > ?", inv.ID, time.Now()).Delete(&models.ServerInvite{})
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected != 1 {
		return ErrNotFound
	}
	var existing models.ServerAccess
	err := tx.Where("server_id = ? AND user_id = ?", inv.ServerID, userID).First(&existing).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return tx.Create(&models.ServerAccess{ServerID: inv.ServerID, UserID: userID, Permissions: inv.Permissions}).Error
	}
	if err != nil {
		return err
	}
	existing.Permissions = inv.Permissions
	return tx.Save(&existing).Error
}

// DeleteExpiredServerInvites drops invitations past their expiry.
func (s *Store) DeleteExpiredServerInvites() (int64, error) {
	res := s.db.Where("expires_at < ?", time.Now()).Delete(&models.ServerInvite{})
	return res.RowsAffected, res.Error
}
