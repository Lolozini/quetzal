//go:build e2e

package e2e

import (
	"strings"
	"testing"

	"k8s.io/client-go/kubernetes"
	ctrlconfig "sigs.k8s.io/controller-runtime/pkg/client/config"

	"github.com/lolozini/quetzal/internal/console"
	"github.com/lolozini/quetzal/internal/models"
	"github.com/lolozini/quetzal/internal/reconciler"
	"github.com/lolozini/quetzal/internal/store"
)

// TestE2ECleanReinstall updates a "modpack" the way a clean reinstall does it:
// the old pack's files go, the world and a kept config stay, and the install
// puts the new pack in -- on a real volume, from the install init container.
func TestE2ECleanReinstall(t *testing.T) {
	ctx, _, st, rec := setup(t)
	cfg, _ := ctrlconfig.GetConfig()
	cs, _ := kubernetes.NewForConfig(cfg)

	tmpl := &models.Template{
		Slug: "e2e-modpack", Name: "e2e modpack", DataPath: "/data",
		Images:  []models.TemplateImage{{Ref: "alpine:3.24", Default: true}},
		Startup: "echo up; while true; do sleep 5; done",
		Console: models.ConsoleConfig{Type: models.ConsoleAttach},
		Install: &models.InstallScript{Image: "alpine:3.24", Script: `cd /mnt/server
mkdir -p mods config
echo "pack $PACK" > "mods/pack-$PACK.jar"
echo "defaults $PACK" > config/pack.toml`},
		Variables: []models.TemplateVariable{{Name: "Pack", EnvVariable: "PACK", Type: models.VarString, Default: "1", Editable: true}},
	}
	saved, err := st.UpsertTemplate(tmpl)
	if err != nil {
		t.Fatalf("template: %v", err)
	}
	srv := &models.Server{
		Slug: "e2e-clean", DisplayName: "clean", TemplateID: saved.ID, TemplateVersion: saved.Version,
		Image: "alpine:3.24", Namespace: reconciler.NamespaceFor("e2e-clean"),
		DesiredState: models.StateRunning, Env: map[string]string{"PACK": "1"},
		Storage:           models.Storage{Type: models.StoragePVC, Size: "1Gi"},
		InstallGeneration: 1,
	}
	if err := st.CreateServer(srv); err != nil {
		t.Fatalf("create: %v", err)
	}
	t.Cleanup(func() { _ = rec.DeleteServer(ctx, srv) })
	reconcileUntilRunning(ctx, t, rec, st, srv.ID)

	pod, err := console.FindRunningPod(ctx, cs, srv.Namespace, srv.Slug)
	if err != nil {
		t.Fatalf("find pod: %v", err)
	}
	// The world the players built, and a config somebody tuned.
	execInPod(ctx, t, cs, cfg, srv.Namespace, pod, []string{"sh", "-c",
		"mkdir -p /data/world/region && echo chunk > /data/world/region/r.0.0.mca && echo tuned > /data/config/server.toml && echo my-own > /data/ops.json"})

	// Pack 2, clean: the server stops, and comes back on the new pack.
	if err := st.SetDesiredState(srv.ID, models.StateStopped); err != nil {
		t.Fatalf("stop: %v", err)
	}
	if err := rec.ReconcileServer(ctx, srv.ID); err != nil {
		t.Fatalf("reconcile stop: %v", err)
	}
	waitNoPods(ctx, t, cs, srv.Namespace, srv.Slug)
	if err := st.ReinstallServer(srv.ID, store.ServerReinstall{
		TemplateID: saved.ID, TemplateVersion: saved.Version, Image: "alpine:3.24",
		Env: map[string]string{"PACK": "2"}, Install: true, Wipe: true,
		Keep: []string{"world*", "config/server.toml", "ops.json"},
	}); err != nil {
		t.Fatalf("reinstall: %v", err)
	}
	if err := st.SetDesiredState(srv.ID, models.StateRunning); err != nil {
		t.Fatalf("start: %v", err)
	}
	reconcileUntilRunning(ctx, t, rec, st, srv.ID)

	pod, err = console.FindRunningPod(ctx, cs, srv.Namespace, srv.Slug)
	if err != nil {
		t.Fatalf("find pod: %v", err)
	}
	out := execInPod(ctx, t, cs, cfg, srv.Namespace, pod, []string{"sh", "-c",
		"cd /data && find . -type f ! -name .quetzal-installed | sort && cat config/server.toml"})
	for _, want := range []string{"./world/region/r.0.0.mca", "./config/server.toml", "./ops.json", "./mods/pack-2.jar", "./config/pack.toml", "tuned"} {
		if !strings.Contains(out, want) {
			t.Errorf("%s missing after the clean reinstall; files:\n%s", want, out)
		}
	}
	if strings.Contains(out, "pack-1.jar") {
		t.Errorf("the old pack's mod survived the clean reinstall; files:\n%s", out)
	}

	// Once the server is down again, the wipe and its list are retired; the
	// list stays remembered for the next update.
	if err := st.SetDesiredState(srv.ID, models.StateStopped); err != nil {
		t.Fatalf("stop: %v", err)
	}
	if err := rec.ReconcileServer(ctx, srv.ID); err != nil {
		t.Fatalf("reconcile stop: %v", err)
	}
	got, _ := st.GetServer(srv.ID)
	if got.InstallWipe || len(got.InstallKeep) != 0 {
		t.Errorf("after the reinstall: wipe %v keeping %q", got.InstallWipe, got.InstallKeep)
	}
	if len(got.ReinstallKeep) != 3 {
		t.Errorf("remembered %q", got.ReinstallKeep)
	}
}
