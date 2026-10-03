package reconciler

import (
	"context"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

// Paper out of heap prints java.lang.OutOfMemoryError and exits 0: the recette
// of 0.10.0 read "Crashed — the game exited with code 0" (R-26), with nothing
// pointing at the memory. The message quotes the error the run's log ends
// with.
func TestACrashSaysWhatItsLogEndsWith(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Namespace: "ns", Name: "server-1", Labels: map[string]string{serverLabel: "mc"}},
		Status: corev1.PodStatus{Phase: corev1.PodRunning, ContainerStatuses: []corev1.ContainerStatus{{
			Name: workloadName, RestartCount: 3,
			State:                corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "CrashLoopBackOff"}},
			LastTerminationState: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 0, Reason: "Completed"}},
		}}},
	}
	var asked struct {
		pod      string
		previous bool
	}
	r := &Reconciler{
		Client: fake.NewClientBuilder().WithScheme(scheme).WithObjects(pod).Build(),
		LogTail: func(_ context.Context, _, p, _ string, previous bool, _ int64) (string, error) {
			asked.pod, asked.previous = p, previous
			return "[12:00:01 INFO]: Preparing spawn area: 84%\n" +
				"\x1b[31mException in thread \"Server thread\" java.lang.OutOfMemoryError: Java heap space\x1b[0m\n" +
				"[12:00:02 INFO]: Stopping server\n", nil
		},
	}
	h := r.inspectPods(context.Background(), "ns", "mc")
	if !h.crashloop || !strings.Contains(h.msg, "java.lang.OutOfMemoryError: Java heap space") || !strings.Contains(h.msg, "more memory") {
		t.Errorf("message %q, want the out-of-memory error and what to do", h.msg)
	}
	if strings.Contains(h.msg, "\x1b") {
		t.Errorf("message %q keeps the log's colour codes", h.msg)
	}
	if asked.pod != "server-1" || !asked.previous {
		t.Errorf("read %+v, want the previous run of server-1", asked)
	}

	// No log to read: the message as it was.
	r.LogTail = nil
	if h := r.inspectPods(context.Background(), "ns", "mc"); h.msg != "the game exited with code 0" {
		t.Errorf("without a log: %q", h.msg)
	}
}
