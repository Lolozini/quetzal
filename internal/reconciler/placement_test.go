package reconciler

import (
	"context"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

// When the node holding a server's data stopped answering, the panel went on
// showing "Hibernated" or "Starting" without a word. The status now says what
// holds it back.
func TestStatusSaysWhatHoldsTheServerBack(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatalf("scheme: %v", err)
	}
	s, _ := testServerAndTemplate()
	node := func(name string, ready corev1.ConditionStatus) *corev1.Node {
		return &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: name},
			Status: corev1.NodeStatus{Conditions: []corev1.NodeCondition{{Type: corev1.NodeReady, Status: ready}}}}
	}
	pod := func(name string, labels map[string]string, nodeName string, phase corev1.PodPhase, conds ...corev1.PodCondition) *corev1.Pod {
		return &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Namespace: s.Namespace, Name: name, Labels: labels},
			Spec:       corev1.PodSpec{NodeName: nodeName},
			Status:     corev1.PodStatus{Phase: phase, Conditions: conds},
		}
	}
	unschedulable := corev1.PodCondition{Type: corev1.PodScheduled, Status: corev1.ConditionFalse, Reason: corev1.PodReasonUnschedulable,
		Message: "0/3 nodes are available: 3 node(s) had volume node affinity conflict"}
	data := map[string]string{DataLabel: s.Slug}
	game := map[string]string{serverLabel: s.Slug}

	cases := []struct {
		name string
		objs []client.Object
		want string
	}{
		{"all well", []client.Object{node("n1", corev1.ConditionTrue), pod("dm", data, "n1", corev1.PodRunning)}, ""},
		{"its node stopped answering", []client.Object{node("n2", corev1.ConditionUnknown), pod("dm", data, "n2", corev1.PodRunning)},
			"the node n2, which holds its data, is not responding"},
		{"its data cannot be mounted anywhere", []client.Object{pod("dm", data, "", corev1.PodPending, unschedulable)},
			"no node can hold its data: 0/3 nodes are available"},
		{"its game cannot be scheduled", []client.Object{node("n1", corev1.ConditionTrue), pod("dm", data, "n1", corev1.PodRunning),
			pod("game", game, "", corev1.PodPending, unschedulable)}, "no node can run it: 0/3 nodes are available"},
	}
	for _, c := range cases {
		r := &Reconciler{Client: fake.NewClientBuilder().WithScheme(scheme).WithObjects(c.objs...).Build()}
		h := r.inspectPods(context.Background(), s.Namespace, s.Slug)
		got := r.placementProblem(context.Background(), s, h.unscheduled)
		if c.want == "" && got != "" || c.want != "" && !strings.Contains(got, c.want) {
			t.Errorf("%s: %q, want %q", c.name, got, c.want)
		}
	}
}
