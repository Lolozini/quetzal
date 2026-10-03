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
// can't re-fire. How far a chain has got is kept in the database (Schedule.Run),
// and a controller that starts finds the chains a previous one left half done
// and carries them on.
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
	// Backup requests a backup and returns its ID; the chain then waits for it
	// to end (see awaitBackup).
	Backup(ctx context.Context, srv *models.Server) (uint, error)
}

// backupPoll is how often a chain's backup step looks at its backup.
const backupPoll = 5 * time.Second

// backupWait bounds that wait. The backup manager fails a backup whose Job runs
// past its deadline, six hours, so this only ends the wait for one that never
// got as far as a Job, on a cluster that stopped answering.
const backupWait = 7 * time.Hour

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
	return NextRunIn(expr, after, "")
}

// NextRunIn is NextRun read in an IANA time zone: the cron fields mean what they
// say in that zone, so "0 4 * * *" is four in the morning there and not wherever
// the control plane happens to run. An empty zone keeps the process's own, which
// in a container is UTC.
//
// A daylight-saving jump is handled by the cron library, which walks forward
// from `after` in the given location: an hour that does not exist that day is
// skipped rather than fired twice.
func NextRunIn(expr string, after time.Time, tz string) (time.Time, error) {
	loc, err := LoadZone(tz)
	if err != nil {
		return time.Time{}, err
	}
	sched, err := cron.ParseStandard(expr)
	if err != nil {
		return time.Time{}, err
	}
	return sched.Next(after.In(loc)), nil
}

// LoadZone resolves an IANA zone name, with empty meaning the process's own.
// The binary embeds the zone database (see the time/tzdata import in the
// commands): the runtime image is distroless and carries no /usr/share/zoneinfo,
// so without it every named zone would fail to load.
func LoadZone(tz string) (*time.Location, error) {
	tz = strings.TrimSpace(tz)
	if tz == "" {
		return time.Local, nil
	}
	loc, err := time.LoadLocation(tz)
	if err != nil {
		return nil, fmt.Errorf("unknown time zone %q (use an IANA name such as Europe/Paris)", tz)
	}
	return loc, nil
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
		// A chain under way that nothing here runs was left by a controller
		// that went away mid-chain: carry it on.
		if sc.Run != nil {
			if s.acquire(sc.ID) {
				s.launch(ctx, *sc, *sc.Run, true)
			}
			continue
		}
		// A schedule with no computed NextRun (freshly enabled / migrated) gets one
		// now and fires on a later tick — never retroactively.
		if sc.NextRun == nil {
			if nr, err := NextRunIn(sc.Cron, now, sc.Timezone); err == nil {
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
		if nr, err := NextRunIn(sc.Cron, now, sc.Timezone); err == nil {
			next = &nr
		}
		_ = s.Store.SetScheduleNextRun(sc.ID, next)

		// Skip if a previous run of this same schedule is still in progress.
		if !s.acquire(sc.ID) {
			continue
		}
		run := models.ScheduleRun{Fired: now, Due: now}
		if tasks := sc.TaskChain(); len(tasks) > 0 {
			run.Due = now.Add(time.Duration(tasks[0].TimeOffset) * time.Second)
		}
		if err := s.Store.SetScheduleRun(sc.ID, &run); err != nil {
			log.Printf("scheduler: start chain %d: %v", sc.ID, err)
			s.release(sc.ID)
			continue
		}
		s.launch(ctx, *sc, run, false)
	}
}

// launch runs a chain in its own goroutine, from where run says.
func (s *Scheduler) launch(ctx context.Context, sc models.Schedule, run models.ScheduleRun, resumed bool) {
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		defer s.release(sc.ID)
		// Isolate a panicking task: a chain runs detached, so an unrecovered
		// panic here would take down the whole controller (and all
		// reconciliation) rather than just failing this schedule.
		defer func() {
			if r := recover(); r != nil {
				log.Printf("scheduler: chain %d panicked: %v", sc.ID, r)
				s.finish(&sc, run.Fired, fmt.Sprintf("error: panic: %v", r))
			}
		}()
		status, ended := s.runChain(ctx, &sc, &run, resumed)
		if !ended {
			return // the controller is going away: the next one carries on
		}
		s.finish(&sc, run.Fired, status)
	}()
}

