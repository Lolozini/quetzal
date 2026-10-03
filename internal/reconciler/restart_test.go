package reconciler

import (
	"context"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/managedfields"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/lolozini/quetzal/internal/models"
	"github.com/lolozini/quetzal/internal/store"
)

// stopRecorder stands in for the console: it records the stop commands sent.
type stopRecorder struct{ sent []string }

func (s *stopRecorder) onStop(_ context.Context, _, _, cmd string) error {
	s.sent = append(s.sent, cmd)
	return nil
}

// runningServer stores a running server on a template with a console stop
// command, and returns a reconciler on a fake cluster where its game is up.
func runningServer(t *testing.T) (*Reconciler, *store.Store, client.Client, *models.Server, *stopRecorder) {
	t.Helper()
	st := reconStore(t)
	tmpl, err := st.UpsertTemplate(&models.Template{
		Slug: "signal", Name: "Signal", Startup: "run-game", DataPath: "/data", StopCommand: "save-and-stop",
		Images:  []models.TemplateImage{{Ref: "alpine:3.20", Default: true}},
		Console: models.ConsoleConfig{Type: models.ConsoleAttach},
	})
	if err != nil {
		t.Fatalf("template: %v", err)
	}
	srv := &models.Server{
		Slug: "sig", TemplateID: tmpl.ID, TemplateVersion: tmpl.Version, Image: "alpine:3.20",
		Namespace: NamespaceFor("sig"), DesiredState: models.StateRunning,
		Resources: models.Resources{Memory: "1Gi"},
		Storage:   models.Storage{Type: models.StoragePVC, Size: "1Gi"},
	}
	if err := st.CreateServer(srv); err != nil {
		t.Fatalf("server: %v", err)
	}
	cl := fake.NewClientBuilder().WithScheme(clientgoscheme.Scheme).
		WithTypeConverters(managedfields.NewDeducedTypeConverter()).
		WithStatusSubresource(&appsv1.Deployment{}).Build()
	rec := &stopRecorder{}
	r := New(cl, st)
	r.OnStop = rec.onStop
	reconcileOK(t, r, srv.ID)
	gameUp(t, cl, srv)
	return r, st, cl, srv, rec
}

func reconcileOK(t *testing.T, r *Reconciler, id uint) {
	t.Helper()
	if err := r.ReconcileServer(context.Background(), id); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
}

// gameUp plays the Deployment controller and the kubelet: the game's pod
// exists and is ready.
func gameUp(t *testing.T, cl client.Client, srv *models.Server) {
	t.Helper()
	ctx := context.Background()
	var dep appsv1.Deployment
	if err := cl.Get(ctx, client.ObjectKey{Namespace: srv.Namespace, Name: workloadName}, &dep); err != nil {
		t.Fatalf("game Deployment: %v", err)
	}
	dep.Status.Replicas, dep.Status.ReadyReplicas, dep.Status.UpdatedReplicas = 1, 1, 1
	dep.Status.ObservedGeneration = dep.Generation
	if err := cl.Status().Update(ctx, &dep); err != nil {
		t.Fatalf("Deployment status: %v", err)
	}
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "server-1", Namespace: srv.Namespace, Labels: map[string]string{serverLabel: srv.Slug}}}
	if err := cl.Create(ctx, pod); err != nil {
		t.Fatalf("game pod: %v", err)
	}
}

// gameDown plays the kubelet once the pod has terminated.
func gameDown(t *testing.T, cl client.Client, srv *models.Server) {
	t.Helper()
	ctx := context.Background()
	if err := cl.Delete(ctx, &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "server-1", Namespace: srv.Namespace}}); err != nil {
		t.Fatalf("delete game pod: %v", err)
	}
	var dep appsv1.Deployment
	if err := cl.Get(ctx, client.ObjectKey{Namespace: srv.Namespace, Name: workloadName}, &dep); err != nil {
		t.Fatal(err)
	}
	dep.Status.Replicas, dep.Status.ReadyReplicas, dep.Status.UpdatedReplicas = 0, 0, 0
	if err := cl.Status().Update(ctx, &dep); err != nil {
		t.Fatal(err)
	}
}

func gameReplicas(t *testing.T, cl client.Client, srv *models.Server) int32 {
	t.Helper()
	var dep appsv1.Deployment
	if err := cl.Get(context.Background(), client.ObjectKey{Namespace: srv.Namespace, Name: workloadName}, &dep); err != nil {
		t.Fatal(err)
	}
	if dep.Spec.Replicas == nil {
		return 1
	}
	return *dep.Spec.Replicas
}

func gamePodSpecHash(t *testing.T, cl client.Client, srv *models.Server) string {
	t.Helper()
	var dep appsv1.Deployment
	if err := cl.Get(context.Background(), client.ObjectKey{Namespace: srv.Namespace, Name: workloadName}, &dep); err != nil {
		t.Fatal(err)
	}
	return dep.Annotations[podSpecAnnotation]
}

