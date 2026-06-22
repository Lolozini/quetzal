package api

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/lolozini/quetzal/internal/cluster"
	"github.com/lolozini/quetzal/internal/egg"
	"github.com/lolozini/quetzal/internal/models"
	"github.com/lolozini/quetzal/internal/store"
)

// handleListClusters returns the registered clusters. Any authenticated user may
// list them (to pick a target when creating a server); credentials are never
// included (KubeconfigEnc is json:"-").
func (s *Server) handleListClusters(w http.ResponseWriter, r *http.Request) {
	cs, err := s.Store.ListClusters()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, cs)
}

type clusterRequest struct {
	Name       string `json:"name"`
	Kubeconfig string `json:"kubeconfig"` // optional on update
}

func (s *Server) handleCreateCluster(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	var req clusterRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid body")
		return
	}
	req.Name = strings.TrimSpace(req.Name)
	if req.Name == "" || strings.TrimSpace(req.Kubeconfig) == "" {
		writeError(w, http.StatusBadRequest, "name and kubeconfig are required")
		return
	}
	clients, err := cluster.Build(req.Kubeconfig)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid kubeconfig: "+err.Error())
		return
	}
	slug := egg.Slugify(req.Name)
	if slug == "" {
		writeError(w, http.StatusBadRequest, "name produces an empty slug")
		return
	}
	if _, err := s.Store.GetClusterBySlug(slug); err == nil {
		writeError(w, http.StatusConflict, "a cluster with this name already exists")
		return
	}
	c := &models.Cluster{Slug: slug, Name: req.Name}
	if err := s.Store.CreateCluster(c, req.Kubeconfig); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	// Record an initial connectivity probe so the UI shows status immediately.
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	version, nodes, perr := cluster.Probe(ctx, clients)
	msg := ""
	if perr != nil {
		msg = perr.Error()
	}
	_ = s.Store.SetClusterStatus(c.ID, perr == nil, version, nodes, msg)
	c, _ = s.Store.GetCluster(c.ID)
	s.audit(r, 0, "cluster.create", c.Slug)
	writeJSON(w, http.StatusCreated, c)
}

func (s *Server) handleUpdateCluster(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	c, ok := s.lookupCluster(w, r)
	if !ok {
		return
	}
	var req clusterRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid body")
		return
	}
	name := strings.TrimSpace(req.Name)
	if name == "" {
		name = c.Name
	}
	if req.Kubeconfig != "" {
		if c.InCluster {
			writeError(w, http.StatusBadRequest, "the local cluster has no stored kubeconfig")
			return
		}
		if _, err := cluster.Build(req.Kubeconfig); err != nil {
			writeError(w, http.StatusBadRequest, "invalid kubeconfig: "+err.Error())
			return
		}
	}
	if err := s.Store.UpdateCluster(c.ID, name, req.Kubeconfig); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.audit(r, 0, "cluster.update", c.Slug)
	updated, _ := s.Store.GetCluster(c.ID)
	writeJSON(w, http.StatusOK, updated)
}

func (s *Server) handleDeleteCluster(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	c, ok := s.lookupCluster(w, r)
	if !ok {
		return
	}
	if c.InCluster {
		writeError(w, http.StatusBadRequest, "cannot delete the local cluster")
		return
	}
	n, err := s.Store.CountServersByCluster(c.ID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if n > 0 {
		writeError(w, http.StatusConflict, "cluster still has servers; delete or move them first")
		return
	}
	if err := s.Store.DeleteCluster(c.ID); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.audit(r, 0, "cluster.delete", c.Slug)
	w.WriteHeader(http.StatusNoContent)
}

// handleTestCluster probes a cluster's connectivity and records the result.
func (s *Server) handleTestCluster(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	c, ok := s.lookupCluster(w, r)
	if !ok {
		return
	}
	clients, err := s.Registry.For(c.ID)
	if err != nil {
		_ = s.Store.SetClusterStatus(c.ID, false, "", 0, err.Error())
		updated, _ := s.Store.GetCluster(c.ID)
		writeJSON(w, http.StatusOK, updated)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	version, nodes, perr := cluster.Probe(ctx, clients)
	msg := ""
	if perr != nil {
		msg = perr.Error()
	}
	_ = s.Store.SetClusterStatus(c.ID, perr == nil, version, nodes, msg)
	updated, _ := s.Store.GetCluster(c.ID)
	writeJSON(w, http.StatusOK, updated)
}

type nodeInfo struct {
	Name     string `json:"name"`
	Ready    bool   `json:"ready"`
	Version  string `json:"version"`
	OS       string `json:"os"`
	CPU      string `json:"cpu"`
	Memory   string `json:"memory"`
	Internal string `json:"internalIP,omitempty"`
}

// handleClusterNodes lists a cluster's Kubernetes nodes (the "nodes" concept):
// capacity and readiness, read-only.
func (s *Server) handleClusterNodes(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	c, ok := s.lookupCluster(w, r)
	if !ok {
		return
	}
	clients, err := s.Registry.For(c.ID)
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, "cluster unavailable: "+err.Error())
		return
	}
	nl, err := clients.Clientset.CoreV1().Nodes().List(r.Context(), metav1.ListOptions{})
	if err != nil {
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	out := make([]nodeInfo, 0, len(nl.Items))
	for i := range nl.Items {
		n := &nl.Items[i]
		info := nodeInfo{
			Name:    n.Name,
			Version: n.Status.NodeInfo.KubeletVersion,
			OS:      n.Status.NodeInfo.OSImage,
			CPU:     n.Status.Capacity.Cpu().String(),
			Memory:  n.Status.Capacity.Memory().String(),
		}
		for _, cond := range n.Status.Conditions {
			if cond.Type == corev1.NodeReady {
				info.Ready = cond.Status == corev1.ConditionTrue
			}
		}
		for _, a := range n.Status.Addresses {
			if a.Type == corev1.NodeInternalIP {
				info.Internal = a.Address
			}
		}
		out = append(out, info)
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) lookupCluster(w http.ResponseWriter, r *http.Request) (*models.Cluster, bool) {
	id, err := strconv.ParseUint(strings.TrimSpace(r.PathValue("cid")), 10, 64)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid cluster id")
		return nil, false
	}
	c, err := s.Store.GetCluster(uint(id))
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, http.StatusNotFound, "cluster not found")
		} else {
			writeError(w, http.StatusInternalServerError, err.Error())
		}
		return nil, false
	}
	return c, true
}
