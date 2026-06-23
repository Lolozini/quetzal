//go:build e2e

package e2e

import (
	"crypto/ed25519"
	"os"
	"strings"
	"testing"

	"golang.org/x/crypto/ssh"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/lolozini/quetzal/internal/models"
	"github.com/lolozini/quetzal/internal/reconciler"
)

// TestE2ESFTP brings up a server with the SFTP sidecar enabled and verifies the
// pod becomes Ready (which requires the copied sftp binary to run, load its host
// key and bind) and that the supporting objects exist. Needs QUETZAL_E2E_IMAGE.
func TestE2ESFTP(t *testing.T) {
	image := os.Getenv("QUETZAL_E2E_IMAGE")
	if image == "" {
		t.Skip("QUETZAL_E2E_IMAGE not set (the sftp sidecar needs the Quetzal image in-cluster)")
	}
	ctx, c, st, rec := setup(t)
	rec.ActivatorImage = image

	// An owner with a registered SSH key.
	owner := &models.User{Username: "alice", PasswordHash: "x"}
	if err := st.CreateUser(owner); err != nil {
		t.Fatalf("create user: %v", err)
	}
	pub, _, _ := ed25519.GenerateKey(nil)
	sshPub, _ := ssh.NewPublicKey(pub)
	line := strings.TrimSpace(string(ssh.MarshalAuthorizedKey(sshPub)))
	if err := st.AddSSHKey(&models.SSHKey{UserID: owner.ID, Name: "k", PublicKey: line, Fingerprint: ssh.FingerprintSHA256(sshPub)}); err != nil {
		t.Fatalf("add key: %v", err)
	}

	tmpl, err := st.GetTemplateBySlug("generic-process")
	if err != nil {
		t.Fatalf("template: %v", err)
	}
	srv := &models.Server{
		Slug: "e2e-sftp", DisplayName: "sftp", TemplateID: tmpl.ID, TemplateVersion: tmpl.Version,
		Image: defaultImage(tmpl), Namespace: reconciler.NamespaceFor("e2e-sftp"),
		DesiredState: models.StateRunning, OwnerID: owner.ID,
		SFTP:    models.SFTPConfig{Enabled: true},
		Storage: models.Storage{Type: models.StoragePVC, Size: "1Gi"},
	}
	if err := st.CreateServer(srv); err != nil {
		t.Fatalf("create server: %v", err)
	}
	t.Cleanup(func() { _ = rec.DeleteServer(ctx, srv) })

	// Ready requires the sftp sidecar to be running alongside the game container.
	reconcileUntilRunning(ctx, t, rec, st, srv.ID)

	// The supporting objects exist.
	var svc corev1.Service
	if err := c.Get(ctx, client.ObjectKey{Namespace: srv.Namespace, Name: reconciler.SFTPServiceName}, &svc); err != nil {
		t.Fatalf("sftp service: %v", err)
	}
	if len(svc.Spec.Ports) == 0 || svc.Spec.Ports[0].NodePort == 0 {
		t.Errorf("sftp service has no NodePort: %+v", svc.Spec.Ports)
	}
	var sec corev1.Secret
	if err := c.Get(ctx, client.ObjectKey{Namespace: srv.Namespace, Name: reconciler.SFTPHostKeySecret}, &sec); err != nil {
		t.Errorf("host key secret: %v", err)
	}
	var cm corev1.ConfigMap
	if err := c.Get(ctx, client.ObjectKey{Namespace: srv.Namespace, Name: reconciler.SFTPAuthKeysConfigMap}, &cm); err != nil {
		t.Fatalf("authorized_keys configmap: %v", err)
	}
	if !strings.Contains(cm.Data[reconciler.SFTPAuthKeysField], line) {
		t.Errorf("authorized_keys missing the owner's key:\n%s", cm.Data[reconciler.SFTPAuthKeysField])
	}

	// The pod has a running sftp container.
	var pods corev1.PodList
	if err := c.List(ctx, &pods, client.InNamespace(srv.Namespace)); err != nil {
		t.Fatalf("list pods: %v", err)
	}
	var sawSFTP bool
	for i := range pods.Items {
		for _, cs := range pods.Items[i].Status.ContainerStatuses {
			if cs.Name == "sftp" && cs.State.Running != nil {
				sawSFTP = true
			}
		}
	}
	if !sawSFTP {
		t.Error("sftp sidecar container is not running")
	}
}
