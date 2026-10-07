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

	// Timezone is the IANA zone the cron expression is read in (e.g.
	// "Europe/Paris"). Empty means the control plane's own zone, which in a
	// container is UTC — so "restart at 4am" used to fire at 6am local in
	// summer, with nothing anywhere saying why.
	Timezone string `gorm:"size:64" json:"timezone,omitempty"`

	// Observed execution state, written by the scheduler.
	NextRun    *time.Time `json:"nextRun,omitempty"`
	LastRun    *time.Time `json:"lastRun,omitempty"`
	LastStatus string     `json:"lastStatus,omitempty"`

	// Run is the chain under way, nil when none is. It is kept in the
	// database so that a controller restarted mid-chain -- an upgrade, a
	// rollout, a node drain -- carries on where it was: the chain lived in the
	// controller's memory, and a restart during a delay or a backup dropped
	// the rest of it, so a nightly stop, backup, start left the server stopped
	// until the next night.
	Run *ScheduleRun `gorm:"serializer:json" json:"run,omitempty"`
	// Generation binds the current checkpoint to this task revision and firing.
	// Cancelling or replacing a run invalidates every outstanding writer.
	Generation uint64 `gorm:"not null;default:0" json:"-"`
}

// ScheduleRun is how far a chain has got.
type ScheduleRun struct {
	Generation uint64    `json:"generation"`
	Fired      time.Time `json:"fired"` // when the schedule fired
	Next       int       `json:"next"`  // index of the task to run next
	Due        time.Time `json:"due"`   // when it runs, its delay included
	// Backup is the backup the next task requested and waits for, so that a
	// chain picked up again waits for it rather than taking another one.
	Backup uint     `json:"backup,omitempty"`
	Done   []string `json:"done,omitempty"` // what each task before it did
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
