//go:build e2e

package e2e

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/lolozini/quetzal/internal/models"
	"github.com/lolozini/quetzal/internal/reconciler"
)

// TestE2EUpgradeLeavesRunningServersAlone upgrades the panel under a running
// server. The server's pod runs a helper out of the panel's image, and in
// 0.5.0 the new image tag alone made Kubernetes replace the pod, kicking the
// players of every such server at once.
//
// Kind has one image under two names, "quetzal:e2e" and
// "docker.io/library/quetzal:e2e": the second stands for the next release.
func TestE2EUpgradeLeavesRunningServersAlone(t *testing.T) {
	image := os.Getenv("QUETZAL_E2E_IMAGE")
	if image == "" || strings.Contains(image, "/") {
		t.Skip("QUETZAL_E2E_IMAGE must be a short local name such as quetzal:e2e")
	}
	next := "docker.io/library/" + image
	ctx, c, st, rec := setup(t)
	rec.ActivatorImage = image

	tmpl, err := st.GetTemplateBySlug("generic-process")
	if err != nil {
		t.Fatalf("template: %v", err)
	}
	tmpl.ConfigFiles = []models.ConfigFile{{Path: "server.properties", Parser: models.ParserProperties,
		Find: map[string]string{"motd": "{{server.build.env.MESSAGE}}"}}}
	if _, err := st.UpsertTemplate(tmpl); err != nil {
		t.Fatalf("upsert template: %v", err)
	}
	srv := &models.Server{
		Slug: "e2e-upgrade", DisplayName: "upgrade", TemplateID: tmpl.ID, TemplateVersion: tmpl.Version,
		Image: defaultImage(tmpl), Namespace: reconciler.NamespaceFor("e2e-upgrade"),
		DesiredState: models.StateRunning, Env: map[string]string{"MESSAGE": "hi"},
		Storage: models.Storage{Type: models.StoragePVC, Size: "1Gi"},
	}
	if err := st.CreateServer(srv); err != nil {
		t.Fatalf("create: %v", err)
	}
	t.Cleanup(func() { _ = rec.DeleteServer(ctx, srv) })
	reconcileUntilRunning(ctx, t, rec, st, srv.ID)

	game := func() (appsv1.Deployment, string) {
		t.Helper()
		var dep appsv1.Deployment
		if err := c.Get(ctx, depKey(srv.Namespace), &dep); err != nil {
			t.Fatalf("deployment: %v", err)
		}
		var pods corev1.PodList
		if err := c.List(ctx, &pods, client.InNamespace(srv.Namespace), client.MatchingLabels{reconciler.ServerLabel: srv.Slug}); err != nil {
			t.Fatalf("pods: %v", err)
		}
		uids := []string{}
		for _, p := range pods.Items {
			uids = append(uids, string(p.UID))
		}
		return dep, strings.Join(uids, ",")
	}
	renderCopy := func(dep appsv1.Deployment) string {
		for _, ic := range dep.Spec.Template.Spec.InitContainers {
			if ic.Name == reconciler.RenderCopyContainer {
				return ic.Image
			}
		}
		return ""
	}
	settle := func() {
		for i := 0; i < 3; i++ {
			if err := rec.ReconcileServer(ctx, srv.ID); err != nil {
				t.Fatalf("reconcile: %v", err)
			}
			time.Sleep(time.Second)
		}
	}
	before, pod := game()
	if renderCopy(before) != image {
		t.Fatalf("render-copy runs %q, want %q", renderCopy(before), image)
	}

	// The upgrade.
	rec.ActivatorImage = next
	settle()
	after, podAfter := game()
	if podTemplate(after) != podTemplate(before) || podAfter != pod {
		t.Errorf("the upgrade changed the running server: pods %s -> %s, render-copy %q",
			pod, podAfter, renderCopy(after))
	}

	// The same, from a Deployment made before Quetzal hashed its pod specs.
	// Giving it the hash bumps its generation (annotations do, on a
	// Deployment) but leaves its pods alone.
	delete(after.Annotations, "quetzal.dev/pod-spec")
	if err := c.Update(ctx, &after); err != nil {
		t.Fatalf("strip the annotation: %v", err)
	}
	settle()
	adopted, podAdopted := game()
	if podTemplate(adopted) != podTemplate(before) || podAdopted != pod {
		t.Errorf("the first upgrade from a 0.5.0 Deployment changed the running server: pods %s -> %s, render-copy %q",
			pod, podAdopted, renderCopy(adopted))
	}
	if adopted.Annotations["quetzal.dev/pod-spec"] == "" {
		t.Error("the Deployment was not given its hash")
	}

	// A stop and start brings the new helper.
	if err := st.SetDesiredState(srv.ID, models.StateStopped); err != nil {
		t.Fatalf("stop: %v", err)
	}
	err = wait.PollUntilContextTimeout(ctx, 2*time.Second, 2*time.Minute, true, func(ctx context.Context) (bool, error) {
		if err := rec.ReconcileServer(ctx, srv.ID); err != nil {
			return false, nil
		}
		_, pods := game()
		return pods == "", nil
	})
	if err != nil {
		t.Fatalf("never stopped: %v", err)
	}
	if err := st.SetDesiredState(srv.ID, models.StateRunning); err != nil {
		t.Fatalf("start: %v", err)
	}
	reconcileUntilRunning(ctx, t, rec, st, srv.ID)
	if restarted, _ := game(); renderCopy(restarted) != next {
		t.Errorf("after a restart render-copy runs %q, want the new %q", renderCopy(restarted), next)
	}
}

// podTemplate is a Deployment's pod template as JSON: when it changes,
// Kubernetes replaces the pods.
func podTemplate(d appsv1.Deployment) string {
	b, _ := json.Marshal(d.Spec.Template)
	return string(b)
}
