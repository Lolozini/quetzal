package api

import (
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/lolozini/quetzal/internal/egg"
	"github.com/lolozini/quetzal/internal/models"
)

// handleImportEggURL fetches an egg JSON from a URL and imports it as a template
// (admin), through the SSRF-guarded fetch.
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
	data, err := s.Fetch(r.Context(), rawFileURL(req.URL), maxTemplateBody)
	if err != nil {
		writeError(w, http.StatusBadGateway, "could not fetch egg: "+err.Error())
		return
	}
	t, err := egg.ToTemplate(data)
	if err != nil {
		writeError(w, http.StatusBadRequest, eggParseError(data, err))
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

// rawFileURL rewrites a repository *page* URL into the raw-file URL that serves
// the JSON, so pasting the link straight from the browser works. GitHub and
// GitLab blob pages return HTML, which would otherwise fail to parse as an egg.
// Anything else is returned unchanged.
func rawFileURL(raw string) string {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Host == "" {
		return raw
	}
	switch {
	case u.Host == "github.com" || u.Host == "www.github.com":
		// /owner/repo/blob/<ref>/<path> -> raw.githubusercontent.com/owner/repo/<ref>/<path>
		parts := strings.Split(strings.TrimPrefix(u.Path, "/"), "/")
		if len(parts) >= 5 && parts[2] == "blob" {
			u.Host = "raw.githubusercontent.com"
			u.Path = "/" + parts[0] + "/" + parts[1] + "/" + strings.Join(parts[3:], "/")
			u.RawQuery = ""
			return u.String()
		}
	case strings.Contains(u.Path, "/-/blob/"):
		// GitLab: /group/project/-/blob/<ref>/<path> -> .../-/raw/<ref>/<path>
		u.Path = strings.Replace(u.Path, "/-/blob/", "/-/raw/", 1)
		u.RawQuery = ""
		return u.String()
	}
	return raw
}

// eggParseError turns a parse failure into a message that says what to do about
// it. The common mistake is feeding a web page (a repository blob page, or an
// error/login page) instead of the raw egg JSON, which reads as a cryptic
// "invalid character '<'".
func eggParseError(data []byte, err error) string {
	if looksLikeHTML(data) {
		return "that looks like a web page, not an egg: use the raw JSON file (on GitHub, the \"Raw\" button)"
	}
	return "invalid egg: " + err.Error()
}

// looksLikeHTML reports whether the payload starts like an HTML document.
func looksLikeHTML(data []byte) bool {
	s := strings.ToLower(strings.TrimSpace(string(data)))
	if len(s) > 512 {
		s = s[:512]
	}
	return strings.HasPrefix(s, "<!doctype html") || strings.HasPrefix(s, "<html") || strings.HasPrefix(s, "<!--") && strings.Contains(s, "<html")
}
