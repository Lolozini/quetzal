package reconciler

import (
	"context"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/lolozini/quetzal/internal/models"
)

func waiting(name, image, reason, msg string) corev1.ContainerStatus {
	return corev1.ContainerStatus{Name: name, Image: image, State: corev1.ContainerState{
		Waiting: &corev1.ContainerStateWaiting{Reason: reason, Message: msg},
	}}
}

// A container that cannot start is not crashing and has not exited: nothing the
// reconciler looked at moved, so the server read "Starting" with no message while
// Kubernetes had the answer in the container's waiting state.
func TestStuckMessageSaysWhyAContainerCannotStart(t *testing.T) {
	for name, tc := range map[string]struct {
		cs   corev1.ContainerStatus
		want []string // every one must be in the message
	}{
		"no such tag": {
			waiting("server", "ghcr.io/x/game:9.9", "ErrImagePull", `rpc error: code = NotFound desc = failed to pull and unpack image "ghcr.io/x/game:9.9": not found`),
			[]string{"cannot pull the image", "ghcr.io/x/game:9.9", "does not exist", "retried"},
		},
		"private image": {
			waiting("install", "ghcr.io/x/private:1", "ImagePullBackOff", "failed to authorize: unauthorized: authentication required"),
			[]string{"install", "refused access"},
		},
		"rate limit": {
			waiting("server", "alpine:3.24", "ErrImagePull", "toomanyrequests: You have reached your pull rate limit"),
			[]string{"rate limit"},
		},
		"registry down": {
			waiting("server", "alpine:3.24", "ErrImagePull", "dial tcp: lookup registry-1.docker.io: no such host"),
			[]string{"cannot be reached"},
		},
		"invalid name": {
			waiting("server", "Not A Valid Name", "InvalidImageName", ""),
			[]string{"not valid", "Not A Valid Name"},
		},
		"missing secret": {
			waiting("server", "alpine", "CreateContainerConfigError", `secret "server-env" not found`),
			[]string{"cannot be configured", `secret "server-env" not found`},
		},
		"cannot create": {
			waiting("server", "alpine", "CreateContainerError", ""),
			[]string{"cannot be created", "no reason given"},
		},
	} {
		t.Run(name, func(t *testing.T) {
			got := stuckMessage(tc.cs)
			for _, w := range tc.want {
				if !strings.Contains(got, w) {
					t.Errorf("message %q does not contain %q", got, w)
				}
			}
		})
	}

	// A container that is merely starting up, running or finished is not stuck.
	for name, cs := range map[string]corev1.ContainerStatus{
		"creating":     waiting("server", "alpine", "ContainerCreating", ""),
		"initialising": waiting("server", "alpine", "PodInitializing", ""),
		"crash loop":   waiting("server", "alpine", "CrashLoopBackOff", "back-off"),
		"running":      {Name: "server", State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}}},
		"finished":     {Name: "install", State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{}}},
	} {
		if got := stuckMessage(cs); got != "" {
			t.Errorf("%s: a container that is not stuck got %q", name, got)
		}
	}
}

func stuckFixture(t *testing.T, objs ...runtime.Object) (*Reconciler, *models.Server, *models.Template) {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := appsv1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	st := reconStore(t)
	s, tmpl := testServerAndTemplate()
	if err := st.CreateServer(s); err != nil {
		t.Fatal(err)
	}
	cl := fake.NewClientBuilder().WithScheme(scheme).WithRuntimeObjects(objs...).Build()
	return &Reconciler{Client: cl, Store: st}, s, tmpl
}

func statusOf(t *testing.T, r *Reconciler, s *models.Server, tmpl *models.Template) models.Status {
	t.Helper()
	if err := r.updateStatus(context.Background(), s, tmpl); err != nil {
		t.Fatal(err)
	}
	got, err := r.Store.GetServer(s.ID)
	if err != nil {
		t.Fatal(err)
	}
	return got.Status
}

// The panel shows the server's message: an image that will not pull has to be in
// it, whichever container (the egg's install step or the game) is the one stuck.
func TestServerWhoseImageWillNotPullSaysSo(t *testing.T) {
	for name, pod := range map[string]corev1.PodStatus{
		"game": {ContainerStatuses: []corev1.ContainerStatus{
			waiting(workloadName, "ghcr.io/x/game:9.9", "ImagePullBackOff", "not found"),
		}},
		"install step": {
			InitContainerStatuses: []corev1.ContainerStatus{
				waiting("install", "ghcr.io/x/installer:1", "ErrImagePull", "not found"),
			},
			ContainerStatuses: []corev1.ContainerStatus{
				waiting(workloadName, "ghcr.io/x/game:1", "PodInitializing", ""),
			},
		},
	} {
		t.Run(name, func(t *testing.T) {
			p := &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{Namespace: "quetzal-srv-s1", Name: "p", Labels: map[string]string{serverLabel: "s1"}},
				Status:     pod,
			}
			r, s, tmpl := stuckFixture(t, p)
			got := statusOf(t, r, s, tmpl)
			if got.Phase != models.PhaseStarting {
				t.Errorf("phase = %s, want Starting (the pull is retried)", got.Phase)
			}
			if !strings.Contains(got.Message, "cannot pull the image") {
				t.Errorf("message = %q, want it to say the image cannot be pulled", got.Message)
			}
		})
	}
}

// With a full quota the Deployment never gets a pod, so there is no container to
// read: the reason is on the Deployment itself.
func TestServerWhoseQuotaIsFullSaysSo(t *testing.T) {
	dep := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Namespace: "quetzal-srv-s1", Name: workloadName},
		Status: appsv1.DeploymentStatus{Conditions: []appsv1.DeploymentCondition{{
			Type: appsv1.DeploymentReplicaFailure, Status: corev1.ConditionTrue, Reason: "FailedCreate",
			Message: `pods "server-7d9f" is forbidden: exceeded quota: quetzal-quota, requested: limits.memory=8Gi, used: limits.memory=2Gi, limited: limits.memory=8Gi`,
		}}},
	}
	r, s, tmpl := stuckFixture(t, dep)
	got := statusOf(t, r, s, tmpl)
	if got.Phase != models.PhaseStarting {
		t.Errorf("phase = %s, want Starting", got.Phase)
	}
	if !strings.Contains(got.Message, "exceeded quota") || !strings.Contains(got.Message, "refuses to create its pod") {
		t.Errorf("message = %q, want the quota as the reason", got.Message)
	}
}

// A healthy start must not pick up a stale message.
func TestAServerStartingNormallyHasNoStuckMessage(t *testing.T) {
	p := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Namespace: "quetzal-srv-s1", Name: "p", Labels: map[string]string{serverLabel: "s1"}},
		Status: corev1.PodStatus{ContainerStatuses: []corev1.ContainerStatus{
			waiting(workloadName, "alpine:3.24", "ContainerCreating", ""),
		}},
	}
	dep := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Namespace: "quetzal-srv-s1", Name: workloadName}}
	r, s, tmpl := stuckFixture(t, p, dep)
	got := statusOf(t, r, s, tmpl)
	if got.Phase != models.PhaseStarting || got.Message != "" {
		t.Errorf("phase/message = %s/%q, want Starting with none", got.Phase, got.Message)
	}
}
