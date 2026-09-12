package api

import (
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/lolozini/quetzal/internal/models"
	"github.com/lolozini/quetzal/internal/scheduler"
	"github.com/lolozini/quetzal/internal/store"
)

func (s *Server) handleListSchedules(w http.ResponseWriter, r *http.Request) {
	srv, ok := s.requireServer(w, r, models.PermView)
	if !ok {
		return
	}
	scs, err := s.Store.ListSchedulesForServer(srv.ID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, scs)
}

type scheduleRequest struct {
	Name string `json:"name"`
	Cron string `json:"cron"`
	// Tasks is the ordered chain. For backward compatibility, a single legacy
	// Action/Payload (with no Tasks) is accepted and normalized into one task.
	Tasks   []models.ScheduleTask `json:"tasks"`
	Action  models.ScheduleAction `json:"action"`
	Payload string                `json:"payload"`
	Enabled bool                  `json:"enabled"`
	// Timezone is the IANA zone the cron is read in; empty keeps the control
	// plane's own, which in a container is UTC.
	Timezone string `json:"timezone"`
}

// schedulePatch is the update body. Every field is optional: PATCH edits what
// the caller sent and leaves the rest alone. Decoding straight into
// scheduleRequest instead made an omitted "enabled" read as false, silently
// disabling the schedule — a backup chain would just stop running, with a 200
// and no warning.
type schedulePatch struct {
	Name     *string                `json:"name"`
	Cron     *string                `json:"cron"`
	Tasks    *[]models.ScheduleTask `json:"tasks"`
	Action   *models.ScheduleAction `json:"action"`
	Payload  *string                `json:"payload"`
	Enabled  *bool                  `json:"enabled"`
	Timezone *string                `json:"timezone"`
}

func (s *Server) handleCreateSchedule(w http.ResponseWriter, r *http.Request) {
	srv, ok := s.requireServer(w, r, models.PermSchedules)
	if !ok {
		return
	}
	var req scheduleRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid body")
		return
	}
	tasks, err := validateSchedule(req)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := s.authorizeTasks(r, srv, tasks); err != nil {
		writeError(w, http.StatusForbidden, err.Error())
		return
	}
	sc := &models.Schedule{
		ServerID: srv.ID,
		Name:     req.Name,
		Cron:     req.Cron,
		Timezone: strings.TrimSpace(req.Timezone),
		Tasks:    tasks,
		Action:   tasks[0].Action, // mirror the first task for legacy display
		Payload:  tasks[0].Payload,
		Enabled:  req.Enabled,
	}
	if sc.Enabled {
		if nr, err := scheduler.NextRunIn(sc.Cron, time.Now(), sc.Timezone); err == nil {
			sc.NextRun = &nr
		}
	}
	if err := s.Store.CreateSchedule(sc); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.audit(r, srv.ID, "schedule.create", sc.Name+" ("+summarizeTasks(tasks)+" @ "+sc.Cron+")")
	writeJSON(w, http.StatusCreated, sc)
}

func (s *Server) handleUpdateSchedule(w http.ResponseWriter, r *http.Request) {
	sc, srv, ok := s.lookupSchedule(w, r, models.PermSchedules)
	if !ok {
		return
	}
	var patch schedulePatch
	if err := decodeJSON(r, &patch); err != nil {
		writeError(w, http.StatusBadRequest, "invalid body")
		return
	}
	// Seed the request from what is stored, then overlay only the fields the
	// caller actually sent, so a partial PATCH edits one thing instead of
	// resetting the rest to their zero values.
	req := scheduleRequest{
		Name: sc.Name, Cron: sc.Cron, Tasks: sc.TaskChain(), Enabled: sc.Enabled,
		Timezone: sc.Timezone,
	}
	if patch.Name != nil {
		req.Name = *patch.Name
	}
	if patch.Cron != nil {
		req.Cron = *patch.Cron
	}
	if patch.Tasks != nil {
		req.Tasks = *patch.Tasks
	}
	if patch.Action != nil {
		req.Action, req.Tasks = *patch.Action, nil
		if patch.Payload != nil {
			req.Payload = *patch.Payload
		}
	}
	if patch.Enabled != nil {
		req.Enabled = *patch.Enabled
	}
	if patch.Timezone != nil {
		req.Timezone = *patch.Timezone
	}
	tasks, err := validateSchedule(req)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	// Turning a schedule off, or renaming it, causes nothing to run: a subuser
	// watching a task misbehave can stop it whether or not they could have
	// written it. Anything that changes what runs — or that arms it again — is
	// authorized like a fresh chain.
	if req.Enabled || !sameChain(sc.TaskChain(), tasks) {
		if err := s.authorizeTasks(r, srv, tasks); err != nil {
			writeError(w, http.StatusForbidden, err.Error())
			return
		}
	}
	sc.Name, sc.Cron, sc.Enabled = req.Name, req.Cron, req.Enabled
	sc.Timezone = strings.TrimSpace(req.Timezone)
	sc.Tasks = tasks
	sc.Action, sc.Payload = tasks[0].Action, tasks[0].Payload
	// Recompute the next fire time from the (possibly changed) cron; clear it when
	// disabled so the scheduler won't fire it.
	sc.NextRun = nil
	if sc.Enabled {
		if nr, err := scheduler.NextRunIn(sc.Cron, time.Now(), sc.Timezone); err == nil {
			sc.NextRun = &nr
		}
	}
	if err := s.Store.UpdateSchedule(sc); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, sc)
}

