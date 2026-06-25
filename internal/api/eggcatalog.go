package api

import (
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/lolozini/quetzal/internal/egg"
	"github.com/lolozini/quetzal/internal/models"
	"github.com/lolozini/quetzal/internal/store"
)

// maxCatalogBody caps a fetched catalog manifest.
const maxCatalogBody = 1 << 20 // 1 MiB

// CatalogEntry is one installable egg in a catalog manifest.
type CatalogEntry struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	Category    string `json:"category,omitempty"`
	Author      string `json:"author,omitempty"`
	URL         string `json:"url"`
}

// handleImportEggURL fetches an egg JSON from a URL and imports it as a template
// (admin). The same SSRF-guarded fetch backs both ad-hoc imports and catalog
// installs.
func (s *Server) handleImportEggURL(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdminPerm(w, r, models.AdminPermTemplates) {
		return
	}
	var req struct {
		URL string `json:"url"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 4096)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if strings.TrimSpace(req.URL) == "" {
		writeError(w, http.StatusBadRequest, "url is required")
		return
	}
	data, err := s.Fetch(r.Context(), req.URL, maxTemplateBody)
	if err != nil {
		writeError(w, http.StatusBadGateway, "could not fetch egg: "+err.Error())
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
	s.audit(r, 0, "template.import-url", saved.Slug)
	writeJSON(w, http.StatusCreated, saved)
}

// handleGetEggCatalog returns the configured catalog URL and, if set, the eggs
// listed in its manifest (fetched with the SSRF-guarded client). A manifest is
// either a bare JSON array of entries or {"eggs":[...]}.
func (s *Server) handleGetEggCatalog(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdminPerm(w, r, models.AdminPermTemplates) {
		return
	}
	catalogURL, _ := s.Store.GetSetting(store.SettingEggCatalogURL)
	resp := struct {
		CatalogURL string         `json:"catalogUrl"`
		Eggs       []CatalogEntry `json:"eggs"`
		Error      string         `json:"error,omitempty"`
	}{CatalogURL: catalogURL, Eggs: []CatalogEntry{}}

	// Always return the configured URL (and 200) so the admin can see/fix it; a
	// fetch/parse failure is reported inline rather than as an HTTP error.
	if strings.TrimSpace(catalogURL) != "" {
		if data, err := s.Fetch(r.Context(), catalogURL, maxCatalogBody); err != nil {
			resp.Error = "could not fetch catalog: " + err.Error()
		} else if eggs, err := parseCatalog(data); err != nil {
			resp.Error = "invalid catalog manifest: " + err.Error()
		} else {
			resp.Eggs = eggs
		}
	}
	writeJSON(w, http.StatusOK, resp)
}

// handleSetEggCatalog sets the catalog manifest URL (admin).
func (s *Server) handleSetEggCatalog(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdminPerm(w, r, models.AdminPermTemplates) {
		return
	}
	var req struct {
		URL string `json:"url"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 4096)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	req.URL = strings.TrimSpace(req.URL)
	if req.URL != "" {
		if u, err := url.Parse(req.URL); err != nil || (u.Scheme != "http" && u.Scheme != "https") {
			writeError(w, http.StatusBadRequest, "url must be http or https")
			return
		}
	}
	if err := s.Store.SetSetting(store.SettingEggCatalogURL, req.URL); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.audit(r, 0, "egg-catalog.set", req.URL)
	w.WriteHeader(http.StatusNoContent)
}

// parseCatalog accepts a bare JSON array of entries or {"eggs":[...]} and returns
// the entries that have a name and a URL.
func parseCatalog(data []byte) ([]CatalogEntry, error) {
	var wrapped struct {
		Eggs []CatalogEntry `json:"eggs"`
	}
	var entries []CatalogEntry
	if err := json.Unmarshal(data, &wrapped); err == nil && wrapped.Eggs != nil {
		entries = wrapped.Eggs
	} else if err := json.Unmarshal(data, &entries); err != nil {
		return nil, err
	}
	out := make([]CatalogEntry, 0, len(entries))
	for _, e := range entries {
		if strings.TrimSpace(e.Name) != "" && strings.TrimSpace(e.URL) != "" {
			out = append(out, e)
		}
	}
	return out, nil
}
