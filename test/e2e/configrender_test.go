//go:build e2e

package e2e

import (
	"bytes"
	"context"
	"os"
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

// TestE2EConfigRender verifies that egg config.files are actually rendered into
// the data volume at startup. It exercises the two-init-container mechanism
// (copy from the Quetzal image, render as the game's user), so it needs the
// Quetzal image loaded into the cluster; set QUETZAL_E2E_IMAGE to its ref.
func TestE2EConfigRender(t *testing.T) {
	image := os.Getenv("QUETZAL_E2E_IMAGE")
	if image == "" {
		t.Skip("QUETZAL_E2E_IMAGE not set (the config-render init needs the Quetzal image in-cluster)")
	}
	ctx, _, st, rec := setup(t)
	rec.ActivatorImage = image // reused as the system image for the render-copy init

	// Take the generic shell template and declare a config file on it.
	tmpl, err := st.GetTemplateBySlug("generic-process")
	if err != nil {
		t.Fatalf("template: %v", err)
	}
	tmpl.ConfigFiles = []models.ConfigFile{{
		Path:   "server.properties",
		Parser: models.ParserProperties,
		Find: map[string]string{
			"server-name": "{{server.build.env.SRV_NAME}}",
			"max-players": "7",
		},
	}}
	if _, err := st.UpsertTemplate(tmpl); err != nil {
		t.Fatalf("upsert template: %v", err)
	}

	srv := &models.Server{
		Slug: "e2e-cfgrender", DisplayName: "cfg", TemplateID: tmpl.ID, TemplateVersion: tmpl.Version,
		Image: defaultImage(tmpl), Namespace: reconciler.NamespaceFor("e2e-cfgrender"),
		DesiredState: models.StateRunning, Env: map[string]string{"SRV_NAME": "quetzal"},
		Storage: models.Storage{Type: models.StoragePVC, Size: "1Gi"},
	}
	if err := st.CreateServer(srv); err != nil {
		t.Fatalf("create: %v", err)
	}
	t.Cleanup(func() { _ = rec.DeleteServer(ctx, srv) })
	reconcileUntilRunning(ctx, t, rec, st, srv.ID)

	cfg, err := ctrlconfig.GetConfig()
	if err != nil {
		t.Fatalf("kube config: %v", err)
	}
	cs, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		t.Fatalf("clientset: %v", err)
	}

	// The render init container should have written server.properties before the
	// main container started.
	var out string
	err = wait.PollUntilContextTimeout(ctx, 2*time.Second, time.Minute, true, func(ctx context.Context) (bool, error) {
		pod, ok := console.RunningPod(ctx, cs, srv.Namespace, srv.Slug)
		if !ok {
			return false, nil
		}
		var buf bytes.Buffer
		if err := console.Exec(ctx, cs, cfg, srv.Namespace, pod, []string{"cat", "/data/server.properties"}, nil, &buf); err != nil {
			return false, nil
		}
		out = buf.String()
		return true, nil
	})
	if err != nil {
		t.Fatalf("could not read rendered file: %v", err)
	}
	for _, want := range []string{"server-name=quetzal", "max-players=7"} {
		if !strings.Contains(out, want) {
			t.Errorf("rendered server.properties missing %q:\n%s", want, out)
		}
	}
}
