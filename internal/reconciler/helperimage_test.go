package reconciler

import (
	"context"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/lolozini/quetzal/internal/crypto"
	"github.com/lolozini/quetzal/internal/models"
)

const (
	panelV1 = "ghcr.io/lolozini/quetzal:v0.5.0"
	panelV2 = "ghcr.io/lolozini/quetzal:v0.5.1"
)

// helperDeployments builds each kind of Deployment that runs a helper out of
// the panel's image, as a given panel version would.
func helperDeployments(s *models.Server, tmpl *models.Template, panel string) map[string]*appsv1.Deployment {
	return map[string]*appsv1.Deployment{
		"game (render-copy)":       BuildDeployment(s, tmpl, panel, nil),
		"data-manager (sftp-copy)": BuildDataDeployment(s, tmpl, panel, 1),
		"proxy activator": BuildActivatorDeployment(s, tmpl, ActivatorParams{
			Image: panel, WakeURL: "http://panel/wake", ActiveURL: "http://panel/active", Token: "t", Proxy: true,
		}),
	}
}

func serverWithHelpers() (*models.Server, *models.Template) {
	s, tmpl := testServerAndTemplate()
	tmpl.ConfigFiles = []models.ConfigFile{{Path: "server.properties", Parser: models.ParserProperties,
		Find: map[string]string{"server-port": "{{server.build.default.port}}"}}}
	s.SFTP.Enabled = true
	return s, tmpl
}

// Upgrading the panel changes its image, and each server's pods ran their
// helpers from it: every upgrade restarted every such server, players and
// all. A running pod now keeps its helper image while nothing else changes.
func TestUpgradeLeavesRunningPodsAlone(t *testing.T) {
	s, tmpl := serverWithHelpers()
	before := helperDeployments(s, tmpl, panelV1)
	after := helperDeployments(s, tmpl, panelV2)
	for name, live := range before {
		keepHelperImage(nil, live, panelV1) // as first applied
		want := after[name]
		if !keepHelperImage(live, want, panelV2) {
			t.Errorf("%s: the upgrade gave a running pod the new helper image", name)
			continue
		}
		if !equality.Semantic.DeepEqual(want.Spec.Template, live.Spec.Template) {
			t.Errorf("%s: the pod template still changes with the upgrade", name)
		}
	}
}

// Anything else that changes the pod restarts it anyway: it comes back with
// the new helper image.
func TestAnotherChangeBringsTheNewHelper(t *testing.T) {
	s, tmpl := serverWithHelpers()
	live := BuildDeployment(s, tmpl, panelV1, nil)
	keepHelperImage(nil, live, panelV1)

	s.Env = map[string]string{"MSG": "a new value"}
	want := BuildDeployment(s, tmpl, panelV2, nil)
	if keepHelperImage(live, want, panelV2) {
		t.Fatal("kept the old helper image on a template that changes anyway")
	}
	if img := initImage(want, RenderCopyContainer); img != panelV2 {
		t.Errorf("render-copy runs %q, want %q", img, panelV2)
	}
}

// A server that is stopped, or being stopped, has no pod to keep.
func TestStartAndStopBringTheNewHelper(t *testing.T) {
	s, tmpl := serverWithHelpers()
	running := BuildDeployment(s, tmpl, panelV1, nil)
	keepHelperImage(nil, running, panelV1)
	s.DesiredState = models.StateStopped
	stopped := BuildDeployment(s, tmpl, panelV1, nil)
	keepHelperImage(nil, stopped, panelV1)

	cases := map[string]struct {
		live *appsv1.Deployment
		to   models.DesiredState
	}{
		"start": {stopped, models.StateRunning},
		"stop":  {running, models.StateStopped},
	}
	for name, c := range cases {
		s.DesiredState = c.to
		want := BuildDeployment(s, tmpl, panelV2, nil)
		if keepHelperImage(c.live, want, panelV2) || initImage(want, RenderCopyContainer) != panelV2 {
			t.Errorf("%s: render-copy runs %q, want the new %q", name, initImage(want, RenderCopyContainer), panelV2)
		}
	}
}

