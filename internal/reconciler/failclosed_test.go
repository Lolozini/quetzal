package reconciler

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/managedfields"
	"k8s.io/apimachinery/pkg/util/validation/field"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	"github.com/lolozini/quetzal/internal/models"
	"github.com/lolozini/quetzal/internal/store"
)

const (
	poolMin   int32 = 30100
	poolMax   int32 = 30110
	takenPort int32 = 30105
)

// fakeCluster is a fake API server for a whole reconcile, answering through
// funcs. client-go's own type converter cannot apply a NetworkPolicy ("failed
// to merge config: expected objects with types from the same schema"), so the
// fake deduces the types instead, which is enough for what is asserted here.
func fakeCluster(funcs interceptor.Funcs) client.Client {
	return fake.NewClientBuilder().WithScheme(clientgoscheme.Scheme).
		WithTypeConverters(managedfields.NewDeducedTypeConverter()).
		WithInterceptorFuncs(funcs).Build()
}

// publishedServer stores a server published on a node port, holding takenPort
// for its game port as the pool would have handed it out.
func publishedServer(t *testing.T, st *store.Store) *models.Server {
	t.Helper()
	tmpl, err := st.UpsertTemplate(&models.Template{
		Slug: "demo", Name: "Demo", Startup: "sleep infinity", DataPath: "/data",
		Images:  []models.TemplateImage{{Ref: "alpine:3.24", Default: true}},
		Console: models.ConsoleConfig{Type: models.ConsoleAttach},
		Ports:   []models.PortSpec{{Name: "game", Port: 25565, Protocol: "TCP", Primary: true}},
	})
	if err != nil {
		t.Fatalf("template: %v", err)
	}
	srv := &models.Server{
		Slug: "np2", TemplateID: tmpl.ID, TemplateVersion: tmpl.Version, Image: "alpine:3.24",
		Namespace: NamespaceFor("np2"), DesiredState: models.StateRunning,
		Resources: models.Resources{Memory: "1Gi"},
		Storage:   models.Storage{Type: models.StoragePVC, Size: "1Gi"},
		Expose:    models.Expose{Type: models.ExposeNodePort},
	}
	if err := st.CreateServer(srv); err != nil {
		t.Fatalf("server: %v", err)
	}
	np, err := st.AllocateNodePort(srv.ID, store.NodePortKey(25565), takenPort, takenPort)
	if err != nil {
		t.Fatalf("allocate: %v", err)
	}
	ports := []models.PortSpec{
		{Name: "p25565-tcp", Port: 25565, Protocol: "TCP", Primary: true, NodePort: np},
		{Name: "p25565-udp", Port: 25565, Protocol: "UDP", NodePort: np},
	}
	if err := st.UpdateServerNetworking(srv.ID, srv.Expose, ports); err != nil {
		t.Fatalf("ports: %v", err)
	}
	srv.Ports = ports
	return srv
}

// refuse answers the patch of an object of kind K with err, as the apiserver
// would, when match says so.
func refuse[K client.Object](match func(K) error) interceptor.Funcs {
	return interceptor.Funcs{
		Patch: func(ctx context.Context, c client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
			if o, ok := obj.(K); ok {
				if err := match(o); err != nil {
					return err
				}
			}
			return c.Patch(ctx, obj, patch, opts...)
		},
	}
}

// nodePortTaken refuses a Service holding port as the apiserver does when it
// has already given that node port to another Service.
func nodePortTaken(port int32) interceptor.Funcs {
	return refuse(func(svc *corev1.Service) error {
		var errs field.ErrorList
		for i, p := range svc.Spec.Ports {
			if p.NodePort == port {
				errs = append(errs, field.Invalid(field.NewPath("spec", "ports").Index(i).Child("nodePort"), port, "provided port is already allocated"))
			}
		}
		if len(errs) == 0 {
			return nil
		}
		return apierrors.NewInvalid(schema.GroupKind{Kind: "Service"}, svc.Name, errs)
	})
}

func newTestReconciler(cl client.Client, st *store.Store) *Reconciler {
	r := New(cl, st)
	r.NodePortMin, r.NodePortMax = poolMin, poolMax
	return r
}

