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

	phase, id, msg, _ := r.startupPhase(starting, withDone, up, time.Now())
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
	phase, id, msg, _ = r.startupPhase(starting, withDone, up, time.Now())
	if phase != models.PhaseRunning || id != "containerd://b" || msg != "" {
		t.Errorf("after the done line: %s %q %q, want Running on containerd://b", phase, id, msg)
	}

	// Once kept in the status, the same container is not asked about again.
	calls = nil
	running := &models.Server{Status: models.Status{Phase: models.PhaseRunning, StartedContainer: "containerd://b"}}
	if phase, _, _, _ := r.startupPhase(running, withDone, up, time.Now()); phase != models.PhaseRunning || len(calls) != 0 {
		t.Errorf("a started container: %s after %d calls, want Running without asking", phase, len(calls))
	}

	// A restart is a new container, which has to print its line again.
	seen = false
	restarted := up
	restarted.gameContainer = "containerd://c"
	if phase, _, _, _ := r.startupPhase(running, withDone, restarted, time.Now()); phase != models.PhaseStarting {
		t.Errorf("a restarted container: %s, want Starting", phase)
	}
}

func TestStartupPhaseFallbacks(t *testing.T) {
	up := podHealth{gamePod: "p", gameContainer: "containerd://b", gameStarted: time.Now().Add(-time.Minute)}
	never := func(string, string, string, time.Time, []startup.Matcher) bool { return false }
	r := &Reconciler{StartupSeen: never}
	starting := &models.Server{Status: models.Status{Phase: models.PhaseStarting}}

	t.Run("a template without done lines", func(t *testing.T) {
		phase, id, _, _ := r.startupPhase(starting, &models.Template{}, up, time.Now())
		if phase != models.PhaseRunning || id != "containerd://b" {
			t.Errorf("%s %q, want Running", phase, id)
		}
	})

	t.Run("an older template's single line", func(t *testing.T) {
		phase, _, _, _ := r.startupPhase(starting, &models.Template{DoneRegex: "Done ("}, up, time.Now())
		if phase != models.PhaseStarting {
			t.Errorf("%s, want Starting: the legacy field is a done line too", phase)
		}
	})

	t.Run("an invalid expression alone", func(t *testing.T) {
		phase, _, msg, _ := r.startupPhase(starting, &models.Template{Done: []string{"regex:(["}}, up, time.Now())
		if phase != models.PhaseRunning || !strings.Contains(msg, "invalid") {
			t.Errorf("%s %q, want Running with the reason", phase, msg)
		}
	})

	t.Run("reported Running before the check existed", func(t *testing.T) {
		legacy := &models.Server{Status: models.Status{Phase: models.PhaseRunning}}
		phase, id, _, _ := r.startupPhase(legacy, &models.Template{Done: []string{"Done ("}}, up, time.Now())
		if phase != models.PhaseRunning || id != "containerd://b" {
			t.Errorf("%s %q, want it kept Running and its container recorded", phase, id)
		}
	})

	t.Run("a done line that never comes", func(t *testing.T) {
		late := up
		late.gameStarted = time.Now().Add(-startupLimit - time.Minute)
		phase, _, msg, missed := r.startupPhase(starting, &models.Template{Done: []string{"Done ("}}, late, time.Now())
		if phase != models.PhaseRunning || !strings.Contains(msg, "out of date") || !missed {
			t.Errorf("%s %q missed=%v, want Running with a warning past the limit, and the miss kept", phase, msg, missed)
		}
	})

	t.Run("no game container in sight", func(t *testing.T) {
		phase, _, _, _ := r.startupPhase(starting, &models.Template{Done: []string{"Done ("}}, podHealth{}, time.Now())
		if phase != models.PhaseStarting {
			t.Errorf("%s, want the previous phase kept", phase)
		}
	})

	t.Run("no way to read the log", func(t *testing.T) {
		bare := &Reconciler{}
		if phase, _, _, _ := bare.startupPhase(starting, &models.Template{Done: []string{"Done ("}}, up, time.Now()); phase != models.PhaseRunning {
			t.Errorf("%s, want Running", phase)
		}
	})
}

