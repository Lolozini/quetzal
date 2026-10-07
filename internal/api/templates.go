package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"path"
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
		t.SuggestedPorts = models.DetectPorts(t)
		t.AllocatedPort = t.UsesAllocation()
	}
	t.EffectiveKeep = t.EffectiveReinstallKeep()
	writeJSON(w, http.StatusOK, t)
}

// handleImportEgg imports a Pterodactyl/Pelican egg JSON as a template (admin).
// The request body is the raw egg JSON. An egg whose name slugifies to an
// existing template's slug is refused unless the request says what to do: see
// saveImport.
func (s *Server) handleImportEgg(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdminPerm(w, r, models.AdminPermTemplates) {
		return
	}
	data, err := io.ReadAll(io.LimitReader(r.Body, maxTemplateBody))
	if err != nil {
		writeError(w, http.StatusBadRequest, "could not read body")
		return
	}
	t, err := egg.Parse(data)
	if err != nil {
		writeError(w, http.StatusBadRequest, eggParseError(data, err))
		return
	}
	s.saveImport(w, r, t, "template.import")
}

// saveImport saves an imported template. An import whose slug another template
// already had replaced that template without a word, a different egg sharing
// its name included: Pterodactyl's Paper imported over Pelican's took Java 25
// from the servers created afterwards. Such an import is now refused with a
// 409 that says what is there, unless the request asks, with ifExists=replace
// to update that template (bumping its version), or with ifExists=copy to add
// this one beside it under the next free slug (paper-2, "Paper (2)").
func (s *Server) saveImport(w http.ResponseWriter, r *http.Request, t *models.Template, action string) {
	ifExists := r.URL.Query().Get("ifExists")
	if ifExists != "" && ifExists != "replace" && ifExists != "copy" {
		writeError(w, http.StatusBadRequest, `ifExists must be "replace" or "copy"`)
		return
	}
	detail := t.Slug
	existing, err := s.Store.GetTemplateBySlug(t.Slug)
	switch {
	case errors.Is(err, store.ErrNotFound):
	case err != nil:
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	case ifExists == "replace":
		detail += fmt.Sprintf(" (replaced version %d)", existing.Version)
	case ifExists == "copy":
		if !s.copySlug(t) {
			writeError(w, http.StatusConflict, "no free slug for a copy of "+t.Slug)
			return
		}
		detail = t.Slug + " (copy of " + existing.Slug + ")"
	default:
		n, _ := s.Store.CountServersByTemplate(existing.ID)
		writeJSON(w, http.StatusConflict, map[string]any{
			"error": fmt.Sprintf("a template %q (%s, version %d) already exists, used by %d server(s): replace it (a running server keeps the version it started with until it restarts), or import this one as a new template",
				existing.Name, existing.Slug, existing.Version, n),
			"existing": map[string]any{"slug": existing.Slug, "name": existing.Name, "version": existing.Version, "servers": n},
		})
		return
	}
	saved, err := s.Store.UpsertTemplate(t)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.audit(r, 0, action, detail)
	writeJSON(w, http.StatusCreated, saved)
}

// copySlug gives an imported template the first free slug after its own, and
// numbers its name the same way.
func (s *Server) copySlug(t *models.Template) bool {
	for i := 2; i < 100; i++ {
		slug := fmt.Sprintf("%s-%d", t.Slug, i)
		if _, err := s.Store.GetTemplateBySlug(slug); errors.Is(err, store.ErrNotFound) {
			t.Slug, t.Name = slug, fmt.Sprintf("%s (%d)", t.Name, i)
			return true
		}
	}
	return false
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
	// The data path is where the volume mounts and where the file manager is
	// confined. A relative one, or the container root, gives a server that cannot
	// start and a file manager rooted somewhere it should not be.
	if p := t.DataPath; p != "" {
		if !strings.HasPrefix(p, "/") || path.Clean(p) != p || p == "/" {
			writeError(w, http.StatusBadRequest,
				`dataPath must be an absolute, already-clean directory other than "/" (e.g. /home/container)`)
			return
		}
	}
	if !models.ValidWakeProtocol(t.WakeProtocol) {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("wakeProtocol must be %q, %q or empty", models.WakeAnyConnection, models.WakeMinecraft))
		return
	}
	keep, err := models.CleanKeepPaths(t.ReinstallKeep)
	if err != nil {
		writeError(w, http.StatusBadRequest, "reinstallKeep: "+err.Error())
		return
	}
	t.ReinstallKeep = keep
	t.EffectiveKeep = nil // computed for the panel, never stored
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
// use it. Deletion is permanent: templates come from egg imports, so nothing
// re-creates a deleted one.
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
