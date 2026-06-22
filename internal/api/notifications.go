package api

import (
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/lolozini/quetzal/internal/models"
)

// channelDTO is the API view of a channel: secret config values are never
// echoed back, only whether each is set.
type channelDTO struct {
	ID        uint               `json:"id"`
	CreatedAt time.Time          `json:"createdAt"`
	UpdatedAt time.Time          `json:"updatedAt"`
	Name      string             `json:"name"`
	Type      models.ChannelType `json:"type"`
	Enabled   bool               `json:"enabled"`
	ServerID  uint               `json:"serverId"`
	Events    []string           `json:"events"`
	// Config holds the non-secret settings (e.g. email host/port/from/to/tls).
	Config map[string]string `json:"config"`
	// Secrets reports which secret keys are configured, without their values.
	Secrets map[string]bool `json:"secrets"`
}

func (s *Server) maskChannel(c *models.NotificationChannel) channelDTO {
	cfg, _ := s.Store.ChannelConfig(c)
	secretKeys := map[string]bool{}
	for _, k := range models.SecretConfigKeys[c.Type] {
		secretKeys[k] = true
	}
	pub := map[string]string{}
	set := map[string]bool{}
	for k, v := range cfg {
		if secretKeys[k] {
			set[k] = v != ""
			continue
		}
		pub[k] = v
	}
	return channelDTO{
		ID: c.ID, CreatedAt: c.CreatedAt, UpdatedAt: c.UpdatedAt,
		Name: c.Name, Type: c.Type, Enabled: c.Enabled, ServerID: c.ServerID,
		Events: c.Events, Config: pub, Secrets: set,
	}
}

// channelRequest is the create/update payload.
type channelRequest struct {
	Name     string             `json:"name"`
	Type     models.ChannelType `json:"type"`
	Enabled  bool               `json:"enabled"`
	ServerID uint               `json:"serverId"`
	Events   []string           `json:"events"`
	// Config carries both public and secret settings. On update, omitting a
	// secret key (or sending it empty) keeps the stored value.
	Config map[string]string `json:"config"`
}

// authorizeChannelScope gates access by the channel's scope: global channels are
// admin-only; server-scoped ones require PermSettings on that server.
func (s *Server) authorizeChannelScope(w http.ResponseWriter, r *http.Request, serverID uint) bool {
	if serverID == 0 {
		return s.requireAdmin(w, r)
	}
	srv, err := s.Store.GetServer(serverID)
	if err != nil {
		writeError(w, http.StatusNotFound, "server not found")
		return false
	}
	if s.can(userFrom(r.Context()), srv, models.PermSettings) {
		return true
	}
	writeError(w, http.StatusForbidden, "insufficient permissions")
	return false
}

func validChannelType(t models.ChannelType) bool {
	switch t {
	case models.ChannelDiscord, models.ChannelWebhook, models.ChannelEmail:
		return true
	}
	return false
}

// validateChannelConfig checks the minimum required keys are present for a type.
func validateChannelConfig(t models.ChannelType, cfg map[string]string) string {
	switch t {
	case models.ChannelDiscord, models.ChannelWebhook:
		if strings.TrimSpace(cfg["url"]) == "" {
			return "url is required"
		}
	case models.ChannelEmail:
		if strings.TrimSpace(cfg["host"]) == "" || strings.TrimSpace(cfg["from"]) == "" || strings.TrimSpace(cfg["to"]) == "" {
			return "host, from and to are required"
		}
	}
	return ""
}

func (s *Server) handleCreateChannel(w http.ResponseWriter, r *http.Request) {
	var req channelRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid body")
		return
	}
	if !validChannelType(req.Type) {
		writeError(w, http.StatusBadRequest, "unknown channel type")
		return
	}
	if !s.authorizeChannelScope(w, r, req.ServerID) {
		return
	}
	if req.Config == nil {
		req.Config = map[string]string{}
	}
	if msg := validateChannelConfig(req.Type, req.Config); msg != "" {
		writeError(w, http.StatusBadRequest, msg)
		return
	}
	c := &models.NotificationChannel{
		Name: strings.TrimSpace(req.Name), Type: req.Type, Enabled: req.Enabled,
		ServerID: req.ServerID, Events: cleanEvents(req.Events),
	}
	if err := s.Store.CreateChannel(c, req.Config); err != nil {
		writeError(w, http.StatusInternalServerError, "could not create channel")
		return
	}
	s.audit(r, req.ServerID, "notification.create", c.Name+" ("+string(c.Type)+")")
	writeJSON(w, http.StatusCreated, s.maskChannel(c))
}

func (s *Server) handleListChannels(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	cs, err := s.Store.ListChannels()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not list channels")
		return
	}
	out := make([]channelDTO, 0, len(cs))
	for i := range cs {
		out = append(out, s.maskChannel(&cs[i]))
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) handleListServerChannels(w http.ResponseWriter, r *http.Request) {
	srv, ok := s.requireServer(w, r, models.PermSettings)
	if !ok {
		return
	}
	cs, err := s.Store.ListChannelsForServer(srv.ID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not list channels")
		return
	}
	out := make([]channelDTO, 0, len(cs))
	for i := range cs {
		out = append(out, s.maskChannel(&cs[i]))
	}
	writeJSON(w, http.StatusOK, out)
}

