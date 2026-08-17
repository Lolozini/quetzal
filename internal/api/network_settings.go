package api

import (
	"context"
	"net/http"
	"strings"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"

	"github.com/lolozini/quetzal/internal/models"
	"github.com/lolozini/quetzal/internal/reconciler"
	"github.com/lolozini/quetzal/internal/store"
)

// handleGetNetworkSettings returns the published endpoint host (admin only)
// along with the detected node address, shown as a hint so the admin knows what
// their DNS record should point at.
func (s *Server) handleGetNetworkSettings(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdminPerm(w, r, models.AdminPermSettings) {
		return
	}
	host, _ := s.Store.GetSetting(store.SettingEndpointHost)
	writeJSON(w, http.StatusOK, map[string]any{
		"endpointHost": host,
		"nodeAddress":  s.detectedNodeAddress(r),
	})
}

type networkSettingsRequest struct {
	EndpointHost string `json:"endpointHost"`
}

// handleSetNetworkSettings updates the published endpoint host (admin only). A
// blank value clears it, falling back to the raw node address in endpoints.
func (s *Server) handleSetNetworkSettings(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdminPerm(w, r, models.AdminPermSettings) {
		return
	}
	var req networkSettingsRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid body")
		return
	}
	host := strings.TrimSpace(req.EndpointHost)
	if err := s.Store.SetSetting(store.SettingEndpointHost, host); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.audit(r, 0, "network.settings.update", host)
	w.WriteHeader(http.StatusNoContent)
}

// detectedNodeAddress returns a best-effort local node address (ExternalIP,
// else InternalIP) for display as a hint. Empty on any failure — it never
// blocks the settings page.
func (s *Server) detectedNodeAddress(r *http.Request) string {
	return nodeAddress(r.Context(), s.Clientset)
}

// nodeAddress lists a cluster's nodes and picks the address to reach it on,
// returning "" on any failure. The preference order lives in the reconciler so
// the panel and the controller can't drift apart.
func nodeAddress(ctx context.Context, cs kubernetes.Interface) string {
	if cs == nil {
		return ""
	}
	nl, err := cs.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
	if err != nil {
		return ""
	}
	return reconciler.NodeAddress(nl.Items)
}

// nodeAddrTTL bounds how long a looked-up node address is reused. Node IPs
// essentially never change, and the SFTP panel polls every couple of seconds
// while a port is being provisioned, so without this every poll would list the
// whole cluster.
const nodeAddrTTL = 5 * time.Minute

// cachedNodeAddress is nodeAddress with a short per-cluster cache.
func (s *Server) cachedNodeAddress(ctx context.Context, cs kubernetes.Interface, clusterID uint) string {
	s.nodeAddrMu.Lock()
	if e, ok := s.nodeAddr[clusterID]; ok && time.Since(e.at) < nodeAddrTTL {
		s.nodeAddrMu.Unlock()
		return e.addr
	}
	s.nodeAddrMu.Unlock()

	addr := nodeAddress(ctx, cs)
	if addr == "" {
		return "" // don't cache a failure: the next call should retry
	}
	s.nodeAddrMu.Lock()
	if s.nodeAddr == nil {
		s.nodeAddr = map[uint]nodeAddrEntry{}
	}
	s.nodeAddr[clusterID] = nodeAddrEntry{addr: addr, at: time.Now()}
	s.nodeAddrMu.Unlock()
	return addr
}

// endpointHost is the host to advertise in a server's external endpoints: the
// cluster's own DNS name, else the panel-wide one, else that cluster's node
// address. Mirrors the controller's endpoint computation so the SFTP string and
// the game endpoint agree.
func (s *Server) endpointHost(ctx context.Context, cs kubernetes.Interface, clusterID uint) string {
	if h := store.EndpointHostFor(s.Store, clusterID); h != "" {
		return h
	}
	return s.cachedNodeAddress(ctx, cs, clusterID)
}
