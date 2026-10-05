//go:build e2e

package e2e

import (
	"bufio"
	"net"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/lolozini/quetzal/internal/models"
	"github.com/lolozini/quetzal/internal/reconciler"
)

// TestE2ENodePortAddressAnswers publishes a server on a node port, as players
// reach it by default (keeping their address), and connects to the address the
// panel shows. On a cluster of several nodes that address used to be the first
// node's, which does not answer for a pod running elsewhere. It needs such a
// cluster and is skipped on one node, CI's among them.
func TestE2ENodePortAddressAnswers(t *testing.T) {
	ctx, c, st, rec := setup(t)
	var nodes corev1.NodeList
	if err := c.List(ctx, &nodes); err != nil {
		t.Fatalf("nodes: %v", err)
	}
	if len(nodes.Items) < 2 {
		t.Skip("needs a cluster with several nodes")
	}

	tmpl := &models.Template{
		Slug: "e2e-nodeport", Name: "e2e node port", DataPath: "/data",
		Images:  []models.TemplateImage{{Ref: "alpine:3.24", Default: true}},
		Startup: "while true; do echo pong | nc -l -p 25565; done",
		Console: models.ConsoleConfig{Type: models.ConsoleAttach},
		Ports:   []models.PortSpec{{Name: "game", Port: 25565, Protocol: "TCP", Primary: true}},
	}
	saved, err := st.UpsertTemplate(tmpl)
	if err != nil {
		t.Fatalf("template: %v", err)
	}
	srv := &models.Server{
		Slug: "e2e-nodeport", DisplayName: "node port", TemplateID: saved.ID, TemplateVersion: saved.Version,
		Image: "alpine:3.24", Namespace: reconciler.NamespaceFor("e2e-nodeport"),
		DesiredState: models.StateRunning, Storage: models.Storage{Type: models.StoragePVC, Size: "1Gi"},
		Expose: models.Expose{Type: models.ExposeNodePort},
		Ports:  []models.PortSpec{{Name: "game", Port: 25565, Protocol: "TCP", Primary: true, NodePort: 30555}},
	}
	if err := st.CreateServer(srv); err != nil {
		t.Fatalf("create: %v", err)
	}
	t.Cleanup(func() { _ = rec.DeleteServer(ctx, srv) })
	reconcileUntilRunning(ctx, t, rec, st, srv.ID)
	if err := rec.ReconcileServer(ctx, srv.ID); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	got, err := st.GetServer(srv.ID)
	if err != nil {
		t.Fatalf("server: %v", err)
	}

	var pods corev1.PodList
	if err := c.List(ctx, &pods, client.InNamespace(srv.Namespace), client.MatchingLabels{reconciler.ServerLabel: srv.Slug}); err != nil || len(pods.Items) == 0 {
		t.Fatalf("game pod: %v", err)
	}
	t.Logf("game pod on %s, address shown %s", pods.Items[0].Spec.NodeName, got.Status.Address)

	var line string
	deadline := time.Now().Add(time.Minute)
	for time.Now().Before(deadline) {
		conn, err := net.DialTimeout("tcp", got.Status.Address, 3*time.Second)
		if err == nil {
			_ = conn.SetReadDeadline(time.Now().Add(3 * time.Second))
			line, _ = bufio.NewReader(conn).ReadString('\n')
			conn.Close()
			if strings.TrimSpace(line) == "pong" {
				return
			}
		}
		time.Sleep(2 * time.Second)
	}
	t.Errorf("the address shown, %s, does not answer (last read %q)", got.Status.Address, line)
}
