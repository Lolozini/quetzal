package reconciler

import (
	"context"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/lolozini/quetzal/internal/models"
	"github.com/lolozini/quetzal/internal/startup"
)

// A ready pod only means the container is up. The server used to be reported
// Running from that moment, while a Minecraft world was still loading: players
// were refused and "is up and running" went out a minute or two early. It is now
// Running once the game prints its done line, as with Pterodactyl.
func TestStartupPhaseWaitsForTheDoneLine(t *testing.T) {
	started := time.Now().Add(-time.Minute)
	up := podHealth{gamePod: "p", gameContainer: "containerd://b", gameStarted: started}
	withDone := &models.Template{Done: []string{"Done ("}}

	type call struct{ pod, id string }
	var calls []call
	seen := false
	r := &Reconciler{StartupSeen: func(ns, pod, id string, deadline time.Time, ms []startup.Matcher) bool {
		calls = append(calls, call{pod, id})
		if want := started.Add(startupLimit); !deadline.Equal(want) {
			t.Errorf("deadline = %v, want %v", deadline, want)
		}
		return seen
	}}
	starting := &models.Server{Namespace: "ns", Status: models.Status{Phase: models.PhaseStarting}}

	phase, id, msg := r.startupPhase(starting, withDone, up, time.Now())
	if phase != models.PhaseStarting || id != "" {
		t.Errorf("before the done line: %s %q, want Starting and no container", phase, id)
	}
	if !strings.Contains(msg, `"Done ("`) {
		t.Errorf("the message should say what is awaited, got %q", msg)
	}
	if len(calls) != 1 || calls[0] != (call{"p", "containerd://b"}) {
		t.Errorf("asked about %v, want the game container", calls)
	}

	seen = true
	phase, id, msg = r.startupPhase(starting, withDone, up, time.Now())
	if phase != models.PhaseRunning || id != "containerd://b" || msg != "" {
		t.Errorf("after the done line: %s %q %q, want Running on containerd://b", phase, id, msg)
	}

	// Once kept in the status, the same container is not asked about again.
	calls = nil
	running := &models.Server{Status: models.Status{Phase: models.PhaseRunning, StartedContainer: "containerd://b"}}
	if phase, _, _ := r.startupPhase(running, withDone, up, time.Now()); phase != models.PhaseRunning || len(calls) != 0 {
		t.Errorf("a started container: %s after %d calls, want Running without asking", phase, len(calls))
	}

	// A restart is a new container, which has to print its line again.
	seen = false
	restarted := up
	restarted.gameContainer = "containerd://c"
	if phase, _, _ := r.startupPhase(running, withDone, restarted, time.Now()); phase != models.PhaseStarting {
		t.Errorf("a restarted container: %s, want Starting", phase)
	}
}

func TestStartupPhaseFallbacks(t *testing.T) {
	up := podHealth{gamePod: "p", gameContainer: "containerd://b", gameStarted: time.Now().Add(-time.Minute)}
	never := func(string, string, string, time.Time, []startup.Matcher) bool { return false }
	r := &Reconciler{StartupSeen: never}
	starting := &models.Server{Status: models.Status{Phase: models.PhaseStarting}}

	t.Run("a template without done lines", func(t *testing.T) {
		phase, id, _ := r.startupPhase(starting, &models.Template{}, up, time.Now())
		if phase != models.PhaseRunning || id != "containerd://b" {
			t.Errorf("%s %q, want Running", phase, id)
		}
	})

	t.Run("an older template's single line", func(t *testing.T) {
		phase, _, _ := r.startupPhase(starting, &models.Template{DoneRegex: "Done ("}, up, time.Now())
		if phase != models.PhaseStarting {
			t.Errorf("%s, want Starting: the legacy field is a done line too", phase)
		}
	})

	t.Run("an invalid expression alone", func(t *testing.T) {
		phase, _, msg := r.startupPhase(starting, &models.Template{Done: []string{"regex:(["}}, up, time.Now())
		if phase != models.PhaseRunning || !strings.Contains(msg, "invalid") {
			t.Errorf("%s %q, want Running with the reason", phase, msg)
		}
	})

	t.Run("reported Running before the check existed", func(t *testing.T) {
		legacy := &models.Server{Status: models.Status{Phase: models.PhaseRunning}}
		phase, id, _ := r.startupPhase(legacy, &models.Template{Done: []string{"Done ("}}, up, time.Now())
		if phase != models.PhaseRunning || id != "containerd://b" {
			t.Errorf("%s %q, want it kept Running and its container recorded", phase, id)
		}
	})

	t.Run("a done line that never comes", func(t *testing.T) {
		late := up
		late.gameStarted = time.Now().Add(-startupLimit - time.Minute)
		phase, _, msg := r.startupPhase(starting, &models.Template{Done: []string{"Done ("}}, late, time.Now())
		if phase != models.PhaseRunning || !strings.Contains(msg, "out of date") {
			t.Errorf("%s %q, want Running with a warning past the limit", phase, msg)
		}
	})

	t.Run("no game container in sight", func(t *testing.T) {
		phase, _, _ := r.startupPhase(starting, &models.Template{Done: []string{"Done ("}}, podHealth{}, time.Now())
		if phase != models.PhaseStarting {
			t.Errorf("%s, want the previous phase kept", phase)
		}
	})

	t.Run("no way to read the log", func(t *testing.T) {
		bare := &Reconciler{}
		if phase, _, _ := bare.startupPhase(starting, &models.Template{Done: []string{"Done ("}}, up, time.Now()); phase != models.PhaseRunning {
			t.Errorf("%s, want Running", phase)
		}
	})
}

// The game container is the one named after the workload, running, in a pod
// that is not on its way out.
func TestInspectPodsFindsTheGameContainer(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatalf("scheme: %v", err)
	}
	at := metav1.NewTime(time.Now().Add(-time.Minute).Truncate(time.Second))
	running := func(name, id string) corev1.ContainerStatus {
		return corev1.ContainerStatus{Name: name, ContainerID: id, State: corev1.ContainerState{
			Running: &corev1.ContainerStateRunning{StartedAt: at},
		}}
	}
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Namespace: "ns", Name: "p", Labels: map[string]string{serverLabel: "srv"}},
		Status: corev1.PodStatus{ContainerStatuses: []corev1.ContainerStatus{
			running("sftp", "containerd://sidecar"),
			running(workloadName, "containerd://game"),
		}},
	}
	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(pod).Build()
	h := (&Reconciler{Client: cl}).inspectPods(context.Background(), "ns", "srv")
	if h.gamePod != "p" || h.gameContainer != "containerd://game" || !h.gameStarted.Equal(at.Time) {
		t.Errorf("game = %q %q %v, want p containerd://game %v", h.gamePod, h.gameContainer, h.gameStarted, at.Time)
	}
}