// lookupChannel loads the channel in the path and authorizes by its scope.
func (s *Server) lookupChannel(w http.ResponseWriter, r *http.Request) (*models.NotificationChannel, bool) {
	id, err := strconv.ParseUint(strings.TrimSpace(r.PathValue("nid")), 10, 64)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid id")
		return nil, false
	}
	c, err := s.Store.GetChannel(uint(id))
	if err != nil {
		writeError(w, http.StatusNotFound, "channel not found")
		return nil, false
	}
	if !s.authorizeChannelScope(w, r, c.ServerID) {
		return nil, false
	}
	return c, true
}

func (s *Server) handleGetChannel(w http.ResponseWriter, r *http.Request) {
	c, ok := s.lookupChannel(w, r)
	if !ok {
		return
	}
	writeJSON(w, http.StatusOK, s.maskChannel(c))
}

func (s *Server) handleUpdateChannel(w http.ResponseWriter, r *http.Request) {
	c, ok := s.lookupChannel(w, r)
	if !ok {
		return
	}
	var req channelRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid body")
		return
	}
	// Scope (serverId) and type are immutable; recreate to change them.
	c.Name = strings.TrimSpace(req.Name)
	c.Enabled = req.Enabled
	c.Events = cleanEvents(req.Events)

	// Merge config: keep stored secrets unless a new non-empty value is supplied;
	// non-secret keys are taken from the request when provided.
	var merged map[string]string
	if req.Config != nil {
		existing, err := s.Store.ChannelConfig(c)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "could not read channel")
			return
		}
		secretKeys := map[string]bool{}
		for _, k := range models.SecretConfigKeys[c.Type] {
			secretKeys[k] = true
		}
		merged = map[string]string{}
		for k, v := range existing {
			merged[k] = v
		}
		for k, v := range req.Config {
			if secretKeys[k] && strings.TrimSpace(v) == "" {
				continue // blank secret => keep existing
			}
			merged[k] = v
		}
		if msg := validateChannelConfig(c.Type, merged); msg != "" {
			writeError(w, http.StatusBadRequest, msg)
			return
		}
	}
	if err := s.Store.UpdateChannel(c, merged); err != nil {
		writeError(w, http.StatusInternalServerError, "could not update channel")
		return
	}
	s.audit(r, c.ServerID, "notification.update", c.Name)
	updated, _ := s.Store.GetChannel(c.ID)
	writeJSON(w, http.StatusOK, s.maskChannel(updated))
}

func (s *Server) handleDeleteChannel(w http.ResponseWriter, r *http.Request) {
	c, ok := s.lookupChannel(w, r)
	if !ok {
		return
	}
	if err := s.Store.DeleteChannel(c.ID); err != nil {
		writeError(w, http.StatusInternalServerError, "could not delete channel")
		return
	}
	s.audit(r, c.ServerID, "notification.delete", c.Name)
	w.WriteHeader(http.StatusNoContent)
}

// handleTestChannel delivers a synthetic event to a single channel so operators
// can verify configuration. It uses the live (decrypted) config, not the request.
func (s *Server) handleTestChannel(w http.ResponseWriter, r *http.Request) {
	c, ok := s.lookupChannel(w, r)
	if !ok {
		return
	}
	if s.Dispatch == nil {
		writeError(w, http.StatusServiceUnavailable, "notifications are not enabled")
		return
	}
	cfg, err := s.Store.ChannelConfig(c)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not read channel")
		return
	}
	ev := models.Event{
		Type: "test", ServerID: c.ServerID, Message: "Quetzal test notification",
		CreatedAt: time.Now(),
	}
	if err := s.Dispatch.DeliverTo(r.Context(), c, cfg, ev); err != nil {
		writeError(w, http.StatusBadGateway, "delivery failed: "+err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// ---- Events feed ----

func (s *Server) handleServerEvents(w http.ResponseWriter, r *http.Request) {
	srv, ok := s.requireServer(w, r, models.PermView)
	if !ok {
		return
	}
	es, err := s.Store.ListEventsForServer(srv.ID, eventLimit(r))
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not list events")
		return
	}
	writeJSON(w, http.StatusOK, es)
}

func (s *Server) handleGlobalEvents(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	es, err := s.Store.ListEvents(eventLimit(r))
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not list events")
		return
	}
	writeJSON(w, http.StatusOK, es)
}

func eventLimit(r *http.Request) int {
	if n, err := strconv.Atoi(r.URL.Query().Get("limit")); err == nil && n > 0 && n <= 500 {
		return n
	}
	return 100
}

// cleanEvents trims and drops empty event-type filters.
func cleanEvents(in []string) []string {
	var out []string
	for _, e := range in {
		if e = strings.TrimSpace(e); e != "" {
			out = append(out, e)
		}
	}
	return out
}