func get(t *testing.T, cl client.Client, ns, name string, obj client.Object) bool {
	t.Helper()
	err := cl.Get(context.Background(), types.NamespacedName{Namespace: ns, Name: name}, obj)
	if err != nil && !apierrors.IsNotFound(err) {
		t.Fatalf("get %s/%s: %v", ns, name, err)
	}
	return err == nil
}

func eventsOf(t *testing.T, st *store.Store, id uint, typ string) []models.Event {
	t.Helper()
	es, err := st.ListEventsForServer(id, 0, 100)
	if err != nil {
		t.Fatalf("events: %v", err)
	}
	var out []models.Event
	for _, e := range es {
		if e.Type == typ {
			out = append(out, e)
		}
	}
	return out
}

// The recette of 0.10.0 published a server on a node port a Service outside
// Quetzal already held: the cluster refused its Service on every pass, the
// pass stopped there, before the network policy, and the game ran with the run
// of the cluster -- panel and API server included -- while the panel showed it
// "Stopped". The port is now set aside and the server given another, in the
// same pass, policy first.
func TestTakenNodePortMovesTheServer(t *testing.T) {
	st := reconStore(t)
	srv := publishedServer(t, st)
	cl := fakeCluster(nodePortTaken(takenPort))
	r := newTestReconciler(cl, st)

	if err := r.ReconcileServer(context.Background(), srv.ID); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if !get(t, cl, srv.Namespace, "quetzal-default", &networkingv1.NetworkPolicy{}) {
		t.Fatal("no network policy")
	}
	var svc corev1.Service
	if !get(t, cl, srv.Namespace, workloadName, &svc) {
		t.Fatal("no Service")
	}
	moved := svc.Spec.Ports[0].NodePort
	if moved == takenPort || moved < poolMin || moved > poolMax {
		t.Fatalf("Service published on node port %d, want another port of %d-%d", moved, poolMin, poolMax)
	}
	for _, p := range svc.Spec.Ports {
		if p.NodePort != moved {
			t.Errorf("TCP and UDP split: %+v", svc.Spec.Ports)
		}
	}
	got, err := st.GetServer(srv.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range got.Ports {
		if p.NodePort != moved {
			t.Errorf("the server's row still publishes %s on %d, the Service on %d", p.Name, p.NodePort, moved)
		}
	}
	if !strings.HasSuffix(got.Status.Address, ":"+itoa(moved)) {
		t.Errorf("address shown %q, want the new node port %d", got.Status.Address, moved)
	}
	// The taken port stays out of the pool, for this server and the next.
	if _, err := st.AllocateNodePort(srv.ID+1, "p1", takenPort, takenPort); !errors.Is(err, store.ErrNoFreeNodePort) {
		t.Errorf("the taken port went back to the pool: %v", err)
	}
	if es := eventsOf(t, st, srv.ID, models.EventServerPortMoved); len(es) != 1 {
		t.Errorf("%d port-moved events, want 1", len(es))
	} else if !strings.Contains(es[0].Message, itoa(moved)) {
		t.Errorf("event %q does not give the new port", es[0].Message)
	}

	// The next pass leaves it where it is.
	if err := r.ReconcileServer(context.Background(), srv.ID); err != nil {
		t.Fatalf("second reconcile: %v", err)
	}
	if es := eventsOf(t, st, srv.ID, models.EventServerPortMoved); len(es) != 1 {
		t.Errorf("the port moved again: %d events", len(es))
	}
}

// SFTP draws from the same pool, and is moved the same way.
func TestTakenNodePortMovesSFTP(t *testing.T) {
	st := reconStore(t)
	srv := publishedServer(t, st)
	srv.SFTP.Enabled = true
	if err := st.UpdateServer(srv); err != nil {
		t.Fatal(err)
	}
	if _, err := st.AllocateNodePort(srv.ID, SFTPPortName, 30101, 30101); err != nil {
		t.Fatal(err)
	}
	cl := fakeCluster(nodePortTaken(30101))
	r := newTestReconciler(cl, st)
	r.ActivatorImage = "ghcr.io/lolozini/quetzal:test"

	if err := r.ReconcileServer(context.Background(), srv.ID); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	var svc corev1.Service
	if !get(t, cl, srv.Namespace, SFTPServiceName, &svc) {
		t.Fatal("no SFTP Service")
	}
	if np := svc.Spec.Ports[0].NodePort; np == 30101 || np < poolMin || np > poolMax {
		t.Errorf("SFTP on node port %d, want another port of the pool", np)
	}
	if es := eventsOf(t, st, srv.ID, models.EventServerPortMoved); len(es) != 1 {
		t.Errorf("%d port-moved events, want 1", len(es))
	}
}

// A Service refused for any other reason still stops the pass, but after the
// policy, and the status says why instead of keeping what an earlier pass
// wrote.
func TestRefusedServiceKeepsThePolicyAndSaysSo(t *testing.T) {
	st := reconStore(t)
	srv := publishedServer(t, st)
	forbidden := refuse(func(svc *corev1.Service) error {
		return apierrors.NewForbidden(schema.GroupResource{Resource: "services"}, svc.Name, errors.New("not today"))
	})
	cl := fakeCluster(forbidden)
	r := newTestReconciler(cl, st)

	if err := r.ReconcileServer(context.Background(), srv.ID); err == nil {
		t.Fatal("a refused Service did not fail the reconcile")
	}
	if !get(t, cl, srv.Namespace, "quetzal-default", &networkingv1.NetworkPolicy{}) {
		t.Fatal("the game's Deployment went in without its network policy")
	}
	got, err := st.GetServer(srv.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got.Status.Message, "Kubernetes refused this server's Service") || !strings.Contains(got.Status.Message, "not today") {
		t.Errorf("status message %q does not say the Service was refused", got.Status.Message)
	}
	if got.Status.Phase != models.PhaseStarting {
		t.Errorf("phase %q, want what the pods show (Starting)", got.Status.Phase)
	}
}

// Without its network policy, nothing that runs a tenant's code goes in.
func TestNoPolicyNoWorkload(t *testing.T) {
	st := reconStore(t)
	srv := publishedServer(t, st)
	noPolicy := refuse(func(np *networkingv1.NetworkPolicy) error {
		return apierrors.NewForbidden(schema.GroupResource{Group: "networking.k8s.io", Resource: "networkpolicies"}, np.Name, errors.New("denied"))
	})
	cl := fakeCluster(noPolicy)
	r := newTestReconciler(cl, st)

	if err := r.ReconcileServer(context.Background(), srv.ID); err == nil {
		t.Fatal("a refused network policy did not fail the reconcile")
	}
	for _, name := range []string{workloadName, DataDeployName} {
		if get(t, cl, srv.Namespace, name, &appsv1.Deployment{}) {
			t.Errorf("Deployment %s went in without a network policy", name)
		}
	}
	got, err := st.GetServer(srv.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got.Status.Message, "network policy") {
		t.Errorf("status message %q does not name the network policy", got.Status.Message)
	}
}

// A status is read by anyone allowed to see the server: what the API server
// answered is about the object and is shown, a connection error naming the
// cluster's address is not.
func TestFailureNoticeKeepsTheClusterAddressOut(t *testing.T) {
	transport := failed("Service", errors.New(`Patch "https://10.43.0.1:443/api/v1/namespaces/quetzal-srv-x/services/server": dial tcp 10.43.0.1:443: connect: connection refused`))
	if n := failureNotice(transport); strings.Contains(n, "10.43.0.1") {
		t.Errorf("notice gives the cluster's address: %q", n)
	}
	refused := failed("Service", apierrors.NewForbidden(schema.GroupResource{Resource: "services"}, "server", errors.New("nope")))
	if n := failureNotice(refused); !strings.Contains(n, "nope") || !strings.Contains(n, "Service") {
		t.Errorf("notice %q leaves out what Kubernetes answered", n)
	}
}

func itoa(n int32) string { return strconv.Itoa(int(n)) }