func (s *Server) handleDeleteSchedule(w http.ResponseWriter, r *http.Request) {
	sc, _, ok := s.lookupSchedule(w, r, models.PermSchedules)
	if !ok {
		return
	}
	if err := s.Store.DeleteSchedule(sc.ID); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.audit(r, sc.ServerID, "schedule.delete", sc.Name)
	w.WriteHeader(http.StatusNoContent)
}

// lookupSchedule resolves {sid}, checks `perm` on the parent server, and that
// the schedule belongs to it. The server is returned as well, because the task
// chain has to be authorized against it (see authorizeTasks).
func (s *Server) lookupSchedule(w http.ResponseWriter, r *http.Request, perm string) (*models.Schedule, *models.Server, bool) {
	srv, ok := s.requireServer(w, r, perm)
	if !ok {
		return nil, nil, false
	}
	sid, ok := pathID(r, "sid")
	if !ok {
		writeError(w, http.StatusBadRequest, "invalid schedule id")
		return nil, nil, false
	}
	sc, err := s.Store.GetSchedule(sid)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, http.StatusNotFound, "schedule not found")
		} else {
			writeError(w, http.StatusInternalServerError, err.Error())
		}
		return nil, nil, false
	}
	if sc.ServerID != srv.ID {
		writeError(w, http.StatusNotFound, "schedule not found")
		return nil, nil, false
	}
	return sc, srv, true
}

// taskPermission maps a scheduled action to the permission needed to perform it
// by hand. An action it does not know returns false, and authorizeTasks refuses
// it: validateSchedule already rejects unknown actions, so this only matters the
// day someone adds one — and then it has to be granted a permission here rather
// than arriving unguarded.
func taskPermission(a models.ScheduleAction) (string, bool) {
	switch a {
	case models.SchedStart, models.SchedStop, models.SchedRestart:
		return models.PermPower, true
	case models.SchedCommand:
		return models.PermConsole, true
	case models.SchedBackup:
		return models.PermBackups, true
	}
	return "", false
}

// sameChain reports whether two task chains would run the same thing.
func sameChain(a, b []models.ScheduleTask) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// authorizeTasks refuses a chain containing an action the caller could not
// perform directly. Without it "schedules" is the strongest permission there is:
// a subuser who holds it and nothing else could schedule a console command a
// minute out and get the console, power and backups it was never granted.
func (s *Server) authorizeTasks(r *http.Request, srv *models.Server, tasks []models.ScheduleTask) error {
	u := userFrom(r.Context())
	for _, t := range tasks {
		perm, known := taskPermission(t.Action)
		if !known {
			return fmt.Errorf("unknown task action %q", t.Action)
		}
		if s.can(u, srv, perm) {
			continue
		}
		return fmt.Errorf("scheduling a %q task needs the %q permission on this server", t.Action, perm)
	}
	return nil
}

// validateSchedule checks the request and returns the normalized task chain. A
// legacy single Action/Payload (no Tasks) is accepted and turned into one task.
func validateSchedule(req scheduleRequest) ([]models.ScheduleTask, error) {
	if strings.TrimSpace(req.Name) == "" {
		return nil, errors.New("name is required")
	}
	if _, err := scheduler.LoadZone(req.Timezone); err != nil {
		return nil, err
	}
	if _, err := scheduler.NextRunIn(req.Cron, time.Now(), req.Timezone); err != nil {
		return nil, errors.New("invalid cron expression: " + err.Error())
	}
	tasks := req.Tasks
	if len(tasks) == 0 && req.Action != "" {
		tasks = []models.ScheduleTask{{Action: req.Action, Payload: req.Payload}}
	}
	if len(tasks) == 0 {
		return nil, errors.New("at least one task is required")
	}
	if len(tasks) > models.MaxScheduleTasks {
		return nil, fmt.Errorf("too many tasks (max %d)", models.MaxScheduleTasks)
	}
	for i, t := range tasks {
		switch t.Action {
		case models.SchedStart, models.SchedStop, models.SchedRestart, models.SchedBackup:
		case models.SchedCommand:
			if strings.TrimSpace(t.Payload) == "" {
				return nil, fmt.Errorf("task %d: command action requires a payload", i+1)
			}
		default:
			return nil, fmt.Errorf("task %d: action must be start|stop|restart|command|backup", i+1)
		}
		if t.TimeOffset < 0 || t.TimeOffset > models.MaxTaskOffsetSeconds {
			return nil, fmt.Errorf("task %d: timeOffset must be 0–%d seconds", i+1, models.MaxTaskOffsetSeconds)
		}
	}
	return tasks, nil
}

// summarizeTasks renders a chain for audit/log lines.
func summarizeTasks(tasks []models.ScheduleTask) string {
	if len(tasks) == 1 {
		return string(tasks[0].Action)
	}
	parts := make([]string, len(tasks))
	for i, t := range tasks {
		parts[i] = string(t.Action)
	}
	return fmt.Sprintf("%d tasks: %s", len(tasks), strings.Join(parts, "→"))
}
