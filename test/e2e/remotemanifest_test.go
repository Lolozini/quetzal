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
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
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
