package api

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
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

// A node that stops answering leaves its pods "running": nothing is there to
// say otherwise. The file manager exec'd into the data-manager and hung, or,
// once the pod was replaced by one that could not be scheduled, waited two
// minutes and blamed a restore. It now answers at once, and says why.
func TestFileAccessSaysWhenTheNodeIsGone(t *testing.T) {
	dataPod := func(name, node string, phase corev1.PodPhase, conds ...corev1.PodCondition) *corev1.Pod {
		p := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "quetzal-srv-s1", Labels: map[string]string{reconciler.DataLabel: "s1"}},
			Spec:       corev1.PodSpec{NodeName: node},
			Status:     corev1.PodStatus{Phase: phase, Conditions: conds},
		}
		if phase == corev1.PodRunning {
			p.Status.ContainerStatuses = []corev1.ContainerStatus{{
				Name: reconciler.WorkloadName, State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}},
			}}
		}
		return p
	}
	notReady := corev1.PodCondition{Type: corev1.PodReady, Status: corev1.ConditionFalse}
	unschedulable := func(since time.Duration) corev1.PodCondition {
		return corev1.PodCondition{Type: corev1.PodScheduled, Status: corev1.ConditionFalse, Reason: corev1.PodReasonUnschedulable,
			Message:            "0/3 nodes are available: 1 node(s) had volume node affinity conflict",
			LastTransitionTime: metav1.NewTime(time.Now().Add(-since))}
	}
	deadNode := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: "worker2"},
		Status:     corev1.NodeStatus{Conditions: []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionUnknown}}},
	}
	cases := []struct {
		name string
		objs []runtime.Object
		want string // in the error; "" when it should wait and time out instead
	}{
		{"on a node that stopped answering", []runtime.Object{deadNode, dataPod("dm", "worker2", corev1.PodRunning, notReady)}, "worker2, which holds this server's files, is not responding"},
		{"replaced by a pod no node can take", []runtime.Object{dataPod("dm2", "", corev1.PodPending, unschedulable(time.Minute))}, "volume node affinity conflict"},
		{"refused a moment ago, as a new volume can be", []runtime.Object{dataPod("dm3", "", corev1.PodPending, unschedulable(time.Second))}, ""},
	}
	for _, c := range cases {
		s, srv := offlineTestServer(t, c.objs...)
		s.DataReadyTimeout = 3 * time.Second
		start := time.Now()
		_, err := s.dataPodName(context.Background(), s.Clientset, srv.Namespace, srv.Slug)
		var unavailable errDataUnavailable
		switch {
		case c.want == "" && (err == nil || errors.As(err, &unavailable)):
			t.Errorf("%s: err = %v, want the usual wait and timeout", c.name, err)
		case c.want != "" && (!errors.As(err, &unavailable) || !strings.Contains(unavailable.msg, c.want)):
			t.Errorf("%s: err = %v, want it to say %q", c.name, err, c.want)
		case c.want != "" && time.Since(start) > time.Second:
			t.Errorf("%s: took %s to say so", c.name, time.Since(start))
		}
	}
}
