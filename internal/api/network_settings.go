package api

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"strings"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/validation"
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
	if err := checkEndpointHost(host); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := s.Store.SetSetting(store.SettingEndpointHost, host); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.audit(r, 0, "network.settings.update", host)
	w.WriteHeader(http.StatusNoContent)
}

// checkEndpointHost refuses a published host players could not connect to. It
// goes in front of ":<port>" in every address the panel shows, and anything was
// taken: "bad host!" became "bad host!:30158" on every server. A DNS name or an
// IP address, without scheme or port; blank clears the setting.
func checkEndpointHost(host string) error {
	if host == "" || net.ParseIP(host) != nil {
		return nil
	}
	name := strings.TrimSuffix(host, ".")
	ok := name != "" && len(name) <= 253
	for _, label := range strings.Split(name, ".") {
		ok = ok && len(validation.IsDNS1123Label(strings.ToLower(label))) == 0
	}
	if !ok {
		return fmt.Errorf("%q is not a host name or an IP address: give the name or address players connect to, without scheme or port (play.example.com)", host)
	}
	return nil
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
