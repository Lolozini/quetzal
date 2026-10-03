package models

import "time"

// SFTPCursor is how far the controller has read a server's SFTP log: the time
// of the last change it recorded as activity. The log is read again from there,
// so a change is recorded once, across controller restarts too.
type SFTPCursor struct {
	ServerID uint      `gorm:"primaryKey;autoIncrement:false"`
	Seen     time.Time `gorm:"not null"`
}
