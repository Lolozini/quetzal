package reconciler

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/lolozini/quetzal/internal/models"
	"github.com/lolozini/quetzal/internal/store"
)

// On a cluster of three nodes the panel showed the first node's address for a
// server whose pods ran on the third. The Service keeps the player's address
// (externalTrafficPolicy: Local), so only the third answered, and the address
// players were given timed out. It is now the node of the server's pods.
func TestNodePortAddressIsThePodsNode(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatalf("scheme: %v", err)
	}
	node := func(name, ip string) *corev1.Node {
		return &corev1.Node{
			ObjectMeta: metav1.ObjectMeta{Name: name},
			Status:     corev1.NodeStatus{Addresses: []corev1.NodeAddress{{Type: corev1.NodeInternalIP, Address: ip}}},
		}
	}
	s, tmpl := testServerAndTemplate()
	s.Expose = models.Expose{Type: models.ExposeNodePort}
	s.Ports = []models.PortSpec{{Name: "game", Port: 25565, Protocol: "TCP", Primary: true, NodePort: 30150}}
	data := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Namespace: s.Namespace, Name: "data-manager-x", Labels: map[string]string{DataLabel: s.Slug}},
		Spec:       corev1.PodSpec{NodeName: "worker2"},
	}
	cl := fake.NewClientBuilder().WithScheme(scheme).
		WithObjects(node("control-plane", "172.18.0.4"), node("worker", "172.18.0.3"), node("worker2", "172.18.0.2"), data).
		Build()
	st := reconStore(t)
	r := &Reconciler{Client: cl, Store: st}
	ctx := context.Background()

	if _, addr := r.endpointsFor(ctx, s, tmpl); addr != "172.18.0.2:30150" {
		t.Errorf("address = %q, want the pods' node 172.18.0.2:30150", addr)
	}

	// Without the player's address kept, every node answers: any will do.
	no := false
	s.Expose.PreserveClientIP = &no
	if _, addr := r.endpointsFor(ctx, s, tmpl); addr != "172.18.0.4:30150" {
		t.Errorf("address = %q, want the first node 172.18.0.4:30150", addr)
	}
	s.Expose.PreserveClientIP = nil

	// A hostname the administrator set is theirs to point.
	if err := st.SetSetting(store.SettingEndpointHost, "play.example.com"); err != nil {
		t.Fatalf("setting: %v", err)
	}
	if _, addr := r.endpointsFor(ctx, s, tmpl); addr != "play.example.com:30150" {
		t.Errorf("address = %q, want the configured hostname", addr)
	}
}

// The Service selects the activator while the game sleeps and the game once
// it is up. When only the nodes running its pods answer, the two have to share
// the data-manager's node, or the address stops answering as the game wakes.
func TestActivatorFollowsTheGameWhenTrafficStaysLocal(t *testing.T) {
	s, tmpl := testServerAndTemplate()
	p := ActivatorParams{Image: "quetzal", WakeURL: "http://wake", Token: "t"}
	no := false
	cases := []struct {
		name   string
		expose models.Expose
		pinned bool
	}{
		{"node port", models.Expose{Type: models.ExposeNodePort}, true},
		{"load balancer", models.Expose{Type: models.ExposeLoadBalancer}, true},
		{"node port, any node", models.Expose{Type: models.ExposeNodePort, PreserveClientIP: &no}, false},
		{"cluster only", models.Expose{}, false},
	}
	for _, c := range cases {
		s.Expose = c.expose
		aff := BuildActivatorDeployment(s, tmpl, p).Spec.Template.Spec.Affinity
		pinned := aff != nil && aff.PodAffinity != nil && len(aff.PodAffinity.RequiredDuringSchedulingIgnoredDuringExecution) == 1 &&
			aff.PodAffinity.RequiredDuringSchedulingIgnoredDuringExecution[0].LabelSelector.MatchLabels[DataLabel] == s.Slug
		if pinned != c.pinned {
			t.Errorf("%s: activator on the data-manager's node = %v, want %v", c.name, pinned, c.pinned)
		}
	}
}

// A published IPv6 address was glued to its port as "fd00::1:30150", which
// reads as another address altogether. It is bracketed, as clients expect.
func TestAnIPv6AddressIsBracketed(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatalf("scheme: %v", err)
	}
	s, tmpl := testServerAndTemplate()
	s.Expose = models.Expose{Type: models.ExposeNodePort}
	s.Ports = []models.PortSpec{{Name: "game", Port: 25565, Protocol: "TCP", Primary: true, NodePort: 30150}}
	st := reconStore(t)
	if err := st.SetSetting(store.SettingEndpointHost, "fd00::1"); err != nil {
		t.Fatal(err)
	}
	r := &Reconciler{Client: fake.NewClientBuilder().WithScheme(scheme).Build(), Store: st}
	if _, addr := r.endpointsFor(context.Background(), s, tmpl); addr != "[fd00::1]:30150" {
		t.Errorf("address = %q, want [fd00::1]:30150", addr)
	}
}
