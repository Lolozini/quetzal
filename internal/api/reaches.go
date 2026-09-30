package api

import (
	"fmt"
	"net/http"
	"strings"

	"github.com/lolozini/quetzal/internal/models"
)

// maxReaches bounds the servers one server may reach: a proxy in front of a
// network of them needs a handful, not hundreds.
const maxReaches = 32

// checkReaches validates the servers srv is to reach (models.Server.Reaches)
// and returns their slugs, deduplicated.
//
// A game server cannot reach another inside the cluster, so a proxy such as
// Velocity could only be joined to its servers through the internet, where the
// servers behind it, which trust it to have checked the players, could be
// joined directly. Letting one server reach another opens the second to code
// running in the first, so the caller must be able to change both, and they
// must share a cluster: a NetworkPolicy does not cross one.
func (s *Server) checkReaches(r *http.Request, srv *models.Server, want []string) ([]string, *httpErr) {
	u := userFrom(r.Context())
	seen := map[string]bool{}
	slugs := make([]string, 0, len(want))
	for _, slug := range want {
		slug = strings.TrimSpace(slug)
		if slug == "" || seen[slug] {
			continue
		}
		seen[slug] = true
		if slug == srv.Slug {
			return nil, &httpErr{http.StatusBadRequest, "a server does not need to reach itself"}
		}
		target, err := s.Store.GetServerBySlug(slug)
		if err != nil || !s.can(u, target, models.PermView) {
			return nil, &httpErr{http.StatusBadRequest, fmt.Sprintf("no server %q", slug)}
		}
		if !s.can(u, target, models.PermSettings) {
			return nil, &httpErr{http.StatusForbidden, fmt.Sprintf("you cannot change the settings of %q, so you cannot open it to another server", slug)}
		}
		if target.ClusterID != srv.ClusterID {
			return nil, &httpErr{http.StatusBadRequest, fmt.Sprintf("%q runs on another cluster; servers reach each other only inside one", slug)}
		}
		slugs = append(slugs, slug)
	}
	if len(slugs) > maxReaches {
		return nil, &httpErr{http.StatusBadRequest, fmt.Sprintf("a server may reach at most %d others", maxReaches)}
	}
	return slugs, nil
}
