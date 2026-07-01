package api

import (
	"context"
	"net/http"
	"strings"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"

	"github.com/lolozini/quetzal/internal/models"
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

// nodeAddress picks a usable node address (ExternalIP, else InternalIP) from a
// clientset, returning "" on any failure. Shared by the settings hint and the
// SFTP connection string.
func nodeAddress(ctx context.Context, cs kubernetes.Interface) string {
	if cs == nil {
		return ""
	}
	nl, err := cs.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
	if err != nil {
		return ""
	}
	var internal string
	for i := range nl.Items {
		for _, a := range nl.Items[i].Status.Addresses {
			switch a.Type {
			case corev1.NodeExternalIP:
				if a.Address != "" {
					return a.Address
				}
			case corev1.NodeInternalIP:
				if internal == "" {
					internal = a.Address
				}
			}
		}
	}
	return internal
}

// endpointHost is the host to advertise in a server's external endpoints: the
// admin-configured DNS name when set, otherwise the given cluster's node
// address. Mirrors the controller's endpoint computation so the SFTP string and
// the game endpoint agree.
func (s *Server) endpointHost(ctx context.Context, cs kubernetes.Interface) string {
	if h, _ := s.Store.GetSetting(store.SettingEndpointHost); strings.TrimSpace(h) != "" {
		return strings.TrimSpace(h)
	}
	return nodeAddress(ctx, cs)
}
