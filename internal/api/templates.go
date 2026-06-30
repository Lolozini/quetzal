package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/lolozini/quetzal/internal/egg"
	"github.com/lolozini/quetzal/internal/models"
	"github.com/lolozini/quetzal/internal/store"
)

// maxTemplateBody caps an uploaded egg/template (install scripts can be large).
const maxTemplateBody = 2 << 20 // 2 MiB

// handleGetTemplate returns one template by slug (any authenticated user, like
// the list — used by the create and edit screens).
func (s *Server) handleGetTemplate(w http.ResponseWriter, r *http.Request) {
	t, ok := s.lookupTemplate(w, r)
	if !ok {
		return
	}
	// Pre-fill hint for the create form: when the template declares no ports
	// (imported eggs), suggest ports inferred from its port-like variables.
	if len(t.Ports) == 0 {
		t.SuggestedPorts = models.DetectPorts(t.Variables)
	}
	writeJSON(w, http.StatusOK, t)
}

// handleImportEgg imports a Pterodactyl/Pelican egg JSON as a template (admin).
// The request body is the raw egg JSON. Importing an egg whose name slugifies to
// an existing template updates it (and bumps its version).
func (s *Server) handleImportEgg(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdminPerm(w, r, models.AdminPermTemplates) {
		return
	}
	data, err := io.ReadAll(io.LimitReader(r.Body, maxTemplateBody))
	if err != nil {
		writeError(w, http.StatusBadRequest, "could not read body")
		return
	}
	t, err := egg.ToTemplate(data)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid egg: "+err.Error())
		return
	}
	saved, err := s.Store.UpsertTemplate(t)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.audit(r, 0, "template.import", saved.Slug)
	writeJSON(w, http.StatusCreated, saved)
}

// handleUpdateTemplate replaces a template from native Quetzal template JSON
// (admin) — the round-trip companion of the export endpoint. The slug is taken
// from the path and never changed (servers reference the template by ID, and a
// silent rename would be confusing).
func (s *Server) handleUpdateTemplate(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdminPerm(w, r, models.AdminPermTemplates) {
		return
	}
	existing, ok := s.lookupTemplate(w, r)
	if !ok {
		return
	}
	data, err := io.ReadAll(io.LimitReader(r.Body, maxTemplateBody))
	if err != nil {
		writeError(w, http.StatusBadRequest, "could not read body")
		return
	}
	var t models.Template
	if err := json.Unmarshal(data, &t); err != nil {
		writeError(w, http.StatusBadRequest, "invalid template JSON: "+err.Error())
		return
	}
	if strings.TrimSpace(t.Name) == "" {
		writeError(w, http.StatusBadRequest, "template name is required")
		return
	}
	// Pin identity + creation time to the existing row (Save writes every column,
	// so a hand-edited body that omits createdAt would otherwise zero it);
	// everything else comes from the payload.
	t.ID = existing.ID
	t.Slug = existing.Slug
	t.CreatedAt = existing.CreatedAt
	saved, err := s.Store.UpsertTemplate(&t)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.audit(r, 0, "template.update", saved.Slug)
	writeJSON(w, http.StatusOK, saved)
}

// handleExportTemplate streams a template as native JSON for backup/sharing
// (admin). It round-trips with PUT /api/templates/{slug}.
func (s *Server) handleExportTemplate(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdminPerm(w, r, models.AdminPermTemplates) {
		return
	}
	t, ok := s.lookupTemplate(w, r)
	if !ok {
		return
	}
	body, err := json.MarshalIndent(t, "", "  ")
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", t.Slug+".json"))
	_, _ = w.Write(body)
}

// handleDeleteTemplate removes a template (admin), refusing while servers still
// use it. Built-in templates are re-seeded on the next controller/apiserver
// start, so deleting one only hides it until restart.
func (s *Server) handleDeleteTemplate(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdminPerm(w, r, models.AdminPermTemplates) {
		return
	}
	t, ok := s.lookupTemplate(w, r)
	if !ok {
		return
	}
	if n, _ := s.Store.CountServersByTemplate(t.ID); n > 0 {
		writeError(w, http.StatusConflict, fmt.Sprintf("%d server(s) still use this template", n))
		return
	}
	if err := s.Store.DeleteTemplate(t.ID); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.audit(r, 0, "template.delete", t.Slug)
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) lookupTemplate(w http.ResponseWriter, r *http.Request) (*models.Template, bool) {
	slug := strings.TrimSpace(r.PathValue("slug"))
	t, err := s.Store.GetTemplateBySlug(slug)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, http.StatusNotFound, "template not found")
		} else {
			writeError(w, http.StatusInternalServerError, err.Error())
		}
		return nil, false
	}
	return t, true
}
