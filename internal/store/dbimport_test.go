package store

import (
	"errors"
	"testing"
	"time"

	"github.com/lolozini/quetzal/internal/models"
)

// A database import holds its server down as a restore does: it is queued only
// for a server that is not meant to run, the server cannot be started until it
// is done, and only one restore or import is waiting or running at a time --
// the game would write to the database while the file replaces it, and a
// restore would load the same database under it.
func TestADatabaseImportHoldsItsServerDown(t *testing.T) {
	st := newTestStore(t)
	srv := &models.Server{Slug: "ts", Namespace: "quetzal-srv-ts", DesiredState: models.StateRunning}
	if err := st.CreateServer(srv); err != nil {
		t.Fatal(err)
	}
	imp := func(dbID uint) *models.Backup {
		return &models.Backup{ServerID: srv.ID, Direction: models.DirDatabaseImport, Phase: models.BackupPending, DatabaseID: dbID, Path: "/ts3.sql"}
	}
	if err := st.CreateDatabaseImport(imp(1)); !errors.Is(err, ErrServerRunning) {
		t.Fatalf("an import of a running server = %v, want ErrServerRunning", err)
	}
	if err := st.SetDesiredState(srv.ID, models.StateStopped); err != nil {
		t.Fatal(err)
	}
	first := imp(1)
	if err := st.CreateDatabaseImport(first); err != nil {
		t.Fatalf("an import of a stopped server: %v", err)
	}
	if err := st.StartServer(srv.ID, time.Now()); !errors.Is(err, ErrDatabaseImportActive) {
		t.Errorf("a start under an import = %v, want ErrDatabaseImportActive", err)
	}
	if err := st.CreateDatabaseImport(imp(2)); !errors.Is(err, ErrDatabaseImportActive) {
		t.Errorf("a second import = %v, want ErrDatabaseImportActive", err)
	}
	if err := st.CreateRestore(&models.Backup{ServerID: srv.ID, Direction: models.DirRestore, Phase: models.BackupPending}); !errors.Is(err, ErrDatabaseImportActive) {
		t.Errorf("a restore under an import = %v, want ErrDatabaseImportActive", err)
	}
	if active, _ := st.DatabaseImportActive(1); !active {
		t.Error("the database's import is not reported active")
	}
	if active, _ := st.HasActiveDatabaseImport(srv.ID); !active {
		t.Error("the server's import is not reported active")
	}
	if active, _ := st.HasActiveRestore(srv.ID); active {
		t.Error("an import is taken for a restore: the data manager would be stopped for it")
	}
	// It is not one of the server's backups.
	if bs, _ := st.ListBackupsForServer(srv.ID); len(bs) != 0 {
		t.Errorf("the backups list shows the import: %+v", bs)
	}

	// Done, it lets the server start, and stays the database's latest import.
	first.Phase = models.BackupSucceeded
	if err := st.UpdateBackup(first); err != nil {
		t.Fatal(err)
	}
	if err := st.StartServer(srv.ID, time.Now()); err != nil {
		t.Errorf("a start after the import: %v", err)
	}
	if err := st.SetDesiredState(srv.ID, models.StateStopped); err != nil {
		t.Fatal(err)
	}
	second := imp(1)
	second.Path = "/again.sql"
	if err := st.CreateDatabaseImport(second); err != nil {
		t.Fatal(err)
	}
	latest, err := st.LatestDatabaseImports(srv.ID)
	if err != nil || latest[1].ID != second.ID || len(latest) != 1 {
		t.Errorf("latest imports = %+v (%v), want the second for database 1", latest, err)
	}
	if err := st.DeleteDatabaseImports(1); err != nil {
		t.Fatal(err)
	}
	if latest, _ := st.LatestDatabaseImports(srv.ID); len(latest) != 0 {
		t.Errorf("imports of a dropped database remain: %+v", latest)
	}
	// A restore waiting is told as a restore.
	if err := st.CreateRestore(&models.Backup{ServerID: srv.ID, Direction: models.DirRestore, Phase: models.BackupPending}); err != nil {
		t.Fatal(err)
	}
	if err := st.StartServer(srv.ID, time.Now()); !errors.Is(err, ErrRestoreActive) {
		t.Errorf("a start under a restore = %v, want ErrRestoreActive", err)
	}
	if err := st.CreateDatabaseImport(imp(1)); !errors.Is(err, ErrRestoreActive) {
		t.Errorf("an import under a restore = %v, want ErrRestoreActive", err)
	}
}

// What a backup claims records the databases its Job dumps, through the
// column's serializer.
func TestAClaimRecordsTheDatabases(t *testing.T) {
	st := newTestStore(t)
	srv := &models.Server{Slug: "c", Namespace: "quetzal-srv-c"}
	if err := st.CreateServer(srv); err != nil {
		t.Fatal(err)
	}
	b := &models.Backup{ServerID: srv.ID, Direction: models.DirBackup, Phase: models.BackupPending}
	if err := st.CreateBackup(b); err != nil {
		t.Fatal(err)
	}
	if ok, err := st.ClaimBackup(b.ID, "quetzal-backup-1", "abcd", []string{"s1_a", "s1_b"}); err != nil || !ok {
		t.Fatalf("claim = %v %v", ok, err)
	}
	got, err := st.GetBackup(b.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Phase != models.BackupRunning || got.JobName != "quetzal-backup-1" || got.Target != "abcd" ||
		len(got.Databases) != 2 || got.Databases[1] != "s1_b" {
		t.Errorf("claimed = %+v", got)
	}
	// A later update of the operation keeps them.
	got.Phase = models.BackupSucceeded
	if err := st.UpdateBackup(got); err != nil {
		t.Fatal(err)
	}
	if again, _ := st.GetBackup(b.ID); len(again.Databases) != 2 {
		t.Errorf("after an update the databases are %v", again.Databases)
	}
}
