package reconciler

import (
	"context"
	"errors"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	"k8s.io/apimachinery/pkg/util/managedfields"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/lolozini/quetzal/internal/models"
)

// fakeRegistry answers lookups from tags, or not at all when down.
type fakeRegistry struct {
	tags    map[string]string
	down    bool
	lookups int
}

func (f *fakeRegistry) digest(_ context.Context, ref string) (string, error) {
	f.lookups++
	if f.down {
		return "", errors.New("connection refused")
	}
	return f.tags[ref], nil
}

func sha(c string) string { return "sha256:" + strings.Repeat(c, 64) }

// A tag a node had pulled was never pulled again: an image fixed upstream did
// not reach a server that had run the old one. Each start now looks the tags
// up and runs what they point at; nothing changes while the game runs, and a
// registry that does not answer leaves what ran last.
func TestGameImagesArePinnedWhenTheGameStarts(t *testing.T) {
	st := reconStore(t)
	tmpl, err := st.UpsertTemplate(&models.Template{
		Slug: "pinned", Name: "Pinned", Startup: "run-game", DataPath: "/data",
		Images:  []models.TemplateImage{{Ref: "ghcr.io/eggs/yolks:java_21", Default: true}},
		Console: models.ConsoleConfig{Type: models.ConsoleAttach},
		Install: &models.InstallScript{Script: "echo install", Image: "ghcr.io/eggs/installers:alpine"},
	})
	if err != nil {
		t.Fatalf("template: %v", err)
	}
	srv := &models.Server{
		Slug: "pin", TemplateID: tmpl.ID, TemplateVersion: tmpl.Version, Image: "ghcr.io/eggs/yolks:java_21",
		Namespace: NamespaceFor("pin"), DesiredState: models.StateRunning,
		Resources: models.Resources{Memory: "1Gi"},
		Storage:   models.Storage{Type: models.StoragePVC, Size: "1Gi"},
	}
	if err := st.CreateServer(srv); err != nil {
		t.Fatalf("server: %v", err)
	}
	cl := fake.NewClientBuilder().WithScheme(clientgoscheme.Scheme).
		WithTypeConverters(managedfields.NewDeducedTypeConverter()).
		WithStatusSubresource(&appsv1.Deployment{}).Build()
	reg := &fakeRegistry{tags: map[string]string{
		"ghcr.io/eggs/yolks:java_21":     sha("a"),
		"ghcr.io/eggs/installers:alpine": sha("b"),
	}}
	r := New(cl, st)
	r.ResolveImage = reg.digest

	images := func() (game, install string) {
		t.Helper()
		var dep appsv1.Deployment
		if err := cl.Get(context.Background(), client.ObjectKey{Namespace: srv.Namespace, Name: workloadName}, &dep); err != nil {
			t.Fatalf("game Deployment: %v", err)
		}
		spec := dep.Spec.Template.Spec
		for _, c := range spec.InitContainers {
			if c.Name == InstallContainer {
				install = c.Image
			}
		}
		return spec.Containers[0].Image, install
	}
	setState := func(s models.DesiredState) {
		t.Helper()
		if err := st.SetDesiredState(srv.ID, s); err != nil {
			t.Fatalf("desired state: %v", err)
		}
		reconcileOK(t, r, srv.ID)
	}

	// The first start, its install included.
	reconcileOK(t, r, srv.ID)
	game, install := images()
	if game != "ghcr.io/eggs/yolks:java_21@"+sha("a") || install != "ghcr.io/eggs/installers:alpine@"+sha("b") {
		t.Fatalf("first start runs %s and %s", game, install)
	}
	gameUp(t, cl, srv)

	// The tag moves while the game runs: nothing is looked up, nothing rolls.
	reg.tags["ghcr.io/eggs/yolks:java_21"] = sha("c")
	before := reg.lookups
	reconcileOK(t, r, srv.ID)
	if game, _ := images(); game != "ghcr.io/eggs/yolks:java_21@"+sha("a") || reg.lookups != before {
		t.Fatalf("a running game was moved to %s (%d lookups)", game, reg.lookups-before)
	}

	// Stopped, it is not looked up either; started again, it takes the new one.
	setState(models.StateStopped)
	if reg.lookups != before {
		t.Errorf("a stopped server was looked up %d times", reg.lookups-before)
	}
	setState(models.StateRunning)
	if game, _ := images(); game != "ghcr.io/eggs/yolks:java_21@"+sha("c") {
		t.Errorf("the next start runs %s", game)
	}
	got, err := st.GetServer(srv.ID)
	if err != nil || got.ImagePins["ghcr.io/eggs/yolks:java_21"] != sha("c") {
		t.Errorf("pins kept = %v, %v", got.ImagePins, err)
	}

	// The registry is down at the next start: it runs what it ran last.
	setState(models.StateStopped)
	reg.down = true
	reg.tags["ghcr.io/eggs/yolks:java_21"] = sha("d")
	setState(models.StateRunning)
	if game, install := images(); game != "ghcr.io/eggs/yolks:java_21@"+sha("c") || install != "ghcr.io/eggs/installers:alpine@"+sha("b") {
		t.Errorf("with the registry down, the start runs %s and %s", game, install)
	}
}

// Without a pin -- no lookup made yet, a registry that never answered -- the
// tag runs as it did.
func TestRunImageWithoutAPin(t *testing.T) {
	s := &models.Server{ImagePins: map[string]string{"a:1": sha("a")}}
	for ref, want := range map[string]string{
		"a:1":           "a:1@" + sha("a"),
		"b:1":           "b:1",
		"c@" + sha("c"): "c@" + sha("c"),
	} {
		if got := s.RunImage(ref); got != want {
			t.Errorf("RunImage(%s) = %s, want %s", ref, got, want)
		}
	}
}
