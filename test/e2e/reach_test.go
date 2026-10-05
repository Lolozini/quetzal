//go:build e2e

package e2e

import (
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	ctrlconfig "sigs.k8s.io/controller-runtime/pkg/client/config"

	"github.com/lolozini/quetzal/internal/models"
	"github.com/lolozini/quetzal/internal/reconciler"
)

// TestE2EServerReachesTheServersItWasGiven runs a server that answers on a port
// and another that tries it every two seconds. A game server's policy keeps it
// off the cluster network, so the second could not reach the first -- which is
// how a Velocity proxy could reach its servers only through the internet --
// until it is given the first to reach. It needs a CNI that enforces
// NetworkPolicy, and is skipped where the first attempt already gets through.
func TestE2EServerReachesTheServersItWasGiven(t *testing.T) {
	ctx, _, st, rec := setup(t)
	cfg, _ := ctrlconfig.GetConfig()
	cs, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		t.Fatalf("clientset: %v", err)
	}
	server := func(slug, startup string, ports []models.PortSpec) *models.Server {
		t.Helper()
		saved, err := st.UpsertTemplate(&models.Template{
			Slug: slug, Name: slug, DataPath: "/data",
			Images:  []models.TemplateImage{{Ref: "alpine:3.24", Default: true}},
			Startup: startup,
			Console: models.ConsoleConfig{Type: models.ConsoleAttach},
			Ports:   ports,
		})
		if err != nil {
			t.Fatalf("template %s: %v", slug, err)
		}
		srv := &models.Server{
			Slug: slug, DisplayName: slug, TemplateID: saved.ID, TemplateVersion: saved.Version,
			Image: "alpine:3.24", Namespace: reconciler.NamespaceFor(slug),
			DesiredState: models.StateRunning, Storage: models.Storage{Type: models.StoragePVC, Size: "1Gi"},
		}
		if err := st.CreateServer(srv); err != nil {
			t.Fatalf("create %s: %v", slug, err)
		}
		t.Cleanup(func() { _ = rec.DeleteServer(ctx, srv) })
		reconcileUntilRunning(ctx, t, rec, st, srv.ID)
		return srv
	}

	backend := server("e2e-reach-backend", "while true; do echo pong | nc -l -p 25565; done",
		[]models.PortSpec{{Name: "game", Port: 25565, Protocol: "TCP", Primary: true}})
	addr := "server." + backend.Namespace + ".svc.cluster.local"
	front := server("e2e-reach-front",
		"while true; do if nc -w 2 "+addr+" 25565 </dev/null | grep -q pong; then echo REACHED; else echo BLOCKED; fi; sleep 2; done", nil)

	// verdict is the last thing the front printed: REACHED, BLOCKED, or "".
	verdict := func() string {
		pods, err := cs.CoreV1().Pods(front.Namespace).List(ctx, metav1.ListOptions{LabelSelector: reconciler.ServerLabel + "=" + front.Slug})
		if err != nil || len(pods.Items) == 0 {
			return ""
		}
		tail := int64(3)
		raw, err := cs.CoreV1().Pods(front.Namespace).GetLogs(pods.Items[0].Name,
			&corev1.PodLogOptions{Container: reconciler.WorkloadName, TailLines: &tail}).Do(ctx).Raw()
		if err != nil {
			return ""
		}
		lines := strings.Fields(string(raw))
		if len(lines) == 0 {
			return ""
		}
		return lines[len(lines)-1]
	}
	waitFor := func(want string, within time.Duration) bool {
		for deadline := time.Now().Add(within); time.Now().Before(deadline); time.Sleep(2 * time.Second) {
			if verdict() == want {
				return true
			}
		}
		return false
	}

	if !waitFor("BLOCKED", 45*time.Second) {
		if verdict() == "REACHED" {
			t.Skip("this cluster does not enforce NetworkPolicy: the front reached the backend before it was given it")
		}
		t.Fatalf("the front never tried the backend (last: %q)", verdict())
	}
	time.Sleep(6 * time.Second)
	if v := verdict(); v != "BLOCKED" {
		t.Fatalf("the front reached the backend before it was given it (%s)", v)
	}

	if err := st.UpdateServerReaches(front.ID, []string{backend.Slug}); err != nil {
		t.Fatalf("reaches: %v", err)
	}
	if err := rec.ReconcileServer(ctx, front.ID); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if !waitFor("REACHED", time.Minute) {
		t.Errorf("the front cannot reach the backend it was given (last: %q)", verdict())
	}
}
