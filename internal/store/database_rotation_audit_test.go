package store

import (
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/lolozini/quetzal/internal/models"
	"github.com/lolozini/quetzal/internal/testdb"
	"gorm.io/gorm"
)

func TestDatabaseRotationDoesNotReachRemoteWhenPersistenceFails(t *testing.T) {
	s := newTestStore(t)
	h := &models.DatabaseHost{Name: "host", Kind: models.DBHostExternal}
	if err := s.CreateDatabaseHost(h, "adminpass"); err != nil {
		t.Fatal(err)
	}
	d := &models.ServerDatabase{HostID: h.ID, DatabaseName: "db", Username: "user", Remote: "%"}
	if err := s.CreateServerDatabase(d, "oldpassword"); err != nil {
		t.Fatal(err)
	}
	failure := errors.New("injected database write failure")
	if err := s.db.Callback().Update().Before("gorm:update").Register("audit:fail_password_write", func(tx *gorm.DB) {
		if tx.Statement.Table == "server_databases" {
			tx.AddError(failure)
		}
	}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.db.Callback().Update().Remove("audit:fail_password_write") })
	calls := 0
	err := s.RotateServerDatabasePassword(d.ID, "newpassword", func(*models.ServerDatabase, string) error { calls++; return nil })
	if !errors.Is(err, failure) {
		t.Fatalf("rotation error = %v, want persistence failure", err)
	}
	if calls != 0 {
		t.Errorf("remote mutations = %d after persistence failure", calls)
	}
	stored, err := s.GetServerDatabase(d.ID)
	if err != nil {
		t.Fatal(err)
	}
	password, err := s.ServerDatabasePassword(stored)
	if err != nil || password != "oldpassword" {
		t.Errorf("stored password changed: %q, %v", password, err)
	}
}

func TestDatabaseRotationReportsFailedCompensation(t *testing.T) {
	s := newTestStore(t)
	h := &models.DatabaseHost{Name: "host", Kind: models.DBHostExternal}
	if err := s.CreateDatabaseHost(h, "adminpass"); err != nil {
		t.Fatal(err)
	}
	d := &models.ServerDatabase{HostID: h.ID, DatabaseName: "db", Username: "user", Remote: "%"}
	if err := s.CreateServerDatabase(d, "oldpassword"); err != nil {
		t.Fatal(err)
	}
	var attempted []string
	err := s.RotateServerDatabasePassword(d.ID, "newpassword", func(_ *models.ServerDatabase, next string) error {
		attempted = append(attempted, next)
		return errors.New("remote unavailable")
	})
	if !errors.Is(err, ErrDatabaseRotationRemote) {
		t.Fatalf("rotation error = %v", err)
	}
	if len(attempted) != 2 || attempted[0] != "newpassword" || attempted[1] != "oldpassword" {
		t.Errorf("rotation/compensation attempts = %v", attempted)
	}
	stored, err := s.GetServerDatabase(d.ID)
	if err != nil {
		t.Fatal(err)
	}
	password, err := s.ServerDatabasePassword(stored)
	if err != nil || password != "oldpassword" {
		t.Errorf("failed compensation changed stored secret: %q, %v", password, err)
	}
}

func TestDatabaseRotationAcrossStoreConnections(t *testing.T) {
	cfg := Config{Driver: Driver(testdb.Driver()), DSN: testdb.DSN(t, "rotations.db"), Silent: true}
	first, err := Open(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := first.Migrate(); err != nil {
		t.Fatal(err)
	}
	second, err := Open(cfg)
	if err != nil {
		t.Fatal(err)
	}
	h := &models.DatabaseHost{Name: "host", Kind: models.DBHostExternal}
	if err := first.CreateDatabaseHost(h, "adminpass"); err != nil {
		t.Fatal(err)
	}
	d := &models.ServerDatabase{HostID: h.ID, DatabaseName: "db", Username: "user", Remote: "%"}
	if err := first.CreateServerDatabase(d, "oldpassword"); err != nil {
		t.Fatal(err)
	}
	entered, secondDone := make(chan struct{}), make(chan struct{})
	var mu sync.Mutex
	remote := "oldpassword"
	rotate := func(_ *models.ServerDatabase, next string) error {
		mu.Lock()
		remote = next
		mu.Unlock()
		if next == "passwordA" {
			close(entered)
			select {
			case <-secondDone:
			case <-time.After(500 * time.Millisecond):
			}
		}
		return nil
	}
	result := make(chan error, 1)
	go func() { result <- first.RotateServerDatabasePassword(d.ID, "passwordA", rotate) }()
	<-entered
	secondErr := second.RotateServerDatabasePassword(d.ID, "passwordB", rotate)
	close(secondDone)
	if err := <-result; err != nil {
		t.Fatal(err)
	}
	if secondErr != nil {
		t.Fatal(secondErr)
	}
	stored, err := first.GetServerDatabase(d.ID)
	if err != nil {
		t.Fatal(err)
	}
	password, err := first.ServerDatabasePassword(stored)
	if err != nil {
		t.Fatal(err)
	}
	if remote != "passwordB" || password != remote {
		t.Error("cross-replica rotations lost credential consistency")
	}
}
