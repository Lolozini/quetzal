package reconciler

import (
	"context"
	"crypto/ed25519"
	"strings"
	"testing"

	"golang.org/x/crypto/ssh"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/managedfields"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/lolozini/quetzal/internal/models"
)

func TestTransferFreezesDataDeploymentAndSFTPKeys(t *testing.T) {
	st := reconStore(t)
	u := &models.User{Username: "transfer-owner"}
	if err := st.CreateUser(u); err != nil {
		t.Fatal(err)
	}
	srv := &models.Server{Slug: "frozen", Namespace: "frozen", OwnerID: u.ID, DesiredState: models.StateStopped}
	srv.SFTP.Enabled = true
	if err := st.CreateServer(srv); err != nil {
		t.Fatal(err)
	}
	key, err := ssh.NewPublicKey(ed25519.NewKeyFromSeed(make([]byte, ed25519.SeedSize)).Public())
	if err != nil {
		t.Fatal(err)
	}
	if err := st.AddSSHKey(&models.SSHKey{UserID: u.ID, PublicKey: string(ssh.MarshalAuthorizedKey(key)), Fingerprint: ssh.FingerprintSHA256(key)}); err != nil {
		t.Fatal(err)
	}
	cl := fake.NewClientBuilder().WithScheme(clientgoscheme.Scheme).WithTypeConverters(managedfields.NewDeducedTypeConverter()).WithObjects(
		&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: SFTPHostKeySecret, Namespace: srv.Namespace}},
	).Build()
	r := New(cl, st)
	r.ActivatorImage = "quetzal:test"
	ctx := context.Background()
	tmpl := &models.Template{}
	check := func(frozen bool) {
		t.Helper()
		if err := r.ensureDataDeployment(ctx, srv, tmpl); err != nil {
			t.Fatal(err)
		}
		if err := r.ensureSFTP(ctx, srv); err != nil {
			t.Fatal(err)
		}
		var dep appsv1.Deployment
		if err := cl.Get(ctx, client.ObjectKeyFromObject(BuildDataDeployment(srv, tmpl, r.ActivatorImage, 1)), &dep); err != nil {
			t.Fatal(err)
		}
		var keys corev1.ConfigMap
		if err := cl.Get(ctx, client.ObjectKey{Namespace: srv.Namespace, Name: SFTPAuthKeysConfigMap}, &keys); err != nil {
			t.Fatal(err)
		}
		if frozen {
			if *dep.Spec.Replicas != 0 || strings.TrimSpace(keys.Data[SFTPAuthKeysField]) != "" {
				t.Fatalf("transfer left writers authorized: replicas=%d keys=%q", *dep.Spec.Replicas, keys.Data[SFTPAuthKeysField])
			}
		} else if *dep.Spec.Replicas != 1 || strings.TrimSpace(keys.Data[SFTPAuthKeysField]) == "" {
			t.Fatal("unfrozen data access was not restored")
		}
	}
	check(false)
	for _, phase := range []models.TransferPhase{models.TransferBackingUp, models.TransferRestoring, models.TransferCommitting} {
		srv.Transfer = &models.TransferState{Phase: phase}
		check(true)
	}
	srv.Transfer = nil
	check(false)
}