// A Deployment made before the hash existed has nothing to compare: the
// reconciler asks the API server instead (see keepLegacyHelperImage, covered
// end to end), and keepHelperImage alone leaves the new image.
func TestLegacyDeploymentIsLeftToTheAPIServer(t *testing.T) {
	s, tmpl := serverWithHelpers()
	live := BuildDeployment(s, tmpl, panelV1, nil)
	want := BuildDeployment(s, tmpl, panelV2, nil)
	if keepHelperImage(live, want, panelV2) || !isLegacy(live) {
		t.Error("a Deployment without a hash was treated as comparable")
	}
}

// The game image itself is never swapped: only containers that run the panel's
// image are helpers.
func TestOnlyHelpersKeepTheirImage(t *testing.T) {
	s, tmpl := serverWithHelpers()
	live := BuildDeployment(s, tmpl, panelV1, nil)
	keepHelperImage(nil, live, panelV1)
	want := BuildDeployment(s, tmpl, panelV2, nil)
	keepHelperImage(live, want, panelV2)
	if img := want.Spec.Template.Spec.Containers[0].Image; img != s.Image {
		t.Errorf("game container runs %q, want %q", img, s.Image)
	}
	if img := initImage(want, RenderConfigContainer); img != s.Image {
		t.Errorf("render-config runs %q, want the game image %q", img, s.Image)
	}
}

func initImage(d *appsv1.Deployment, name string) string {
	for _, c := range append(append([]corev1.Container(nil), d.Spec.Template.Spec.InitContainers...), d.Spec.Template.Spec.Containers...) {
		if c.Name == name {
			return c.Image
		}
	}
	return ""
}

// A proxy activator runs for as long as its server may sleep, so keeping its
// image until "the next restart" meant keeping it for good, and with it the
// wake rules of the version it started with. An activator takes the new image
// while its server sleeps, when nobody goes through it; a proxy, which carries
// every player while the game is up, waits for that.
func TestActivatorsTakeTheNewImageWhenNobodyGoesThroughThem(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := appsv1.AddToScheme(scheme); err != nil {
		t.Fatalf("scheme: %v", err)
	}
	s, tmpl := serverWithHelpers()
	ctx := context.Background()
	for _, c := range []struct {
		name              string
		proxy, hibernated bool
		want              string
	}{
		{"drop mode (only while asleep)", false, true, panelV2},
		{"proxy, game up", true, false, panelV1},
		{"proxy, game asleep", true, true, panelV2},
	} {
		params := ActivatorParams{Image: panelV1, WakeURL: "http://panel/wake", ActiveURL: "http://panel/active",
			Token: crypto.WakeToken(nil, s.Slug), Proxy: c.proxy}
		live := BuildActivatorDeployment(s, tmpl, params)
		keepHelperImage(nil, live, panelV1) // as first applied
		cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(live).Build()
		r := &Reconciler{Client: cl, ActivatorImage: panelV2, WakeURL: params.WakeURL, ActiveURL: params.ActiveURL}
		s.Hibernated = c.hibernated
		if err := r.ensureActivator(ctx, s, tmpl, c.proxy, !c.proxy); err != nil {
			t.Fatalf("%s: ensure: %v", c.name, err)
		}
		var got appsv1.Deployment
		if err := cl.Get(ctx, client.ObjectKeyFromObject(live), &got); err != nil {
			t.Fatalf("%s: get: %v", c.name, err)
		}
		if img := got.Spec.Template.Spec.Containers[0].Image; img != c.want {
			t.Errorf("%s: activator runs %s, want %s", c.name, img, c.want)
		}
		if got.Annotations[podSpecAnnotation] == "" {
			t.Errorf("%s: the pod spec hash went missing", c.name)
		}
	}
}

