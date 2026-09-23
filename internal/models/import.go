package models

import "time"

// ImportPhase tracks the copy of a server's data from another panel.
type ImportPhase string

const (
	// ImportPreparing: the source panel is producing an archive of the server
	// (a backup, or a compressed copy when the server has no backup slot).
	ImportPreparing ImportPhase = "Preparing"
	// ImportDownloading: the archive streams from the source into the volume.
	ImportDownloading ImportPhase = "Downloading"
	// ImportDone: the data is in place and the server is marked installed.
	ImportDone ImportPhase = "Done"
	// ImportFailed: the import stopped; Message says why. It can be retried.
	ImportFailed ImportPhase = "Failed"
)

// importStale is how long a running import may go without a heartbeat before it
// is considered dead (the apiserver that ran it restarted). Every phase beats
// far more often than this.
const importStale = 2 * time.Minute

// ImportState is the data import of a server from a Pterodactyl panel. It is nil
// for servers that were never imported. The API key used is never stored: a
// retry asks for it again.
type ImportState struct {
	Phase ImportPhase `json:"phase"`
	// Source names what is imported: the panel host and the server identifier.
	Source    string    `json:"source"`
	Message   string    `json:"message,omitempty"`
	Bytes     int64     `json:"bytes,omitempty"`
	Total     int64     `json:"total,omitempty"`
	StartedAt time.Time `json:"startedAt"`
	UpdatedAt time.Time `json:"updatedAt"`
	// StartAfter starts the server once the data is in place.
	StartAfter bool `json:"startAfter,omitempty"`
}

// Running reports whether the import is still in progress at now. A running
// phase whose heartbeat went quiet belongs to an apiserver that is gone.
func (i *ImportState) Running(now time.Time) bool {
	if i == nil || (i.Phase != ImportPreparing && i.Phase != ImportDownloading) {
		return false
	}
	return now.Sub(i.UpdatedAt) < importStale
}

// Effective returns the state as a reader should see it: a running import whose
// heartbeat stopped is reported as failed.
func (i *ImportState) Effective(now time.Time) *ImportState {
	if i == nil || i.Running(now) || (i.Phase != ImportPreparing && i.Phase != ImportDownloading) {
		return i
	}
	c := *i
	c.Phase = ImportFailed
	c.Message = "the import was interrupted (the control plane restarted); retry it"
	return &c
}
