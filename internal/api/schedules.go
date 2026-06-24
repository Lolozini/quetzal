package api

import (
	"errors"
	"fmt"
	"net/http"
	"strconv"
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
	sc := &models.Schedule{
		ServerID: srv.ID,
		Name:     req.Name,
		Cron:     req.Cron,
		Tasks:    tasks,
		Action:   tasks[0].Action, // mirror the first task for legacy display
		Payload:  tasks[0].Payload,
		Enabled:  req.Enabled,
	}
	if sc.Enabled {
		if nr, err := scheduler.NextRun(sc.Cron, time.Now()); err == nil {
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
	sc, ok := s.lookupSchedule(w, r, models.PermSchedules)
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
	sc.Name, sc.Cron, sc.Enabled = req.Name, req.Cron, req.Enabled
	sc.Tasks = tasks
	sc.Action, sc.Payload = tasks[0].Action, tasks[0].Payload
	// Recompute the next fire time from the (possibly changed) cron; clear it when
	// disabled so the scheduler won't fire it.
	sc.NextRun = nil
	if sc.Enabled {
		if nr, err := scheduler.NextRun(sc.Cron, time.Now()); err == nil {
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
	sc, ok := s.lookupSchedule(w, r, models.PermSchedules)
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
// the schedule belongs to it.
func (s *Server) lookupSchedule(w http.ResponseWriter, r *http.Request, perm string) (*models.Schedule, bool) {
	srv, ok := s.requireServer(w, r, perm)
	if !ok {
		return nil, false
	}
	sid, err := strconv.ParseUint(strings.TrimSpace(r.PathValue("sid")), 10, 64)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid schedule id")
		return nil, false
	}
	sc, err := s.Store.GetSchedule(uint(sid))
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, http.StatusNotFound, "schedule not found")
		} else {
			writeError(w, http.StatusInternalServerError, err.Error())
		}
		return nil, false
	}
	if sc.ServerID != srv.ID {
		writeError(w, http.StatusNotFound, "schedule not found")
		return nil, false
	}
	return sc, true
}

// validateSchedule checks the request and returns the normalized task chain. A
// legacy single Action/Payload (no Tasks) is accepted and turned into one task.
func validateSchedule(req scheduleRequest) ([]models.ScheduleTask, error) {
	if strings.TrimSpace(req.Name) == "" {
		return nil, errors.New("name is required")
	}
	if _, err := scheduler.NextRun(req.Cron, time.Now()); err != nil {
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
