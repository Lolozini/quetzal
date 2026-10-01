//go:build e2e

package e2e

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
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

// TestE2EARenderingChangeLeavesRunningServersAlone upgrades the panel to a
// release that renders a server's pod differently with nothing changed on the
// server: 0.7 bounds the install container and gives the game Wings' memory
// headroom. The running pod keeps what it started with, through a Deployment
// made before the hash existed (the API server's dry run decides) and then
// one that has it, and a stop and start brings the new rendering.
func TestE2EARenderingChangeLeavesRunningServersAlone(t *testing.T) {
	ctx, c, st, rec := setup(t)
	saved, err := st.UpsertTemplate(&models.Template{
		Slug: "e2e-render", Name: "e2e-render", DataPath: "/data",
		Images:  []models.TemplateImage{{Ref: "alpine:3.20", Default: true}},
		Install: &models.InstallScript{Script: "echo installed > /mnt/server/installed"},
		Startup: "sleep 3600",
		Console: models.ConsoleConfig{Type: models.ConsoleAttach},
	})
	if err != nil {
		t.Fatalf("template: %v", err)
	}
	// A name of its own each run: the last run's namespace may still be going.
	slug := fmt.Sprintf("e2e-render-%d", time.Now().Unix()%100000)
	srv := &models.Server{
		Slug: slug, DisplayName: "render", TemplateID: saved.ID, TemplateVersion: saved.Version,
		Image: "alpine:3.20", Namespace: reconciler.NamespaceFor(slug),
		DesiredState: models.StateRunning, Resources: models.Resources{Memory: "512Mi"},
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
			if p.DeletionTimestamp == nil && p.Status.Phase == corev1.PodRunning {
				uids = append(uids, string(p.UID))
			}
		}
		return dep, strings.Join(uids, ",")
	}
	gameMemory := func(d appsv1.Deployment) string {
		for _, ct := range d.Spec.Template.Spec.Containers {
			if ct.Name == reconciler.WorkloadName {
				return ct.Resources.Limits.Memory().String()
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
	if dep, _ := game(); gameMemory(dep) != "589Mi" {
		t.Fatalf("a 512Mi server's memory limit is %s, want 589Mi", gameMemory(dep))
	}

	// What 0.6 left running: a Deployment from before the hash, without
	// resources on the install container and with the memory set as the limit.
	dep, _ := game()
	for i := range dep.Spec.Template.Spec.InitContainers {
		dep.Spec.Template.Spec.InitContainers[i].Resources = corev1.ResourceRequirements{}
	}
	for i := range dep.Spec.Template.Spec.Containers {
		ct := &dep.Spec.Template.Spec.Containers[i]
		if ct.Name == reconciler.WorkloadName {
			ct.Resources = corev1.ResourceRequirements{Limits: corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("512Mi")}}
		}
	}
	delete(dep.Annotations, "quetzal.dev/pod-spec")
	if err := c.Update(ctx, &dep); err != nil {
		t.Fatalf("roll back to the 0.6 rendering: %v", err)
	}
	var pod string
	err = wait.PollUntilContextTimeout(ctx, 2*time.Second, 2*time.Minute, true, func(ctx context.Context) (bool, error) {
		d, p := game()
		pod = p
		// Status read before the controller saw the new template still counts
		// the old pod as up to date.
		rolled := d.Status.ObservedGeneration >= d.Generation && d.Status.Replicas == 1 &&
			d.Status.UpdatedReplicas == 1 && d.Status.ReadyReplicas == 1
		return rolled && p != "" && !strings.Contains(p, ","), nil
	})
	if err != nil {
		t.Fatalf("the 0.6 pod never ran: %v", err)
	}
	old, _ := game()

	// The upgrade, once from the Deployment without a hash, then again with it.
	for _, round := range []string{"without a hash", "with its hash"} {
		settle()
		after, podAfter := game()
		if podTemplate(after) != podTemplate(old) || podAfter != pod {
			t.Errorf("%s: the upgrade changed the running server: pods %s -> %s, memory limit %s", round, pod, podAfter, gameMemory(after))
		}
		if after.Annotations["quetzal.dev/pod-spec"] == "" {
			t.Errorf("%s: the Deployment was not given its hash", round)
		}
	}

	// A stop and start brings this release's rendering.
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
	if restarted, _ := game(); gameMemory(restarted) != "589Mi" {
		t.Errorf("after a stop and start the memory limit is %s, want 589Mi", gameMemory(restarted))
	}
}
