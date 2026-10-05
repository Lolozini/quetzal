//go:build e2e

package e2e

import (
	"context"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/util/wait"

	"github.com/lolozini/quetzal/internal/models"
	"github.com/lolozini/quetzal/internal/reconciler"
)

// TestE2ECrashIsReportedQuickly runs a game that fails at every start. On
// Kubernetes 1.35 the kubelet calls that CrashLoopBackOff only once its
// back-off reaches minutes, and Quetzal waited for the word: the server was
// "Starting" for five minutes before it was "Crashed".
func TestE2ECrashIsReportedQuickly(t *testing.T) {
	ctx, _, st, rec := setup(t)
	tmpl := &models.Template{
		Slug: "e2e-crash", Name: "e2e crash", DataPath: "/data",
		Images:  []models.TemplateImage{{Ref: "alpine:3.24", Default: true}},
		Startup: "echo 'Error: this world needs Java 25'; exit 1",
		Console: models.ConsoleConfig{Type: models.ConsoleAttach},
	}
	saved, err := st.UpsertTemplate(tmpl)
	if err != nil {
		t.Fatalf("template: %v", err)
	}
	srv := &models.Server{
		Slug: "e2e-crash", DisplayName: "crash", TemplateID: saved.ID, TemplateVersion: saved.Version,
		Image: "alpine:3.24", Namespace: reconciler.NamespaceFor("e2e-crash"),
		DesiredState: models.StateRunning, Storage: models.Storage{Type: models.StoragePVC, Size: "1Gi"},
	}
	if err := st.CreateServer(srv); err != nil {
		t.Fatalf("create: %v", err)
	}
	t.Cleanup(func() { _ = rec.DeleteServer(ctx, srv) })

	// Every 15 s, as the controller does: the kubelet says CrashLoopBackOff
	// for a moment here and there, which a tighter loop can happen to catch.
	var last models.Status
	err = wait.PollUntilContextTimeout(ctx, 15*time.Second, 90*time.Second, true, func(ctx context.Context) (bool, error) {
		if err := rec.ReconcileServer(ctx, srv.ID); err != nil {
			t.Logf("reconcile (retrying): %v", err)
		}
		got, err := st.GetServer(srv.ID)
		if err != nil {
			return false, err
		}
		last = got.Status
		return last.Phase == models.PhaseCrashed, nil
	})
	if err != nil {
		t.Fatalf("not Crashed within 90 s (last status %+v)", last)
	}
	if last.Message != "the game exited with code 1" {
		t.Errorf("message = %q, want the exit code", last.Message)
	}
}
