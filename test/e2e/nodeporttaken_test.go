//go:build e2e

package e2e

import (
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/lolozini/quetzal/internal/models"
	"github.com/lolozini/quetzal/internal/reconciler"
	"github.com/lolozini/quetzal/internal/store"
)

// TestE2ETakenNodePortMoves publishes a server on a node port that a Service
// outside Quetzal already holds, as the recette of 0.10.0 did: the pool only
// knows Quetzal's allocations. The apiserver refused the server's Service on
// every pass, and the pass stopped there, before the network policy, so the
// game ran with the run of the cluster while the panel showed it "Stopped".
// The port is now set aside and the server moved to another one.
func TestE2ETakenNodePortMoves(t *testing.T) {
	ctx, c, st, rec := setup(t)
	const lo, hi = 32700, 32720
	rec.NodePortMin, rec.NodePortMax = lo, hi

	// The Service outside Quetzal, on a port of Quetzal's pool.
	var taken int32
	for p := int32(lo); p <= hi && taken == 0; p++ {
		foreign := &corev1.Service{
			ObjectMeta: metav1.ObjectMeta{Name: "e2e-outside", Namespace: "default"},
			Spec: corev1.ServiceSpec{
				Type:     corev1.ServiceTypeNodePort,
				Selector: map[string]string{"app": "nothing"},
				Ports:    []corev1.ServicePort{{Port: 80, TargetPort: intstr.FromInt32(80), NodePort: p}},
			},
		}
		if err := c.Create(ctx, foreign); err == nil {
			taken = p
			t.Cleanup(func() { _ = c.Delete(ctx, foreign) })
		} else if !strings.Contains(err.Error(), "already allocated") {
			t.Fatalf("Service outside Quetzal: %v", err)
		}
	}
	if taken == 0 {
		t.Fatalf("no free node port in %d-%d for the Service outside Quetzal", lo, hi)
	}

	tmpl, err := st.UpsertTemplate(&models.Template{
		Slug: "e2e-taken-port", Name: "e2e taken port", DataPath: "/data",
		Images:  []models.TemplateImage{{Ref: "alpine:3.20", Default: true}},
		Startup: "while true; do echo pong | nc -l -p 25565; done",
		Console: models.ConsoleConfig{Type: models.ConsoleAttach},
		Ports:   []models.PortSpec{{Name: "game", Port: 25565, Protocol: "TCP", Primary: true}},
	})
	if err != nil {
		t.Fatalf("template: %v", err)
	}
	srv := &models.Server{
		Slug: "e2e-taken-port", DisplayName: "taken port", TemplateID: tmpl.ID, TemplateVersion: tmpl.Version,
		Image: "alpine:3.20", Namespace: reconciler.NamespaceFor("e2e-taken-port"),
		DesiredState: models.StateRunning, Storage: models.Storage{Type: models.StoragePVC, Size: "1Gi"},
		Expose: models.Expose{Type: models.ExposeNodePort},
	}
	if err := st.CreateServer(srv); err != nil {
		t.Fatalf("create: %v", err)
	}
	t.Cleanup(func() { _ = rec.DeleteServer(ctx, srv) })
	// What the pool hands out when it does not know the port is taken.
	if _, err := st.AllocateNodePort(srv.ID, store.NodePortKey(25565), taken, taken); err != nil {
		t.Fatalf("allocate: %v", err)
	}
	ports := []models.PortSpec{{Name: "game", Port: 25565, Protocol: "TCP", Primary: true, NodePort: taken}}
	if err := st.UpdateServerNetworking(srv.ID, srv.Expose, ports); err != nil {
		t.Fatalf("ports: %v", err)
	}

	reconcileUntilRunning(ctx, t, rec, st, srv.ID)

	assertExists(ctx, t, c, &networkingv1.NetworkPolicy{}, srv.Namespace, "quetzal-default")
	var svc corev1.Service
	if err := c.Get(ctx, client.ObjectKey{Namespace: srv.Namespace, Name: "server"}, &svc); err != nil {
		t.Fatalf("the server's Service: %v", err)
	}
	moved := svc.Spec.Ports[0].NodePort
	if moved == taken || moved < lo || moved > hi {
		t.Fatalf("Service on node port %d, want another port of %d-%d than %d", moved, lo, hi, taken)
	}
	got, err := st.GetServer(srv.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Ports[0].NodePort != moved {
		t.Errorf("the panel publishes node port %d, the Service %d", got.Ports[0].NodePort, moved)
	}
	events, err := st.ListEventsForServer(srv.ID, 0, 50)
	if err != nil {
		t.Fatal(err)
	}
	said := false
	for _, e := range events {
		said = said || e.Type == models.EventServerPortMoved
	}
	if !said {
		t.Error("the move of the port was not told")
	}
}
