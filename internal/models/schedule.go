package models

import "time"

// ScheduleAction is what a schedule does when it fires.
type ScheduleAction string

const (
	SchedStart   ScheduleAction = "start"   // set desired state Running
	SchedStop    ScheduleAction = "stop"    // graceful stop (desired Stopped)
	SchedRestart ScheduleAction = "restart" // recreate the pod
	SchedCommand ScheduleAction = "command" // send Payload to the console (stdin)
	SchedBackup  ScheduleAction = "backup"  // trigger a backup
)

// Schedule is a cron-driven task attached to a server. It is game-agnostic:
// the action is generic (power/console/backup), never game-specific.
type Schedule struct {
	ID        uint      `gorm:"primaryKey" json:"id"`
	CreatedAt time.Time `json:"createdAt"`
	UpdatedAt time.Time `json:"updatedAt"`

	ServerID uint   `gorm:"index" json:"serverId"`
	Name     string `json:"name"`
	// Cron is a standard 5-field cron expression (or a @descriptor).
	Cron    string         `json:"cron"`
	Action  ScheduleAction `json:"action"`
	Payload string         `json:"payload,omitempty"` // command text for SchedCommand
	Enabled bool           `json:"enabled"`

	// Observed execution state, written by the scheduler.
	NextRun    *time.Time `json:"nextRun,omitempty"`
	LastRun    *time.Time `json:"lastRun,omitempty"`
	LastStatus string     `json:"lastStatus,omitempty"`
}
