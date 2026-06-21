package api

import (
	"errors"
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
	Name    string                `json:"name"`
	Cron    string                `json:"cron"`
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
	if err := validateSchedule(req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	sc := &models.Schedule{
		ServerID: srv.ID,
		Name:     req.Name,
		Cron:     req.Cron,
		Action:   req.Action,
		Payload:  req.Payload,
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
	s.audit(r, srv.ID, "schedule.create", sc.Name+" ("+string(sc.Action)+" @ "+sc.Cron+")")
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
	if err := validateSchedule(req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	sc.Name, sc.Cron, sc.Action, sc.Payload, sc.Enabled = req.Name, req.Cron, req.Action, req.Payload, req.Enabled
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

func validateSchedule(req scheduleRequest) error {
	if strings.TrimSpace(req.Name) == "" {
		return errors.New("name is required")
	}
	switch req.Action {
	case models.SchedStart, models.SchedStop, models.SchedRestart, models.SchedBackup:
	case models.SchedCommand:
		if strings.TrimSpace(req.Payload) == "" {
			return errors.New("command action requires a payload")
		}
	default:
		return errors.New("action must be start|stop|restart|command|backup")
	}
	if _, err := scheduler.NextRun(req.Cron, time.Now()); err != nil {
		return errors.New("invalid cron expression: " + err.Error())
	}
	return nil
}
