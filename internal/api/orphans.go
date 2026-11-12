package api

import (
	"net/http"
	"time"

	"github.com/lolozini/quetzal/internal/models"
)

// orphanNamespaces is one cluster's namespaces that have no server in the
// database (models.OrphanReport), with the cluster's name.
type orphanNamespaces struct {
	ClusterID    uint      `json:"clusterId"`
	Cluster      string    `json:"cluster"`
	Since        time.Time `json:"since"`
	Names        []string  `json:"names"`
	Managed      int       `json:"managed"`
	Refused      bool      `json:"refused"`
	GraceSeconds int64     `json:"graceSeconds"`
}

// handleListOrphanNamespaces serves what the controllers found, for the banner
// an administrator sees when a restored database is about to take servers with
// it. Empty when nothing is wrong. A report of a cluster that was removed since
// is not shown.
func (s *Server) handleListOrphanNamespaces(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdminPerm(w, r, models.AdminPermServers) {
		return
	}
	reps, err := s.Store.ListOrphanReports()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	clusters, err := s.Store.ListClusters()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	names := make(map[uint]string, len(clusters))
	for _, c := range clusters {
		names[c.ID] = c.Name
		if c.Name == "" {
			names[c.ID] = c.Slug
		}
	}
	out := make([]orphanNamespaces, 0, len(reps))
	for _, rep := range reps {
		name, ok := names[rep.ClusterID]
		if !ok {
			continue
		}
		out = append(out, orphanNamespaces{
			ClusterID: rep.ClusterID, Cluster: name, Since: rep.Since, Names: rep.Names,
			Managed: rep.Managed, Refused: rep.Refused, GraceSeconds: rep.GraceSeconds,
		})
	}
	writeJSON(w, http.StatusOK, out)
}