// Counter-Strike 2 without a valid game server token takes players but never
// prints its done line, so every start showed it Starting for half an hour.
// After one start that ran out the wait, the next ones are reported Running as
// soon as the container is up, and the reason stays in view while it runs.
func TestStartupPhaseRemembersAMissedDoneLine(t *testing.T) {
	tpl := &models.Template{Done: []string{"Connection to Steam servers successful"}}
	seen := false
	asked := 0
	r := &Reconciler{StartupSeen: func(string, string, string, time.Time, []startup.Matcher) bool {
		asked++
		return seen
	}}

	// The first start runs out the wait.
	first := podHealth{gamePod: "p", gameContainer: "containerd://a", gameStarted: time.Now().Add(-startupLimit - time.Minute)}
	srv := &models.Server{Status: models.Status{Phase: models.PhaseStarting}}
	phase, id, msg, missed := r.startupPhase(srv, tpl, first, time.Now())
	if phase != models.PhaseRunning || !missed || !strings.Contains(msg, "never showed") {
		t.Fatalf("past the limit: %s %q missed=%v, want Running and the miss recorded", phase, msg, missed)
	}

	// The explanation stays while that container runs: it used to be gone at
	// the next reconcile, a few seconds after it appeared.
	srv.Status = models.Status{Phase: phase, StartedContainer: id, Message: msg, StartupMissed: missed}
	if phase, _, msg, missed := r.startupPhase(srv, tpl, first, time.Now()); phase != models.PhaseRunning || !missed || !strings.Contains(msg, "never showed") {
		t.Errorf("the next reconcile: %s %q missed=%v, want the message kept", phase, msg, missed)
	}

	// The server is stopped and started again: no half hour this time.
	srv.Status = models.Status{Phase: models.PhaseStopped, StartupMissed: true}
	next := podHealth{gamePod: "p2", gameContainer: "containerd://b", gameStarted: time.Now().Add(-10 * time.Second)}
	phase, id, msg, missed = r.startupPhase(srv, tpl, next, time.Now())
	if phase != models.PhaseRunning || id != "containerd://b" || !missed || !strings.Contains(msg, "last started") {
		t.Fatalf("the next start: %s %q %q missed=%v, want Running at once, saying why", phase, id, msg, missed)
	}

	// The line is still looked for, and once it shows the usual wait is back.
	srv.Status = models.Status{Phase: phase, StartedContainer: id, Message: msg, StartupMissed: missed}
	asked, seen = 0, true
	phase, _, msg, missed = r.startupPhase(srv, tpl, next, time.Now())
	if asked == 0 || phase != models.PhaseRunning || msg != "" || missed {
		t.Errorf("the line shows: asked %d times, %s %q missed=%v, want Running, no message, nothing missed", asked, phase, msg, missed)
	}
	srv.Status = models.Status{Phase: models.PhaseStopped}
	seen = false
	third := podHealth{gamePod: "p3", gameContainer: "containerd://c", gameStarted: time.Now()}
	if phase, _, msg, _ := r.startupPhase(srv, tpl, third, time.Now()); phase != models.PhaseStarting || !strings.Contains(msg, "waiting for") {
		t.Errorf("a start after the line came back: %s %q, want Starting until it shows", phase, msg)
	}
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

// The miss is about the next start, so it has to outlive the stop in between:
// the status is rebuilt on every pass, and a field left out of it is lost.
func TestAMissedDoneLineOutlivesTheStop(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatalf("scheme: %v", err)
	}
	st := reconStore(t)
	s, tmpl := testServerAndTemplate()
	s.DesiredState = models.StateStopped
	s.Status = models.Status{Phase: models.PhaseRunning, StartupMissed: true}
	if err := st.CreateServer(s); err != nil {
		t.Fatal(err)
	}
	r := &Reconciler{Client: fake.NewClientBuilder().WithScheme(scheme).Build(), Store: st}
	if err := r.updateStatus(context.Background(), s, tmpl); err != nil {
		t.Fatal(err)
	}
	got, err := st.GetServer(s.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status.Phase != models.PhaseStopped || !got.Status.StartupMissed {
		t.Errorf("after the stop: %s missed=%v, want Stopped with the miss kept", got.Status.Phase, got.Status.StartupMissed)
	}
}
