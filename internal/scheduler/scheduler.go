// Package scheduler runs cron-driven tasks attached to servers. It is generic:
// actions are power/console/backup, never game-specific. The concrete side
// effects are provided by an Executor so the scheduling logic stays testable.
package scheduler

import (
	"context"
	"fmt"
	"log"
	"time"

	"github.com/robfig/cron/v3"

	"github.com/lolozini/quetzal/internal/models"
	"github.com/lolozini/quetzal/internal/store"
)

// Executor performs a schedule's side effect against a server.
type Executor interface {
	Start(ctx context.Context, srv *models.Server) error
	Stop(ctx context.Context, srv *models.Server) error
	Restart(ctx context.Context, srv *models.Server) error
	Command(ctx context.Context, srv *models.Server, cmd string) error
	Backup(ctx context.Context, srv *models.Server) error
}

// Scheduler evaluates enabled schedules and fires the ones that are due.
type Scheduler struct {
	Store *store.Store
	Exec  Executor
	// Now is overridable for tests; defaults to time.Now.
	Now func() time.Time
}

// New returns a Scheduler.
func New(st *store.Store, ex Executor) *Scheduler {
	return &Scheduler{Store: st, Exec: ex, Now: time.Now}
}

// NextRun parses a standard cron expression and returns the next fire time
// strictly after `after`.
func NextRun(expr string, after time.Time) (time.Time, error) {
	sched, err := cron.ParseStandard(expr)
	if err != nil {
		return time.Time{}, err
	}
	return sched.Next(after), nil
}

// Tick fires every enabled schedule whose NextRun is due, then reschedules it.
// It is meant to be called periodically (granularity finer than 1 minute) by the
// leader controller.
func (s *Scheduler) Tick(ctx context.Context) {
	now := s.now()
	scs, err := s.Store.ListEnabledSchedules()
	if err != nil {
		log.Printf("scheduler: list: %v", err)
		return
	}
	for i := range scs {
		sc := &scs[i]
		// A schedule with no computed NextRun (freshly enabled / migrated) gets one
		// now and fires on a later tick — never retroactively.
		if sc.NextRun == nil {
			if nr, err := NextRun(sc.Cron, now); err == nil {
				_ = s.Store.SetScheduleNextRun(sc.ID, &nr)
			} else {
				log.Printf("scheduler: bad cron %q on schedule %d: %v", sc.Cron, sc.ID, err)
			}
			continue
		}
		if now.Before(*sc.NextRun) {
			continue
		}
		status := s.run(ctx, sc)
		var next *time.Time
		if nr, err := NextRun(sc.Cron, now); err == nil {
			next = &nr
		}
		if err := s.Store.MarkScheduleRun(sc.ID, now, next, status); err != nil {
			log.Printf("scheduler: mark run %d: %v", sc.ID, err)
		}
	}
}

func (s *Scheduler) run(ctx context.Context, sc *models.Schedule) string {
	srv, err := s.Store.GetServer(sc.ServerID)
	if err != nil {
		return "error: server: " + err.Error()
	}
	switch sc.Action {
	case models.SchedStart:
		err = s.Exec.Start(ctx, srv)
	case models.SchedStop:
		err = s.Exec.Stop(ctx, srv)
	case models.SchedRestart:
		err = s.Exec.Restart(ctx, srv)
	case models.SchedCommand:
		err = s.Exec.Command(ctx, srv, sc.Payload)
	case models.SchedBackup:
		err = s.Exec.Backup(ctx, srv)
	default:
		return "error: unknown action " + string(sc.Action)
	}
	if err != nil {
		return "error: " + err.Error()
	}
	return fmt.Sprintf("ok: %s at %s", sc.Action, s.now().Format(time.RFC3339))
}

func (s *Scheduler) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}
