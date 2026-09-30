package reconciler

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/lolozini/quetzal/internal/models"
	"github.com/lolozini/quetzal/internal/store"
)

func reconStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open(store.Config{
		Driver:    store.DriverSQLite,
		DSN:       filepath.Join(t.TempDir(), "recon.db"),
		Silent:    true,
		SecretKey: []byte("0123456789abcdef0123456789abcdef"),
	})
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	if err := st.Migrate(); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return st
}

func TestInspectPodsDetectsOOM(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatalf("scheme: %v", err)
	}
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Namespace: "ns", Name: "p", Labels: map[string]string{serverLabel: "srv"}},
		Status: corev1.PodStatus{ContainerStatuses: []corev1.ContainerStatus{{
			Name:         "game",
			RestartCount: 3,
			LastTerminationState: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{
				Reason: "OOMKilled", ExitCode: 137,
			}},
		}}},
	}
	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(pod).Build()
	r := &Reconciler{Client: cl}

	h := r.inspectPods(context.Background(), "ns", "srv")
	if h.restarts != 3 {
		t.Errorf("restarts = %d, want 3", h.restarts)
	}
	if !h.oomKilled || h.termReason != "OOMKilled" || h.exitCode != 137 {
		t.Errorf("OOM not detected: %+v", h)
	}
	if h.crashloop {
		t.Errorf("crashloop should be false for a plain restart loop")
	}
}

func TestEmitRestartEvents(t *testing.T) {
	st := reconStore(t)
	r := &Reconciler{Store: st}
	s := &models.Server{ID: 1, Slug: "srv"}

	countEvents := func() []models.Event {
		es, err := st.ListEventsForServer(s.ID, 0, 100)
		if err != nil {
			t.Fatalf("list events: %v", err)
		}
		return es
	}

	// A new OOM restart (0 -> 1) is recorded as an OOMKilled event.
	r.emitRestartEvents(s, models.Status{CrashCount: 0}, podHealth{restarts: 1, oomKilled: true, termReason: "OOMKilled"}, 1)
	es := countEvents()
	if len(es) != 1 || es[0].Type != models.EventServerOOMKilled || !strings.Contains(es[0].Message, "OOMKilled") {
		t.Fatalf("want one OOMKilled event, got %+v", es)
	}

	// No growth (1 -> 1): nothing new.
	r.emitRestartEvents(s, models.Status{CrashCount: 1}, podHealth{restarts: 1, oomKilled: true}, 1)
	if es := countEvents(); len(es) != 1 {
		t.Fatalf("stable count should not emit, got %d events", len(es))
	}

	// A non-OOM exit (1 -> 2) is a generic restart event carrying the exit code.
	r.emitRestartEvents(s, models.Status{CrashCount: 1}, podHealth{restarts: 2, exitCode: 1}, 2)
	es = countEvents()
	if len(es) != 2 || es[0].Type != models.EventServerRestarted || !strings.Contains(es[0].Message, "code 1") {
		t.Fatalf("want a restarted event with exit code, got %+v", es)
	}

	// A crashloop already emits server.crashed via the phase transition, so the
	// restart path must not double up (2 -> 3, crashloop, no OOM).
	r.emitRestartEvents(s, models.Status{CrashCount: 2}, podHealth{restarts: 3, crashloop: true}, 3)
	if es := countEvents(); len(es) != 2 {
		t.Fatalf("crashloop restart should be suppressed, got %d events", len(es))
	}
}

// Kubernetes 1.35 reports a game that failed as terminated through the short
// back-offs (10 s to 80 s) and says CrashLoopBackOff only once they reach
// minutes. The server stayed "Starting" for five minutes, five failed starts,
// before it was "Crashed". A container down after a failed run is now a crash
// however the kubelet words the wait; one killed with its pod is not.
func TestInspectPodsSeesACrashBetweenRestarts(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatalf("scheme: %v", err)
	}
	now := metav1.Now()
	pod := func(name string, phase corev1.PodPhase, deleting bool, st corev1.ContainerState, last corev1.ContainerState) *corev1.Pod {
		p := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Namespace: "ns", Name: name, Labels: map[string]string{serverLabel: "srv"}},
			Status: corev1.PodStatus{Phase: phase, ContainerStatuses: []corev1.ContainerStatus{{
				Name: workloadName, RestartCount: 2, State: st, LastTerminationState: last,
			}}},
		}
		if deleting {
			p.DeletionTimestamp, p.Finalizers = &now, []string{"test"}
		}
		return p
	}
	exited := func(code int32, reason string) corev1.ContainerState {
		return corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: code, Reason: reason}}
	}
	backoff := corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{
		Reason: "CrashLoopBackOff", Message: "back-off 2m40s restarting failed container=server pod=server-abc_ns(uid)",
	}}
	cases := []struct {
		name    string
		pod     *corev1.Pod
		crashed bool
		msg     string
	}{
		{"terminated between restarts (1.35)", pod("p", corev1.PodRunning, false, exited(1, "Error"), corev1.ContainerState{}), true, "the game exited with code 1"},
		{"back-off (1.33)", pod("p", corev1.PodRunning, false, backoff, exited(1, "Error")), true, "the game exited with code 1"},
		{"out of memory", pod("p", corev1.PodRunning, false, exited(137, "OOMKilled"), corev1.ContainerState{}), true, "the game ran out of memory (OOMKilled)"},
		{"a clean exit restarts", pod("p", corev1.PodRunning, false, exited(0, "Completed"), corev1.ContainerState{}), false, ""},
		{"stopped with its pod", pod("p", corev1.PodRunning, true, exited(143, "Error"), corev1.ContainerState{}), false, ""},
		{"evicted", pod("p", corev1.PodFailed, false, exited(137, "Error"), corev1.ContainerState{}), false, ""},
	}
	for _, c := range cases {
		cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(c.pod).Build()
		h := (&Reconciler{Client: cl}).inspectPods(context.Background(), "ns", "srv")
		if h.crashloop != c.crashed || h.msg != c.msg {
			t.Errorf("%s: crashed=%v msg=%q, want %v %q", c.name, h.crashloop, h.msg, c.crashed, c.msg)
		}
	}
}
