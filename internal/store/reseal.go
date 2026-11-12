package store

import (
	"encoding/base64"
	"log"
	"strings"

	"github.com/lolozini/quetzal/internal/crypto"
	"github.com/lolozini/quetzal/internal/models"
)

// sealedColumn is a column that holds a value written by sealValue or SealSecrets.
type sealedColumn struct {
	model  any
	column string
}

// sealedColumns are the columns a secret is written to. Every one of them is
// written in clear text, behind a "plain:" prefix, while the instance runs without
// a key (see sealValue), and has to be sealed once it has one.
var sealedColumns = []sealedColumn{
	{&models.BackupConfig{}, "access_key_enc"},
	{&models.BackupConfig{}, "secret_key_enc"},
	{&models.BackupConfig{}, "repo_password_enc"},
	{&models.User{}, "totp_secret_enc"},
	{&models.DatabaseHost{}, "admin_password_enc"},
	{&models.ServerDatabase{}, "password_enc"},
	{&models.Cluster{}, "kubeconfig_enc"},
	{&models.NotificationChannel{}, "config_enc"},
	{&models.Server{}, "secret_env_enc"},
}

// resealPlainSecrets seals what was stored in clear text while the instance ran
// without a usable key, and reports how many values it sealed. Until 0.19.0 a key
// given as 64 hexadecimal characters was taken for no key at all, so an instance
// that was configured with one wrote everything in clear text; the key is accepted
// now, and what was written before stayed readable by anyone with the database or
// a backup of it, unmarked.
//
// It does nothing without a key, only touches a value that still starts with the
// "plain:" prefix, replaces it only if it is still the value it read, and a value
// it cannot seal is logged and left as it is: the instance keeps reading it.
func (s *Store) resealPlainSecrets() int {
	if len(s.key) == 0 {
		return 0
	}
	sealed := 0
	like := secretPrefixPlain + "%"
	for _, c := range sealedColumns {
		var rows []struct {
			ID  uint
			Val string
		}
		if err := s.db.Model(c.model).Select("id AS id", c.column+" AS val").Where(c.column+" LIKE ?", like).Scan(&rows).Error; err != nil {
			log.Printf("reseal: reading %T.%s: %v", c.model, c.column, err)
			continue
		}
		for _, r := range rows {
			enc, err := s.resealBlob(r.Val)
			if err != nil {
				log.Printf("reseal: %T.%s of row %d is left as it is: %v", c.model, c.column, r.ID, err)
				continue
			}
			res := s.db.Model(c.model).Where("id = ? AND "+c.column+" = ?", r.ID, r.Val).UpdateColumn(c.column, enc)
			if res.Error != nil {
				log.Printf("reseal: writing %T.%s of row %d: %v", c.model, c.column, r.ID, res.Error)
				continue
			}
			sealed += int(res.RowsAffected)
		}
	}
	// The settings that hold a sealed map (the SMTP account).
	var settings []models.Setting
	if err := s.db.Where("value LIKE ?", like).Find(&settings).Error; err != nil {
		log.Printf("reseal: reading settings: %v", err)
		return sealed
	}
	for _, st := range settings {
		enc, err := s.resealBlob(st.Value)
		if err != nil {
			log.Printf("reseal: setting %q is left as it is: %v", st.Key, err)
			continue
		}
		res := s.db.Model(&models.Setting{}).Where("key = ? AND value = ?", st.Key, st.Value).UpdateColumn("value", enc)
		if res.Error != nil {
			log.Printf("reseal: writing setting %q: %v", st.Key, res.Error)
			continue
		}
		sealed += int(res.RowsAffected)
	}
	return sealed
}

// resealBlob turns a "plain:" value, whichever of the two forms wrote it (a string
// or a JSON map, both as base64 of what was meant), into its sealed form.
func (s *Store) resealBlob(blob string) (string, error) {
	b, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(blob, secretPrefixPlain))
	if err != nil {
		return "", err
	}
	ct, err := crypto.Seal(s.key, b)
	if err != nil {
		return "", err
	}
	return secretPrefixEnc + ct, nil
}
