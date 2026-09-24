//go:build e2e

package e2e

import (
	"context"
	"io"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/kubernetes"
	ctrlconfig "sigs.k8s.io/controller-runtime/pkg/client/config"

	"github.com/lolozini/quetzal/internal/models"
	"github.com/lolozini/quetzal/internal/reconciler"
	"github.com/lolozini/quetzal/internal/startup"
)

// TestE2EStartupDoneLine follows a real container's log: the server stays
// Starting while its game "loads", and turns Running once the done line shows.
func TestE2EStartupDoneLine(t *testing.T) {
	ctx, _, st, rec := setup(t)
	cfg, _ := ctrlconfig.GetConfig()
	cs, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		t.Fatalf("clientset: %v", err)
	}
	wctx, cancel := context.WithCancel(ctx)
	defer cancel()
	watcher := startup.NewWatcher(wctx)
	rec.StartupSeen = func(ns, pod, id string, deadline time.Time, ms []startup.Matcher) bool {
		return watcher.Seen(id, deadline, ms, func(ctx context.Context) (io.ReadCloser, error) {
			return cs.CoreV1().Pods(ns).GetLogs(pod, &corev1.PodLogOptions{Container: reconciler.WorkloadName, Follow: true}).Stream(ctx)
		})
	}

	tmpl := &models.Template{
		Slug: "e2e-startup", Name: "e2e startup", DataPath: "/data",
		Images:  []models.TemplateImage{{Ref: "alpine:3.20", Default: true}},
		Startup: "echo loading the world; sleep 25; echo 'Done (25.0s)! For help, type help'; while true; do sleep 5; done",
		Done:    []string{")! For help, type "},
		Console: models.ConsoleConfig{Type: models.ConsoleAttach},
	}
	saved, err := st.UpsertTemplate(tmpl)
	if err != nil {
		t.Fatalf("template: %v", err)
	}
	srv := &models.Server{
		Slug: "e2e-startup", DisplayName: "startup", TemplateID: saved.ID, TemplateVersion: saved.Version,
		Image: "alpine:3.20", Namespace: reconciler.NamespaceFor("e2e-startup"),
		DesiredState: models.StateRunning, Storage: models.Storage{Type: models.StoragePVC, Size: "1Gi"},
	}
	if err := st.CreateServer(srv); err != nil {
		t.Fatalf("create: %v", err)
	}
	t.Cleanup(func() { _ = rec.DeleteServer(ctx, srv) })

	phaseOf := func() models.Status {
		if err := rec.ReconcileServer(ctx, srv.ID); err != nil {
			t.Logf("reconcile (retrying): %v", err)
		}
		got, err := st.GetServer(srv.ID)
		if err != nil {
			t.Fatalf("get server: %v", err)
		}
		return got.Status
	}

	// The container comes up and waits 25 s before its done line: the server is
	// Starting, and says what for.
	var waiting models.Status
	err = wait.PollUntilContextTimeout(ctx, time.Second, 4*time.Minute, true, func(ctx context.Context) (bool, error) {
		waiting = phaseOf()
		if waiting.Phase == models.PhaseRunning {
			t.Fatalf("Running before the done line: %+v", waiting)
		}
		return waiting.Phase == models.PhaseStarting && strings.Contains(waiting.Message, "in the console"), nil
	})
	if err != nil {
		t.Fatalf("never saw the server wait for its done line (last status %+v): %v", waiting, err)
	}

	var up models.Status
	err = wait.PollUntilContextTimeout(ctx, time.Second, 2*time.Minute, true, func(ctx context.Context) (bool, error) {
		up = phaseOf()
		return up.Phase == models.PhaseRunning, nil
	})
	if err != nil {
		t.Fatalf("never Running after the done line (last status %+v): %v", up, err)
	}
	if up.StartedContainer == "" || up.Message != "" {
		t.Errorf("Running status = %+v, want the started container recorded and no message", up)
	}
}
