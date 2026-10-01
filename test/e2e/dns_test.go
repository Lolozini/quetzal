//go:build e2e

package e2e

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/kubernetes"
	ctrlconfig "sigs.k8s.io/controller-runtime/pkg/client/config"

	"github.com/lolozini/quetzal/internal/models"
	"github.com/lolozini/quetzal/internal/reconciler"
)

// TestE2EDNSStaysWithTheClusterResolver runs a pod that answers on port 53 at a
// private address, and a server that resolves a cluster name and tries that
// port. The policy opened port 53 of every address to the servers, so the
// server reached the pod; it may query the cluster's resolver now, and nothing
// else there, and it must still resolve. The server also tries the pod on
// port 8053, which the policy never allowed: where that gets through, the
// cluster does not enforce NetworkPolicy and the test is skipped.
func TestE2EDNSStaysWithTheClusterResolver(t *testing.T) {
	ctx, _, st, rec := setup(t)
	cfg, _ := ctrlconfig.GetConfig()
	cs, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		t.Fatalf("clientset: %v", err)
	}

	// Its own name each run: the last run's may still be on its way out.
	ns := fmt.Sprintf("e2e-dns-decoy-%d", time.Now().Unix()%100000)
	if _, err := cs.CoreV1().Namespaces().Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}, metav1.CreateOptions{}); err != nil {
		t.Fatalf("namespace: %v", err)
	}
	t.Cleanup(func() { _ = cs.CoreV1().Namespaces().Delete(ctx, ns, metav1.DeleteOptions{}) })
	if _, err := cs.CoreV1().Pods(ns).Create(ctx, &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "decoy"},
		Spec: corev1.PodSpec{Containers: []corev1.Container{{
			Name: "decoy", Image: "alpine:3.20",
			Command: []string{"sh", "-c", "(while true; do echo pong | nc -l -p 53; done) & while true; do echo pong | nc -l -p 8053; done"},
		}}},
	}, metav1.CreateOptions{}); err != nil {
		t.Fatalf("decoy: %v", err)
	}
	var decoyIP string
	if err := wait.PollUntilContextTimeout(ctx, 2*time.Second, 2*time.Minute, true, func(ctx2 context.Context) (bool, error) {
		p, err := cs.CoreV1().Pods(ns).Get(ctx2, "decoy", metav1.GetOptions{})
		if err != nil || p.Status.Phase != corev1.PodRunning || p.Status.PodIP == "" {
			return false, nil
		}
		decoyIP = p.Status.PodIP
		return true, nil
	}); err != nil {
		t.Fatalf("decoy never ran: %v", err)
	}

	probe := fmt.Sprintf(`while true; do
if nslookup kubernetes.default.svc.cluster.local >/dev/null 2>&1; then d=RESOLVES; else d=NORESOLVE; fi
if nc -w 2 %[1]s 53 </dev/null | grep -q pong; then a=OPEN; else a=SHUT; fi
if nc -w 2 %[1]s 8053 </dev/null | grep -q pong; then b=OPEN; else b=SHUT; fi
echo "$d:$a:$b"; sleep 2; done`, decoyIP)
	saved, err := st.UpsertTemplate(&models.Template{
		Slug: "e2e-dns", Name: "e2e-dns", DataPath: "/data",
		Images:  []models.TemplateImage{{Ref: "alpine:3.20", Default: true}},
		Startup: probe,
		Console: models.ConsoleConfig{Type: models.ConsoleAttach},
	})
	if err != nil {
		t.Fatalf("template: %v", err)
	}
	srv := &models.Server{
		Slug: "e2e-dns", DisplayName: "e2e-dns", TemplateID: saved.ID, TemplateVersion: saved.Version,
		Image: "alpine:3.20", Namespace: reconciler.NamespaceFor("e2e-dns"),
		DesiredState: models.StateRunning, Storage: models.Storage{Type: models.StoragePVC, Size: "1Gi"},
	}
	if err := st.CreateServer(srv); err != nil {
		t.Fatalf("create: %v", err)
	}
	t.Cleanup(func() { _ = rec.DeleteServer(ctx, srv) })
	reconcileUntilRunning(ctx, t, rec, st, srv.ID)

	verdict := func() string {
		pods, err := cs.CoreV1().Pods(srv.Namespace).List(ctx, metav1.ListOptions{LabelSelector: reconciler.ServerLabel + "=" + srv.Slug})
		if err != nil || len(pods.Items) == 0 {
			return ""
		}
		tail := int64(3)
		raw, err := cs.CoreV1().Pods(srv.Namespace).GetLogs(pods.Items[0].Name,
			&corev1.PodLogOptions{Container: reconciler.WorkloadName, TailLines: &tail}).Do(ctx).Raw()
		if err != nil {
			return ""
		}
		lines := strings.Fields(string(raw))
		if len(lines) == 0 {
			return ""
		}
		return lines[len(lines)-1]
	}
	// The policy takes a moment to hold once the pod runs: wait for the port it
	// always denied to close before reading anything else.
	var last string
	for deadline := time.Now().Add(45 * time.Second); time.Now().Before(deadline); time.Sleep(2 * time.Second) {
		if last = verdict(); strings.HasSuffix(last, ":SHUT") {
			break
		}
	}
	if strings.HasSuffix(last, ":OPEN") {
		t.Skip("this cluster does not enforce NetworkPolicy: the server reached a private address on a port the policy never allowed")
	}
	if strings.Count(last, ":") != 2 {
		t.Fatalf("the server never reported (last: %q)", last)
	}
	// A few rounds, so a verdict from before the policy took hold is not the one read.
	time.Sleep(6 * time.Second)
	if last = verdict(); last != "RESOLVES:SHUT:SHUT" {
		t.Errorf("the server reports %q, want RESOLVES:SHUT:SHUT: it resolves through the cluster's DNS and reaches nothing else on port 53", last)
	}
}
