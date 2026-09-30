//go:build e2e

package e2e

import (
	"fmt"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/client-go/kubernetes"
	ctrlconfig "sigs.k8s.io/controller-runtime/pkg/client/config"

	"github.com/lolozini/quetzal/internal/models"
	"github.com/lolozini/quetzal/internal/reconciler"
)

// TestE2EServerReachesItsExternalDatabase puts a stand-in database in a
// namespace of its own, registers it as an external host by its Service's
// name, and runs a server that tries its database port and another port of
// it. A database on such a host was out of reach of the servers given one:
// only a host named by a literal address got a way through. The server now
// reaches the database port once it has a database there, and nothing else.
// It needs a CNI that enforces NetworkPolicy, and is skipped elsewhere.
func TestE2EServerReachesItsExternalDatabase(t *testing.T) {
	ctx, _, st, rec := setup(t)
	cfg, _ := ctrlconfig.GetConfig()
	cs, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		t.Fatalf("clientset: %v", err)
	}

	// A name of its own per run: a namespace takes a while to go away.
	ns := fmt.Sprintf("e2e-dbhost-%d", time.Now().Unix()%100000)
	labels := map[string]string{"app": "fakedb"}
	if _, err := cs.CoreV1().Namespaces().Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}, metav1.CreateOptions{}); err != nil {
		t.Fatalf("namespace: %v", err)
	}
	t.Cleanup(func() { _ = cs.CoreV1().Namespaces().Delete(ctx, ns, metav1.DeleteOptions{}) })
	if _, err := cs.CoreV1().Pods(ns).Create(ctx, &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "fakedb", Labels: labels},
		Spec: corev1.PodSpec{Containers: []corev1.Container{{
			Name: "db", Image: "alpine:3.20",
			Command: []string{"sh", "-c", "(while true; do echo db | nc -l -p 3306; done) & while true; do echo admin | nc -l -p 8080; done"},
		}}},
	}, metav1.CreateOptions{}); err != nil {
		t.Fatalf("pod: %v", err)
	}
	if _, err := cs.CoreV1().Services(ns).Create(ctx, &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: "fakedb"},
		Spec: corev1.ServiceSpec{Selector: labels, Ports: []corev1.ServicePort{
			{Name: "db", Port: 3306, TargetPort: intstr.FromInt32(3306)},
			{Name: "admin", Port: 8080, TargetPort: intstr.FromInt32(8080)},
		}},
	}, metav1.CreateOptions{}); err != nil {
		t.Fatalf("service: %v", err)
	}
	host := "fakedb." + ns + ".svc.cluster.local"
	h := &models.DatabaseHost{Name: "fake", Kind: models.DBHostExternal, Host: host, Port: 3306, AdminUser: "root"}
	if err := st.CreateDatabaseHost(h, "unused"); err != nil {
		t.Fatalf("database host: %v", err)
	}

	probe := func(port, answer string) string {
		return "if nc -w 2 " + host + " " + port + " </dev/null | grep -q " + answer + "; then printf " + answer + "=yes; else printf " + answer + "=no; fi"
	}
	saved, err := st.UpsertTemplate(&models.Template{
		Slug: "e2e-dbegress", Name: "e2e dbegress", DataPath: "/data",
		Images:  []models.TemplateImage{{Ref: "alpine:3.20", Default: true}},
		Startup: "while true; do " + probe("3306", "db") + "; printf ,; " + probe("8080", "admin") + "; echo; sleep 2; done",
		Console: models.ConsoleConfig{Type: models.ConsoleAttach},
	})
	if err != nil {
		t.Fatalf("template: %v", err)
	}
	srv := &models.Server{
		Slug: "e2e-dbegress", DisplayName: "dbegress", TemplateID: saved.ID, TemplateVersion: saved.Version,
		Image: "alpine:3.20", Namespace: reconciler.NamespaceFor("e2e-dbegress"),
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
	waitFor := func(want string, within time.Duration) bool {
		for deadline := time.Now().Add(within); time.Now().Before(deadline); time.Sleep(2 * time.Second) {
			if verdict() == want {
				return true
			}
		}
		return false
	}

	if !waitFor("db=no,admin=no", 90*time.Second) {
		if v := verdict(); strings.Contains(v, "=yes") {
			t.Skipf("this cluster does not enforce NetworkPolicy: %s before the server had a database there", v)
		}
		t.Fatalf("the server never tried the host (last: %q)", verdict())
	}

	if err := st.CreateServerDatabase(&models.ServerDatabase{ServerID: srv.ID, HostID: h.ID, DatabaseName: "s1", Username: "u1", Remote: "%"}, "pw"); err != nil {
		t.Fatalf("database: %v", err)
	}
	if err := rec.ReconcileServer(ctx, srv.ID); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if !waitFor("db=yes,admin=no", time.Minute) {
		t.Errorf("with a database on the host: %q, want its database port and nothing else", verdict())
	}
}
