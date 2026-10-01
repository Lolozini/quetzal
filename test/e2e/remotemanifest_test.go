//go:build e2e

package e2e

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"
	utilyaml "k8s.io/apimachinery/pkg/util/yaml"
	"sigs.k8s.io/controller-runtime/pkg/client"
	ctrlconfig "sigs.k8s.io/controller-runtime/pkg/client/config"

	"github.com/lolozini/quetzal/internal/cluster"
	"github.com/lolozini/quetzal/internal/models"
	"github.com/lolozini/quetzal/internal/reconciler"
)

// TestE2ERemoteClusterFromTheSetupManifest registers a cluster the way the docs
// say: apply the manifest the panel hands out, then give Quetzal a kubeconfig
// for the account it creates. In 0.5.0 the control plane bound the chart's role
// in each new namespace, which that account may not do, and no server ever ran
// on such a cluster. TestE2EMultiCluster missed it because it registers with an
// administrator's kubeconfig.
func TestE2ERemoteClusterFromTheSetupManifest(t *testing.T) {
	ctx, c, st, _ := setup(t)
	cfg, err := ctrlconfig.GetConfig()
	if err != nil {
		t.Fatalf("kube config: %v", err)
	}

	applyManifest(ctx, t, c, cluster.RemoteManifest())

	// What the kubeconfig script prints: the address the operator already
	// uses, and the token of the account the manifest created.
	var tok corev1.Secret
	err = wait.PollUntilContextTimeout(ctx, time.Second, time.Minute, true, func(ctx context.Context) (bool, error) {
		err := c.Get(ctx, client.ObjectKey{Namespace: cluster.RemoteNamespace, Name: cluster.RemoteServiceAccount + "-token"}, &tok)
		return err == nil && len(tok.Data["token"]) > 0, nil
	})
	if err != nil {
		t.Fatalf("the manifest's token was never issued: %v", err)
	}
	kubeconfig := fmt.Sprintf(`apiVersion: v1
kind: Config
clusters:
  - name: remote
    cluster:
      server: %s
      certificate-authority-data: %s
users:
  - name: %s
    user:
      token: %s
contexts:
  - name: remote
    context:
      cluster: remote
      user: %[3]s
current-context: remote
`, cfg.Host, base64.StdEncoding.EncodeToString(tok.Data["ca.crt"]), cluster.RemoteServiceAccount, tok.Data["token"])

	remote := &models.Cluster{Slug: "e2e-manifest", Name: "registered with the manifest"}
	if err := st.CreateCluster(remote, kubeconfig); err != nil {
		t.Fatalf("create cluster: %v", err)
	}
	clients, err := cluster.New(st, cluster.Clients{}).For(remote.ID)
	if err != nil {
		t.Fatalf("clients for the remote cluster: %v", err)
	}

	gen, err := st.GetTemplateBySlug("generic-process")
	if err != nil {
		t.Fatalf("template: %v", err)
	}
	srv := &models.Server{
		Slug: "e2e-manifest-srv", DisplayName: "manifest", TemplateID: gen.ID, TemplateVersion: gen.Version,
		Image: defaultImage(gen), Namespace: reconciler.NamespaceFor("e2e-manifest-srv"),
		ClusterID: remote.ID, DesiredState: models.StateRunning,
		Env:     map[string]string{"MESSAGE": "hi"},
		Storage: models.Storage{Type: models.StoragePVC, Size: "1Gi"},
	}
	if err := st.CreateServer(srv); err != nil {
		t.Fatalf("create server: %v", err)
	}

	// Wired as the controller wires it; the local role name must not matter.
	rec := reconciler.New(clients.Client, st)
	rec.NamespacedRole = cluster.NamespacedRole(remote, "quetzal-namespaced")
	t.Cleanup(func() { _ = rec.DeleteServer(ctx, srv) })
	reconcileUntilRunning(ctx, t, rec, st, srv.ID)

	var rb rbacv1.RoleBinding
	if err := c.Get(ctx, client.ObjectKey{Namespace: srv.Namespace, Name: reconciler.RoleBindingName}, &rb); err != nil {
		t.Fatalf("role binding: %v", err)
	}
	if rb.RoleRef.Name != cluster.RemoteNamespacedRole {
		t.Errorf("bound %q, want the manifest's %q", rb.RoleRef.Name, cluster.RemoteNamespacedRole)
	}

	// And nothing beyond its own namespaces, as the account the operator hands
	// over. Each of these reaches past them; the server above shows that what
	// Quetzal does need still works.
	cs := clients.Clientset
	self := rbacv1.Subject{Kind: "ServiceAccount", Name: cluster.RemoteServiceAccount, Namespace: cluster.RemoteNamespace}
	binding := func(ns string, subject rbacv1.Subject) *rbacv1.RoleBinding {
		return &rbacv1.RoleBinding{
			ObjectMeta: metav1.ObjectMeta{Name: "e2e-reach", Namespace: ns},
			RoleRef:    rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "ClusterRole", Name: cluster.RemoteNamespacedRole},
			Subjects:   []rbacv1.Subject{subject},
		}
	}
	refused := func(what string, err error) {
		t.Helper()
		if err == nil {
			t.Errorf("%s: allowed", what)
		}
	}
	_, err = cs.RbacV1().RoleBindings("kube-system").Create(ctx, binding("kube-system", self), metav1.CreateOptions{})
	refused("binding its role to itself in kube-system", err)
	everyone := rbacv1.Subject{Kind: "Group", APIGroup: rbacv1.GroupName, Name: "system:authenticated"}
	_, err = cs.RbacV1().RoleBindings(srv.Namespace).Create(ctx, binding(srv.Namespace, everyone), metav1.CreateOptions{})
	refused("binding its role to every authenticated user, in its own namespace", err)

	canary := binding("kube-system", rbacv1.Subject{Kind: "User", APIGroup: rbacv1.GroupName, Name: "e2e-nobody"})
	canary.RoleRef = rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "ClusterRole", Name: "view"}
	if err := c.Create(ctx, canary); err != nil {
		t.Fatalf("canary binding: %v", err)
	}
	t.Cleanup(func() { _ = c.Delete(context.Background(), canary) })
	err = cs.RbacV1().RoleBindings("kube-system").Delete(ctx, canary.Name, metav1.DeleteOptions{})
	refused("deleting a binding in kube-system", err)

	_, err = cs.CoreV1().Namespaces().Patch(ctx, "kube-system", types.MergePatchType,
		[]byte(`{"metadata":{"labels":{"e2e-reach":"yes"}}}`), metav1.PatchOptions{})
	refused("labelling kube-system", err)
	stray := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "e2e-not-quetzal"}}
	if _, err := cs.CoreV1().Namespaces().Create(ctx, stray, metav1.CreateOptions{}); err == nil {
		t.Errorf("creating a namespace of another name: allowed")
		_ = c.Delete(context.Background(), stray)
	}

	// Volumes are not its business at all: a PersistentVolume's claimRef is
	// what decides whose data a claim mounts.
	_, err = cs.CoreV1().PersistentVolumes().List(ctx, metav1.ListOptions{})
	if !apierrors.IsForbidden(err) {
		t.Errorf("listing PersistentVolumes: got %v, want forbidden", err)
	}
}

// applyManifest applies a multi-document manifest as an operator would, and
// deletes what it created when the test ends.
func applyManifest(ctx context.Context, t *testing.T, c client.Client, manifest string) {
	t.Helper()
	var applied []*unstructured.Unstructured
	t.Cleanup(func() {
		for i := len(applied) - 1; i >= 0; i-- {
			_ = c.Delete(context.Background(), applied[i])
		}
	})
	dec := utilyaml.NewYAMLOrJSONDecoder(strings.NewReader(manifest), 4096)
	for {
		u := &unstructured.Unstructured{}
		err := dec.Decode(&u.Object)
		if errors.Is(err, io.EOF) {
			return
		}
		if err != nil {
			t.Fatalf("decode manifest: %v", err)
		}
		if len(u.Object) == 0 {
			continue
		}
		if err := c.Patch(ctx, u, client.Apply, client.FieldOwner("e2e"), client.ForceOwnership); err != nil {
			t.Fatalf("apply %s %s: %v", u.GetKind(), u.GetName(), err)
		}
		applied = append(applied, u)
	}
}
