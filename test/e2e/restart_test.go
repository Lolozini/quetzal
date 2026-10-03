//go:build e2e

package e2e

import (
	"context"
	"strings"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/kubernetes"
	ctrlconfig "sigs.k8s.io/controller-runtime/pkg/client/config"

	"github.com/lolozini/quetzal/internal/console"
	"github.com/lolozini/quetzal/internal/models"
	"github.com/lolozini/quetzal/internal/reconciler"
)

// signalStartup is a game that writes what it was told to its data volume --
// its stop command, or SIGTERM -- and prints it at the next boot, with the
// version of its template.
func signalStartup(version string) string {
	return "cat events.log 2>/dev/null; echo READY-" + version + "; " +
		"trap 'echo sigterm >> events.log; exit 0' TERM; " +
		"while true; do if read -r -t 1 line; then if [ \"$line\" = save-and-stop ]; then echo stop-command >> events.log; exit 0; fi; fi; done"
}

// TestE2ERestartAndTemplateUpdate runs the recette of 0.10.0's findings on a
// real cluster. A restart, and a new setting, replaced the game's pod with
// SIGTERM alone, never its stop command (R-05); an updated template replaced
// the pod of every running server on it at once (R-22).
func TestE2ERestartAndTemplateUpdate(t *testing.T) {
	ctx, _, st, rec := setup(t)
	cfg, err := ctrlconfig.GetConfig()
	if err != nil {
		t.Fatal(err)
	}
	cs, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		t.Fatal(err)
	}
	// As the controller wires it: the stop command goes to the game's stdin.
	rec.OnStop = func(ctx context.Context, ns, slug, stop string) error {
		pod, err := console.FindRunningPod(ctx, cs, ns, slug)
		if err != nil {
			return err
		}
		cctx, cancel := context.WithTimeout(ctx, 15*time.Second)
		defer cancel()
		return console.SendStdin(cctx, cs, cfg, ns, pod, stop+"\n")
	}

	tmpl, err := st.UpsertTemplate(&models.Template{
		Slug: "e2e-signal", Name: "e2e signal", DataPath: "/data", StopCommand: "save-and-stop",
		Images:  []models.TemplateImage{{Ref: "alpine:3.20", Default: true}},
		Startup: signalStartup("v1"),
		Console: models.ConsoleConfig{Type: models.ConsoleAttach},
	})
	if err != nil {
		t.Fatalf("template: %v", err)
	}
	srv := &models.Server{
		Slug: "e2e-signal", DisplayName: "signal", TemplateID: tmpl.ID, TemplateVersion: tmpl.Version,
		Image: "alpine:3.20", Namespace: reconciler.NamespaceFor("e2e-signal"),
		DesiredState: models.StateRunning, Storage: models.Storage{Type: models.StoragePVC, Size: "1Gi"},
		Resources: models.Resources{Memory: "128Mi"},
	}
	if err := st.CreateServer(srv); err != nil {
		t.Fatalf("create: %v", err)
	}
	t.Cleanup(func() { _ = rec.DeleteServer(ctx, srv) })

	// gameLog waits for a new game pod -- not one of seen -- to be up and to
	// have printed want, and returns its name and log.
	gameLog := func(seen map[string]bool, want string) (string, string) {
		t.Helper()
		var name, log string
		err := wait.PollUntilContextTimeout(ctx, 3*time.Second, 4*time.Minute, true, func(ctx context.Context) (bool, error) {
			if err := rec.ReconcileServer(ctx, srv.ID); err != nil {
				t.Logf("reconcile (retrying): %v", err)
			}
			pod, ok := console.RunningPod(ctx, cs, srv.Namespace, srv.Slug)
			if !ok || seen[pod] {
				return false, nil
			}
			out, err := console.ContainerLog(ctx, cs, srv.Namespace, pod, reconciler.WorkloadName, 50)
			if err != nil || !strings.Contains(out, want) {
				return false, nil
			}
			name, log = pod, out
			return true, nil
		})
		if err != nil {
			t.Fatalf("no new game pod printing %q: %v", want, err)
		}
		return name, log
	}

	first, _ := gameLog(map[string]bool{}, "READY-v1")

	// A restart: the stop command reaches the game, and a new pod comes up.
	if ok, err := st.RequestRestart(srv.ID, time.Now()); err != nil || !ok {
		t.Fatalf("restart: %v %v", ok, err)
	}
	second, log := gameLog(map[string]bool{first: true}, "READY-v1")
	if !strings.Contains(log, "stop-command") {
		t.Errorf("after the restart, the game says it got:\n%s\nwant its stop command", log)
	}
	if got, _ := st.GetServer(srv.ID); got.RestartRequestedAt != nil {
		t.Error("the restart is still pending once the game is back")
	}

	// An updated template does not touch the running pod.
	tmpl.Startup = signalStartup("v2")
	if _, err := st.UpsertTemplate(tmpl); err != nil {
		t.Fatalf("update template: %v", err)
	}
	for i := 0; i < 3; i++ {
		if err := rec.ReconcileServer(ctx, srv.ID); err != nil {
			t.Fatalf("reconcile: %v", err)
		}
		time.Sleep(2 * time.Second)
	}
	if pod, ok := console.RunningPod(ctx, cs, srv.Namespace, srv.Slug); !ok || pod != second {
		t.Fatalf("the template's update replaced the running pod (%s, now %s)", second, pod)
	}
	if got, _ := st.GetServer(srv.ID); !strings.Contains(got.Status.Message, "next restart") {
		t.Errorf("status message %q does not say the update waits for the next restart", got.Status.Message)
	}

	// A new setting replaces the pod: stop command first, and the new template
	// comes with it.
	if err := st.UpdateServerResources(srv.ID, models.Resources{Memory: "160Mi"}); err != nil {
		t.Fatal(err)
	}
	_, log = gameLog(map[string]bool{first: true, second: true}, "READY-v2")
	if strings.Count(log, "stop-command") != 2 {
		t.Errorf("after the new setting, the game says it got:\n%s\nwant a second stop command", log)
	}
}
