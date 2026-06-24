// Package scheduler runs cron-driven task chains attached to servers. It is
// generic: actions are power/console/backup, never game-specific. The concrete
// side effects are provided by an Executor so the scheduling logic stays
// testable.
//
// A schedule's tasks run as an ordered chain, each with an optional delay
// (TimeOffset). Because a delay must not block the controller's reconcile loop
// (Tick is called serially alongside reconciliation/backups/hibernation), each
// due chain runs in its own goroutine; an in-flight guard prevents a schedule
// from overlapping itself, and next_run is advanced up front so a long chain
// can't re-fire.
package scheduler

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"

	"github.com/robfig/cron/v3"

	"github.com/lolozini/quetzal/internal/models"
	"github.com/lolozini/quetzal/internal/store"
)

// Executor performs a schedule task's side effect against a server.
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
	// Sleep waits d or until ctx is cancelled; overridable in tests so chain
	// delays don't make tests slow. Returns ctx.Err() when cancelled.
	Sleep func(ctx context.Context, d time.Duration) error

	mu       sync.Mutex
	inflight map[uint]bool
	wg       sync.WaitGroup
}

// New returns a Scheduler.
func New(st *store.Store, ex Executor) *Scheduler {
	return &Scheduler{Store: st, Exec: ex, Now: time.Now, inflight: map[uint]bool{}}
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

// Tick fires every enabled schedule whose NextRun is due. It is meant to be
// called periodically (granularity finer than 1 minute) by the leader
// controller. Due chains run asynchronously; use Wait to block for them.
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
		// Advance NextRun immediately so a long-running or delayed chain can't
		// re-fire on the next tick. The async result write below only touches
		// last_run/last_status, never this advanced next_run.
		var next *time.Time
		if nr, err := NextRun(sc.Cron, now); err == nil {
			next = &nr
		}
		_ = s.Store.SetScheduleNextRun(sc.ID, next)

		// Skip if a previous run of this same schedule is still in progress.
		if !s.acquire(sc.ID) {
			continue
		}
		scCopy := *sc
		s.wg.Add(1)
		go func() {
			defer s.wg.Done()
			defer s.release(scCopy.ID)
			status := s.runChain(ctx, &scCopy)
			if err := s.Store.MarkScheduleResult(scCopy.ID, now, status); err != nil {
				log.Printf("scheduler: mark result %d: %v", scCopy.ID, err)
			}
		}()
	}
}

// Wait blocks until all in-flight chains finish (for graceful shutdown / tests).
func (s *Scheduler) Wait() { s.wg.Wait() }

// runChain executes a schedule's task chain in order and returns a status
// summary. Each task may wait TimeOffset seconds first; a failing task aborts
// the rest unless it is ContinueOnFailure. The server is re-loaded before each
// task so a mid-chain suspension or deletion is respected.
func (s *Scheduler) runChain(ctx context.Context, sc *models.Schedule) string {
	if _, err := s.Store.GetServer(sc.ServerID); err != nil {
		// The server is gone (deleted out from under the schedule): remove the
		// orphan so it stops firing and spamming errors.
		if errors.Is(err, store.ErrNotFound) {
			_ = s.Store.DeleteSchedule(sc.ID)
			return "removed (server deleted)"
		}
		return "error: server: " + err.Error()
	}
	tasks := sc.TaskChain()
	if len(tasks) == 0 {
		return "error: no tasks"
	}
	var b strings.Builder
	for i, t := range tasks {
		if i > 0 {
			b.WriteString("; ")
		}
		if t.TimeOffset > 0 {
			if err := s.sleep(ctx, time.Duration(t.TimeOffset)*time.Second); err != nil {
				fmt.Fprintf(&b, "#%d %s: cancelled", i+1, t.Action)
				break
			}
		}
		srv, err := s.Store.GetServer(sc.ServerID)
		if err != nil {
			fmt.Fprintf(&b, "#%d %s: error: server unavailable", i+1, t.Action)
			break
		}
		ok, msg := s.runTask(ctx, srv, t)
		fmt.Fprintf(&b, "#%d %s: %s", i+1, t.Action, msg)
		if !ok && !t.ContinueOnFailure {
			b.WriteString(" — chain aborted")
			break
		}
	}
	return b.String()
}

// runTask performs a single task and reports whether it succeeded plus a short
// message. A power action on a suspended server is skipped (not a failure) so a
// cron can't silently lift an admin suspension.
func (s *Scheduler) runTask(ctx context.Context, srv *models.Server, t models.ScheduleTask) (bool, string) {
	if srv.DesiredState == models.StateSuspended &&
		(t.Action == models.SchedStart || t.Action == models.SchedStop || t.Action == models.SchedRestart) {
		return true, "skipped (server suspended)"
	}
	var err error
	switch t.Action {
	case models.SchedStart:
		err = s.Exec.Start(ctx, srv)
	case models.SchedStop:
		err = s.Exec.Stop(ctx, srv)
	case models.SchedRestart:
		err = s.Exec.Restart(ctx, srv)
	case models.SchedCommand:
		err = s.Exec.Command(ctx, srv, t.Payload)
	case models.SchedBackup:
		err = s.Exec.Backup(ctx, srv)
	default:
		return false, "error: unknown action " + string(t.Action)
	}
	if err != nil {
		return false, "error: " + err.Error()
	}
	return true, "ok"
}

// acquire marks a schedule as in-flight, returning false if it already is.
func (s *Scheduler) acquire(id uint) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.inflight[id] {
		return false
	}
	s.inflight[id] = true
	return true
}

func (s *Scheduler) release(id uint) {
	s.mu.Lock()
	delete(s.inflight, id)
	s.mu.Unlock()
}

func (s *Scheduler) sleep(ctx context.Context, d time.Duration) error {
	if s.Sleep != nil {
		return s.Sleep(ctx, d)
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (s *Scheduler) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}
