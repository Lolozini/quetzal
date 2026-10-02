package models

import "time"

// Upload kinds.
const (
	UploadFile    = "file"    // the bytes become one file
	UploadArchive = "archive" // the bytes are an archive, unpacked into a directory
)

// FileUpload is a file being sent to a server's volume in pieces. Each piece is
// its own request, short enough for any proxy in front of the panel to let
// through, and the pieces gather in a temporary file beside the destination
// until the last one arrives. How much has arrived is the size of that file,
// which is also where an interrupted upload resumes.
type FileUpload struct {
	// ID is random and goes in URLs; it also names the temporary file.
	ID        string    `gorm:"primaryKey;size:32" json:"id"`
	CreatedAt time.Time `json:"createdAt"`

	ServerID uint `gorm:"index" json:"serverId"`
	// UserID is who started it: nobody else may add to it or finish it.
	UserID uint `gorm:"index" json:"-"`
	// Path is the destination as the client gave it, relative to the data
	// directory: the file for UploadFile, the directory for UploadArchive.
	Path   string `json:"path"`
	Kind   string `gorm:"size:16" json:"kind"`
	Format string `gorm:"size:8" json:"format,omitempty"` // "zip" or "tar", for an archive
	Size   int64  `json:"size"`

	// ExpiresAt moves forward with each piece; an upload left alone past it is
	// collected, temporary file included.
	ExpiresAt time.Time `gorm:"index" json:"expiresAt"`
	// LockedUntil keeps one request at a time working on the upload: two
	// pieces appended at once would interleave.
	LockedUntil time.Time `json:"-"`
}
