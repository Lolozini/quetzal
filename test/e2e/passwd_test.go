//go:build e2e

package e2e

import (
	"context"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/kubernetes"
	"sigs.k8s.io/controller-runtime/pkg/client"
	ctrlconfig "sigs.k8s.io/controller-runtime/pkg/client/config"

	"github.com/lolozini/quetzal/internal/models"
	"github.com/lolozini/quetzal/internal/reconciler"
)

// TestE2EEggUserHasAName runs a template that names no user, as imported eggs
// don't, and asks the game container who it is. Without the mounted passwd the
// answer was "unknown ID 988", and Valheim segfaulted on the same question.
func TestE2EEggUserHasAName(t *testing.T) {
	ctx, c, st, rec := setup(t)
	cfg, _ := ctrlconfig.GetConfig()
	cs, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		t.Fatalf("clientset: %v", err)
	}

	tmpl := &models.Template{
		Slug: "e2e-passwd", Name: "e2e passwd", DataPath: "/data",
		Images:  []models.TemplateImage{{Ref: "alpine:3.24", Default: true}},
		Startup: `echo "user=$(id -un) home=$(getent passwd $(id -u) | cut -d: -f6)"; while true; do sleep 5; done`,
		Console: models.ConsoleConfig{Type: models.ConsoleAttach},
	}
	saved, err := st.UpsertTemplate(tmpl)
	if err != nil {
		t.Fatalf("template: %v", err)
	}
	srv := &models.Server{
		Slug: "e2e-passwd", DisplayName: "passwd", TemplateID: saved.ID, TemplateVersion: saved.Version,
		Image: "alpine:3.24", Namespace: reconciler.NamespaceFor("e2e-passwd"),
		DesiredState: models.StateRunning, Storage: models.Storage{Type: models.StoragePVC, Size: "1Gi"},
	}
	if err := st.CreateServer(srv); err != nil {
		t.Fatalf("create: %v", err)
	}
	t.Cleanup(func() { _ = rec.DeleteServer(ctx, srv) })
	reconcileUntilRunning(ctx, t, rec, st, srv.ID)

	var pods corev1.PodList
	if err := c.List(ctx, &pods, client.InNamespace(srv.Namespace), client.MatchingLabels{reconciler.ServerLabel: srv.Slug}); err != nil || len(pods.Items) == 0 {
		t.Fatalf("game pod: %v (%d found)", err, len(pods.Items))
	}
	pod := pods.Items[0].Name
	var line string
	err = wait.PollUntilContextTimeout(ctx, time.Second, time.Minute, true, func(ctx context.Context) (bool, error) {
		out, err := cs.CoreV1().Pods(srv.Namespace).GetLogs(pod, &corev1.PodLogOptions{Container: reconciler.WorkloadName}).DoRaw(ctx)
		if err != nil {
			return false, nil
		}
		line = strings.TrimSpace(string(out))
		return line != "", nil
	})
	if err != nil {
		t.Fatalf("no output from the game container: %v", err)
	}
	if line != "user=container home=/data" {
		t.Errorf("the game container says %q, want user=container home=/data", line)
	}
}