// finish closes a chain: its run is over, and its result recorded.
func (s *Scheduler) finish(sc *models.Schedule, fired time.Time, status string) {
	if err := s.Store.SetScheduleRun(sc.ID, nil); err != nil {
		log.Printf("scheduler: end chain %d: %v", sc.ID, err)
	}
	if err := s.Store.MarkScheduleResult(sc.ID, fired, status); err != nil {
		log.Printf("scheduler: mark result %d: %v", sc.ID, err)
	}
	s.record(sc, status)
}

// Wait blocks until all in-flight chains finish (for graceful shutdown / tests).
func (s *Scheduler) Wait() { s.wg.Wait() }

// record writes a run into the panel's audit log and the server's activity.
// What a schedule did showed only as its last status, which the next run
// overwrote, and nowhere in the log an administrator reads: a server restarted
// at four in the morning left no trace of why. A run whose server is gone is
// not recorded: the schedule went with it.
func (s *Scheduler) record(sc *models.Schedule, status string) {
	srv, err := s.Store.GetServer(sc.ServerID)
	if err != nil {
		return
	}
	name := sc.Name
	if name == "" {
		name = fmt.Sprintf("schedule #%d", sc.ID)
	}
	detail := fmt.Sprintf("%q: %s", name, status)
	if err := s.Store.AddAudit(&models.AuditEntry{ServerID: srv.ID, Action: models.EventScheduleRun, Detail: detail}); err != nil {
		log.Printf("scheduler: audit %d: %v", sc.ID, err)
	}
	if err := s.Store.AddEvent(&models.Event{ServerID: srv.ID, Type: models.EventScheduleRun, Message: srv.Slug + ": " + detail}); err != nil {
		log.Printf("scheduler: event %d: %v", sc.ID, err)
	}
}

// resumeLimit is how late a chain left half done may still be carried on:
// past it, the rest of the chain is closer to the schedule's next firing than
// to the one it belongs to.
const resumeLimit = 24 * time.Hour

// runChain executes a schedule's task chain in order, from the task run says,
// and returns a status summary and whether the chain ended. Each task may wait
// TimeOffset seconds first; a failing task aborts the rest unless it is
// ContinueOnFailure. The server is re-loaded before each task so a mid-chain
// suspension or deletion is respected. Progress is written to run, and to the
// database, after each task; a cancelled context leaves the chain where it
// was, for the next controller to carry on.
func (s *Scheduler) runChain(ctx context.Context, sc *models.Schedule, run *models.ScheduleRun, resumed bool) (string, bool) {
	if _, err := s.Store.GetServer(sc.ServerID); err != nil {
		// The server is gone (deleted out from under the schedule): remove the
		// orphan so it stops firing and spamming errors.
		if errors.Is(err, store.ErrNotFound) {
			_ = s.Store.DeleteSchedule(sc.ID)
			return "removed (server deleted)", true
		}
		return "error: server: " + err.Error(), true
	}
	tasks := sc.TaskChain()
	if len(tasks) == 0 {
		return "error: no tasks", true
	}
	if resumed {
		if late := s.now().Sub(run.Due); late > resumeLimit {
			return summary(run, fmt.Sprintf("the controller restarted mid-chain and the rest was %s overdue; not run", late.Round(time.Minute))), true
		}
		log.Printf("scheduler: carrying on chain %d at task %d", sc.ID, run.Next+1)
	}
	for first := true; run.Next < len(tasks); first = false {
		i, t := run.Next, tasks[run.Next]
		wait := time.Duration(t.TimeOffset) * time.Second
		if first && resumed {
			wait = run.Due.Sub(s.now())
		}
		if wait > 0 {
			if err := s.sleep(ctx, wait); err != nil {
				return "", false
			}
		}
		srv, err := s.Store.GetServer(sc.ServerID)
		if err != nil {
			run.Done = append(run.Done, fmt.Sprintf("#%d %s: error: server unavailable", i+1, t.Action))
			break
		}
		var ok bool
		var msg string
		if t.Action == models.SchedBackup && run.Backup != 0 {
			// Carried on while waiting for its backup: that one, not another.
			ok, msg = result(s.awaitBackup(ctx, run.Backup))
		} else {
			ok, msg = s.runTask(ctx, srv, t, func(id uint) {
				run.Backup = id
				s.saveRun(sc.ID, run)
			})
		}
		if !ok && ctx.Err() != nil {
			return "", false // cut short by the controller going away: run it again
		}
		entry := fmt.Sprintf("#%d %s: %s", i+1, t.Action, msg)
		if !ok && !t.ContinueOnFailure {
			run.Done = append(run.Done, entry+" — chain aborted")
			break
		}
		run.Done = append(run.Done, entry)
		run.Next, run.Backup = i+1, 0
		if run.Next < len(tasks) {
			run.Due = s.now().Add(time.Duration(tasks[run.Next].TimeOffset) * time.Second)
		}
		s.saveRun(sc.ID, run)
	}
	if resumed {
		return summary(run, "carried on after the controller restarted"), true
	}
	return summary(run, ""), true
}