// 0.7 bounds the install container and the helpers and gives the game Wings'
// memory headroom: a change to the pod of every server that has a limit, which
// would have restarted them all with the upgrade. A running pod keeps the
// rendering it started with, as it keeps its helper image, until something
// restarts it anyway.
func TestARenderingChangeLeavesRunningPodsAlone(t *testing.T) {
	s, tmpl := serverWithHelpers()
	tmpl.Install = &models.InstallScript{Script: "echo installing"}
	s.Resources = models.Resources{Memory: "1536Mi", CPU: "1"}
	as06 := func(s *models.Server) map[string]*appsv1.Deployment {
		out := helperDeployments(s, tmpl, panelV1)
		for _, d := range out {
			undoResources07(d)
			keepHelperImage(nil, d, panelV1) // as first applied
		}
		return out
	}
	before := as06(s)
	for name, want := range helperDeployments(s, tmpl, panelV2) {
		live := before[name]
		if !keepHelperImage(live, want, panelV2) {
			t.Errorf("%s: the upgrade gave a running pod the new rendering", name)
			continue
		}
		if !equality.Semantic.DeepEqual(want.Spec.Template, live.Spec.Template) {
			t.Errorf("%s: the pod template still changes with the upgrade", name)
		}
	}

	// A new memory limit restarts the server, which then gets this rendering.
	s.Resources.Memory = "2Gi"
	want := BuildDeployment(s, tmpl, panelV2, nil)
	if keepHelperImage(before["game (render-copy)"], want, panelV2) {
		t.Fatal("kept the old rendering of a server whose memory changed")
	}
	if limits := want.Spec.Template.Spec.Containers[0].Resources.Limits; limits.Memory().String() != "2355Mi" {
		t.Errorf("memory limit %s after the change, want 2355Mi", limits.Memory())
	}

	// So does a stop: the next start renders afresh.
	s.Resources.Memory = "1536Mi"
	s.DesiredState = models.StateStopped
	stopped := BuildDeployment(s, tmpl, panelV2, nil)
	if keepHelperImage(before["game (render-copy)"], stopped, panelV2) {
		t.Error("kept the old rendering for a server being stopped")
	}
	for _, c := range stopped.Spec.Template.Spec.InitContainers {
		if c.Name == InstallContainer && len(c.Resources.Limits) == 0 {
			t.Error("the stopped server's install still has no limits")
		}
	}
}

// Game servers ran in UTC whatever the panel's zone: a game logged 12:36 at
// 14:36 in Paris, and a plugin's daily restart came two hours off (recette of
// 0.10.0, R-06). They take the panel's zone now -- and a pod already running
// keeps UTC until it restarts, rather than every server restarting with the
// upgrade that brings the zone.
func TestServersTakeThePanelZoneAtTheirNextStart(t *testing.T) {
	t.Cleanup(func() { _ = SetServerZone("") })
	s, tmpl := serverWithHelpers()
	live := BuildDeployment(s, tmpl, panelV1, nil) // rendered in UTC
	keepHelperImage(nil, live, panelV1)

	if err := SetServerZone("Europe/Paris"); err != nil {
		t.Fatal(err)
	}
	want := BuildDeployment(s, tmpl, panelV2, nil)
	if !keepHelperImage(live, want, panelV2) || !equality.Semantic.DeepEqual(want.Spec.Template, live.Spec.Template) {
		t.Fatal("the new zone restarted a running server")
	}

	s.DesiredState = models.StateStopped
	stopped := BuildDeployment(s, tmpl, panelV2, nil)
	keepHelperImage(live, stopped, panelV2)
	for _, e := range stopped.Spec.Template.Spec.Containers[0].Env {
		if e.Name == "TZ" && e.Value != "Europe/Paris" {
			t.Errorf("TZ = %q at the next start, want the panel's zone", e.Value)
		}
	}

	if err := SetServerZone("Mars/Olympus_Mons"); err == nil || serverZone != "UTC" {
		t.Errorf("an unknown zone: err %v, zone %q; want refused, and UTC", err, serverZone)
	}
}