// A restart used to delete the game's pod: the game got SIGTERM and nothing
// else, and the next pod could start on the volume while the old one still
// wrote to it. It is a stop now, stop command first, and a start once the pod
// is gone.
func TestRestartStopsTheGameWithItsCommandThenStartsIt(t *testing.T) {
	r, st, cl, srv, rec := runningServer(t)
	if ok, err := st.RequestRestart(srv.ID, time.Now()); err != nil || !ok {
		t.Fatalf("request restart: %v %v", ok, err)
	}
	reconcileOK(t, r, srv.ID)
	if len(rec.sent) != 1 || rec.sent[0] != "save-and-stop" {
		t.Fatalf("stop commands sent: %q, want the template's", rec.sent)
	}
	if n := gameReplicas(t, cl, srv); n != 0 {
		t.Fatalf("replicas while the game stops = %d, want 0", n)
	}
	got, _ := st.GetServer(srv.ID)
	if got.Status.Phase != models.PhaseStopping {
		t.Errorf("phase = %s, want Stopping", got.Status.Phase)
	}

	// Still terminating: it waits.
	reconcileOK(t, r, srv.ID)
	if n := gameReplicas(t, cl, srv); n != 0 {
		t.Fatalf("started again while the old pod was still there")
	}
	// Gone: it starts.
	gameDown(t, cl, srv)
	reconcileOK(t, r, srv.ID)
	if n := gameReplicas(t, cl, srv); n != 1 {
		t.Fatalf("replicas once the game is down = %d, want 1", n)
	}
	if got, _ := st.GetServer(srv.ID); got.RestartRequestedAt != nil {
		t.Error("the restart is still pending")
	}
	if len(rec.sent) != 1 {
		t.Errorf("stop commands sent: %q, want one", rec.sent)
	}
}

// A pod that never goes does not keep the server down for good.
func TestRestartGivesUpWaitingForAPodThatStays(t *testing.T) {
	r, st, cl, srv, _ := runningServer(t)
	if _, err := st.RequestRestart(srv.ID, time.Now().Add(-restartLimit-time.Minute)); err != nil {
		t.Fatal(err)
	}
	reconcileOK(t, r, srv.ID)
	if n := gameReplicas(t, cl, srv); n != 1 {
		t.Errorf("replicas = %d, want the server started again", n)
	}
}

// A stopped server has nothing to restart.
func TestRestartOfAStoppedServerDoesNothing(t *testing.T) {
	_, st, _, srv, _ := runningServer(t)
	if err := st.SetDesiredState(srv.ID, models.StateStopped); err != nil {
		t.Fatal(err)
	}
	if ok, err := st.RequestRestart(srv.ID, time.Now()); err != nil || ok {
		t.Errorf("restart of a stopped server = %v %v, want refused", ok, err)
	}
}

// A new setting replaces the game's pod, and the game got SIGTERM alone: it
// gets its stop command first now. A pass that changes nothing sends nothing.
func TestAChangeThatReplacesThePodSendsTheStopCommandFirst(t *testing.T) {
	r, st, _, srv, rec := runningServer(t)
	reconcileOK(t, r, srv.ID)
	if len(rec.sent) != 0 {
		t.Fatalf("a pass with nothing to change sent %q", rec.sent)
	}
	if err := st.UpdateServerResources(srv.ID, models.Resources{Memory: "2Gi"}); err != nil {
		t.Fatal(err)
	}
	reconcileOK(t, r, srv.ID)
	if len(rec.sent) != 1 || rec.sent[0] != "save-and-stop" {
		t.Errorf("stop commands sent when the pod is replaced: %q", rec.sent)
	}
}

// An edited template used to replace the pod of every running server on it at
// once. A running server keeps the version it started with, and takes the new
// one when it stops.
func TestARunningServerKeepsItsTemplateUntilItStops(t *testing.T) {
	r, st, cl, srv, rec := runningServer(t)
	before := gamePodSpecHash(t, cl, srv)

	cur, err := st.GetTemplate(srv.TemplateID)
	if err != nil {
		t.Fatal(err)
	}
	cur.Startup = "run-game --v2"
	if _, err := st.UpsertTemplate(cur); err != nil {
		t.Fatal(err)
	}
	reconcileOK(t, r, srv.ID)
	if after := gamePodSpecHash(t, cl, srv); after != before {
		t.Fatal("the template's update replaced the running pod")
	}
	if len(rec.sent) != 0 {
		t.Errorf("stop commands sent: %q", rec.sent)
	}
	got, _ := st.GetServer(srv.ID)
	if got.TemplateVersion != 1 || !strings.Contains(got.Status.Message, "next restart") {
		t.Errorf("version %d, message %q: want 1, and the update announced for the next restart", got.TemplateVersion, got.Status.Message)
	}

	// Stopped, it takes the new version, and the old one is not kept.
	if err := st.SetDesiredState(srv.ID, models.StateStopped); err != nil {
		t.Fatal(err)
	}
	reconcileOK(t, r, srv.ID)
	got, _ = st.GetServer(srv.ID)
	if got.TemplateVersion != 2 {
		t.Errorf("version after a stop = %d, want 2", got.TemplateVersion)
	}
	if _, err := st.GetTemplateRevision(srv.TemplateID, 1); err == nil {
		t.Error("version 1 is still kept with nobody on it")
	}
	if after := gamePodSpecHash(t, cl, srv); after == before {
		t.Error("the stopped server's pod was not made from the new version")
	}
}

// A change of the server's own settings replaces its pod anyway: it takes the
// new template in the same stroke, rather than being replaced a second time.
func TestASettingChangeTakesTheNewTemplateAtOnce(t *testing.T) {
	r, st, _, srv, rec := runningServer(t)
	cur, err := st.GetTemplate(srv.TemplateID)
	if err != nil {
		t.Fatal(err)
	}
	cur.Startup = "run-game --v2"
	if _, err := st.UpsertTemplate(cur); err != nil {
		t.Fatal(err)
	}
	if err := st.UpdateServerResources(srv.ID, models.Resources{Memory: "2Gi"}); err != nil {
		t.Fatal(err)
	}
	reconcileOK(t, r, srv.ID)
	got, _ := st.GetServer(srv.ID)
	if got.TemplateVersion != 2 {
		t.Errorf("version = %d, want the new one taken with the new setting", got.TemplateVersion)
	}
	if len(rec.sent) != 1 {
		t.Errorf("stop commands sent: %q, want one for the one replacement", rec.sent)
	}
}