// summary is a chain's status: what each task did, and a note.
func summary(run *models.ScheduleRun, note string) string {
	out := strings.Join(run.Done, "; ")
	switch {
	case note == "":
	case out == "":
		out = note
	default:
		out += " (" + note + ")"
	}
	return out
}

func (s *Scheduler) saveRun(id uint, run *models.ScheduleRun) {
	if err := s.Store.SetScheduleRun(id, run); err != nil {
		log.Printf("scheduler: save chain %d: %v", id, err)
	}
}

func result(err error) (bool, string) {
	if err != nil {
		return false, "error: " + err.Error()
	}
	return true, "ok"
}

// runTask performs a single task and reports whether it succeeded plus a short
// message. Nothing runs on a suspended server, which its owner's schedules must
// not touch any more than they can: a power action would lift the suspension,
// and each backup's retention pushes out a snapshot from before it (skipped,
// not a failure).
func (s *Scheduler) runTask(ctx context.Context, srv *models.Server, t models.ScheduleTask, backupRequested func(uint)) (bool, string) {
	if srv.DesiredState == models.StateSuspended {
		return true, "skipped (server suspended)"
	}
	// Starting a server whose data is still being imported would run the egg's
	// install over the arriving files; starting one mid-transfer puts a pod back
	// on the volume the transfer is waiting to have to itself, and the transfer
	// then waits for good. The API refuses both; the scheduler has to as well.
	if t.Action == models.SchedStart || t.Action == models.SchedRestart {
		if srv.Import.Running(time.Now()) {
			return false, "skipped (the server's data is still being imported)"
		}
		if srv.Transfer != nil {
			return false, "skipped (the server is being transferred to another cluster)"
		}
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
		var id uint
		if id, err = s.Exec.Backup(ctx, srv); err == nil {
			backupRequested(id)
			err = s.awaitBackup(ctx, id)
		}
	default:
		return false, "error: unknown action " + string(t.Action)
	}
	return result(err)
}

// awaitBackup waits for the backup a step requested to end, so that the next
// step runs after it rather than alongside it. A chain that stops the server,
// backs it up and starts it again used to start it while the backup was still
// being taken -- copying a running game, the one thing the chain was written to
// avoid -- and one that paused the game's saves for a backup resumed them
// before anything had been copied. The step fails with the backup.
func (s *Scheduler) awaitBackup(ctx context.Context, id uint) error {
	deadline := s.now().Add(backupWait)
	for {
		b, err := s.Store.GetBackup(id)
		switch {
		case errors.Is(err, store.ErrNotFound):
			return errors.New("the backup was deleted before it finished")
		case err != nil:
			return err
		}
		switch b.Phase {
		case models.BackupSucceeded, models.BackupDeleting:
			// Deleting is a succeeded backup someone has since asked to remove.
			return nil
		case models.BackupFailed:
			return fmt.Errorf("the backup failed: %s", b.Message)
		}
		if !s.now().Before(deadline) {
			return fmt.Errorf("the backup had not finished after %s", backupWait)
		}
		if err := s.sleep(ctx, backupPoll); err != nil {
			return err
		}
	}
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
