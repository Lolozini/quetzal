package api

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/rest"

	"github.com/lolozini/quetzal/internal/models"
	"github.com/lolozini/quetzal/internal/reconciler"
	"github.com/lolozini/quetzal/internal/store"
	"github.com/lolozini/quetzal/templates"
)

func offlineTestServer(t *testing.T, objs ...runtime.Object) (*Server, *models.Server) {
	t.Helper()
	st, err := store.Open(store.Config{Driver: store.DriverSQLite, DSN: filepath.Join(t.TempDir(), "off.db"), Silent: true})
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	if err := st.Migrate(); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	if err := templates.Seed(st); err != nil {
		t.Fatalf("seed: %v", err)
	}
	tmpl, err := st.GetTemplateBySlug("minecraft-paper")
	if err != nil {
		t.Fatalf("template: %v", err)
	}
	cs := fake.NewSimpleClientset(objs...)
	s := New(st, cs, &rest.Config{})
	srv := &models.Server{
		TemplateID:   tmpl.ID,
		Slug:         "s1",
		Namespace:    "quetzal-srv-s1",
		Image:        "itzg/minecraft-server:latest",
		DesiredState: models.StateStopped,
		Storage:      models.Storage{Type: models.StoragePVC, Size: "5Gi"},
	}
	return s, srv
}

// TestEnsureMaintPodReusesRunning verifies a ready maintenance pod is reused
// (not recreated) for offline file access.
func TestEnsureMaintPodReusesRunning(t *testing.T) {
	running := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: reconciler.MaintName, Namespace: "quetzal-srv-s1"},
		Status: corev1.PodStatus{
			Phase: corev1.PodRunning,
			ContainerStatuses: []corev1.ContainerStatus{{
				Name:  reconciler.WorkloadName,
				State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}},
			}},
		},
	}
	s, srv := offlineTestServer(t, running)
	cs := s.Clientset

	pod, err := s.ensureMaintPod(context.Background(), srv, cs)
	if err != nil {
		t.Fatalf("ensureMaintPod: %v", err)
	}
	if pod != reconciler.MaintName {
		t.Fatalf("pod = %q, want %q", pod, reconciler.MaintName)
	}
	// No second pod should have been created.
	list, _ := cs.CoreV1().Pods("quetzal-srv-s1").List(context.Background(), metav1.ListOptions{})
	if len(list.Items) != 1 {
		t.Fatalf("pod count = %d, want 1 (reused, not recreated)", len(list.Items))
	}
}

// TestEnsureMaintPodCreatesWhenAbsent verifies that with no maintenance pod, one
// is created (and, since the fake clientset never marks it running, the wait
// times out — proving the create path runs).
func TestEnsureMaintPodCreatesWhenAbsent(t *testing.T) {
	s, srv := offlineTestServer(t)
	s.MaintReadyTimeout = 60 * time.Millisecond
	cs := s.Clientset

	_, err := s.ensureMaintPod(context.Background(), srv, cs)
	if err == nil {
		t.Fatal("expected timeout error (fake pod never becomes ready)")
	}
	// The pod must have been created regardless.
	got, gerr := cs.CoreV1().Pods("quetzal-srv-s1").Get(context.Background(), reconciler.MaintName, metav1.GetOptions{})
	if gerr != nil {
		t.Fatalf("maintenance pod not created: %v", gerr)
	}
	if got.Labels[reconciler.MaintLabel] != "s1" {
		t.Errorf("created pod missing maint label: %v", got.Labels)
	}
}
