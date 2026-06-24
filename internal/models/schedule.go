package models

import "time"

// ScheduleAction is what a schedule task does when it runs.
type ScheduleAction string

const (
	SchedStart   ScheduleAction = "start"   // set desired state Running
	SchedStop    ScheduleAction = "stop"    // graceful stop (desired Stopped)
	SchedRestart ScheduleAction = "restart" // recreate the pod
	SchedCommand ScheduleAction = "command" // send Payload to the console (stdin)
	SchedBackup  ScheduleAction = "backup"  // trigger a backup
)

// Limits on a schedule's task chain (mirrors Pterodactyl's sane bounds).
const (
	MaxScheduleTasks     = 25        // tasks per schedule
	MaxTaskOffsetSeconds = 24 * 3600 // 24h cap on an inter-task delay
)

// ScheduleTask is one step in a schedule's chain. Tasks run in order; TimeOffset
// is how long to wait before this task (after the previous one), enabling
// patterns like "warn players → wait → stop → wait → backup → start". A failing
// task aborts the rest of the chain unless ContinueOnFailure is set.
type ScheduleTask struct {
	Action            ScheduleAction `json:"action"`
	Payload           string         `json:"payload,omitempty"` // command text for SchedCommand
	TimeOffset        int            `json:"timeOffset"`        // seconds to wait before running this task
	ContinueOnFailure bool           `json:"continueOnFailure,omitempty"`
}

// Schedule is a cron-driven task chain attached to a server. It is
// game-agnostic: actions are generic (power/console/backup), never
// game-specific.
type Schedule struct {
	ID        uint      `gorm:"primaryKey" json:"id"`
	CreatedAt time.Time `json:"createdAt"`
	UpdatedAt time.Time `json:"updatedAt"`

	ServerID uint   `gorm:"index" json:"serverId"`
	Name     string `json:"name"`
	// Cron is a standard 5-field cron expression (or a @descriptor).
	Cron string `json:"cron"`

	// Tasks is the ordered chain run when the schedule fires. Legacy single-task
	// schedules (created before chains) may instead carry Action/Payload below
	// with an empty Tasks; TaskChain() normalizes both into a chain.
	Tasks []ScheduleTask `gorm:"serializer:json" json:"tasks"`

	// Action/Payload are the legacy single-task fields, kept for backward
	// compatibility and mirrored from the first task for display.
	Action  ScheduleAction `json:"action,omitempty"`
	Payload string         `json:"payload,omitempty"`
	Enabled bool           `json:"enabled"`

	// Observed execution state, written by the scheduler.
	NextRun    *time.Time `json:"nextRun,omitempty"`
	LastRun    *time.Time `json:"lastRun,omitempty"`
	LastStatus string     `json:"lastStatus,omitempty"`
}

// TaskChain returns the schedule's ordered tasks, normalizing a legacy
// single-action schedule (Action/Payload, no Tasks) into a one-task chain.
func (sc *Schedule) TaskChain() []ScheduleTask {
	if len(sc.Tasks) > 0 {
		return sc.Tasks
	}
	if sc.Action != "" {
		return []ScheduleTask{{Action: sc.Action, Payload: sc.Payload}}
	}
	return nil
}
