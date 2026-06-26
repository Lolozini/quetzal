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

// TestDataPodNameReturnsRunning verifies file access finds the always-on
// data-manager pod (by DataLabel) when its container is running.
func TestDataPodNameReturnsRunning(t *testing.T) {
	running := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "data-manager-abc",
			Namespace: "quetzal-srv-s1",
			Labels:    map[string]string{reconciler.DataLabel: "s1"},
		},
		Status: corev1.PodStatus{
			Phase: corev1.PodRunning,
			ContainerStatuses: []corev1.ContainerStatus{{
				Name:  reconciler.WorkloadName,
				State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}},
			}},
		},
	}
	s, srv := offlineTestServer(t, running)
	pod, err := s.dataPodName(context.Background(), s.Clientset, srv.Namespace, srv.Slug)
	if err != nil {
		t.Fatalf("dataPodName: %v", err)
	}
	if pod != "data-manager-abc" {
		t.Fatalf("pod = %q, want data-manager-abc", pod)
	}
}

// TestDataPodNameTimesOutWhenAbsent verifies file access reports unavailability
// when no data-manager pod is ready (e.g. during a restore, when the reconciler
// has scaled it to zero).
func TestDataPodNameTimesOutWhenAbsent(t *testing.T) {
	s, srv := offlineTestServer(t)
	s.DataReadyTimeout = 60 * time.Millisecond
	if _, err := s.dataPodName(context.Background(), s.Clientset, srv.Namespace, srv.Slug); err == nil {
		t.Fatal("expected timeout error when no data-manager pod is ready")
	}
}
