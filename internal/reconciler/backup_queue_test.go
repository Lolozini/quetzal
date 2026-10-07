package reconciler

import (
	"context"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	"k8s.io/apimachinery/pkg/util/managedfields"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/lolozini/quetzal/internal/models"
)

func TestQueuedRestoreKeepsEarlierBackupSchedulable(t *testing.T) {
	st := reconStore(t)
	srv := &models.Server{Slug: "queued", Namespace: "queued", DesiredState: models.StateStopped}
	if err := st.CreateServer(srv); err != nil {
		t.Fatal(err)
	}
	backup := &models.Backup{ServerID: srv.ID, Direction: models.DirBackup, Phase: models.BackupPending}
	if err := st.CreateBackup(backup); err != nil {
		t.Fatal(err)
	}
	restore := &models.Backup{ServerID: srv.ID, Direction: models.DirRestore, Phase: models.BackupPending}
	if err := st.CreateRestore(restore); err != nil {
		t.Fatal(err)
	}
	cl := fake.NewClientBuilder().WithScheme(clientgoscheme.Scheme).WithTypeConverters(managedfields.NewDeducedTypeConverter()).Build()
	r := New(cl, st)
	ctx := context.Background()
	tmpl := &models.Template{}
	want := BuildDataDeployment(srv, tmpl, "", 1)
	for _, phase := range []models.BackupPhase{models.BackupPending, models.BackupRunning, models.BackupSucceeded} {
		backup.Phase = phase
		if err := st.UpdateBackup(backup); err != nil {
			t.Fatal(err)
		}
		if err := r.ensureDataDeployment(ctx, srv, tmpl); err != nil {
			t.Fatal(err)
		}
		var got appsv1.Deployment
		if err := cl.Get(ctx, client.ObjectKeyFromObject(want), &got); err != nil {
			t.Fatal(err)
		}
		replicas := int32(1)
		if phase == models.BackupSucceeded {
			replicas = 0
		}
		if *got.Spec.Replicas != replicas {
			t.Fatalf("earlier backup %s: data replicas=%d want %d", phase, *got.Spec.Replicas, replicas)
		}
	}
}
