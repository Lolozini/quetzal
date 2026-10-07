package api

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/lolozini/quetzal/internal/dbprovision"
	"github.com/lolozini/quetzal/internal/models"
)

func databaseOperationFixture(t *testing.T) (*Server, *models.User, *models.Server, *models.DatabaseHost) {
	t.Helper()
	s := eggTestServer(t)
	u := &models.User{Username: "owner"}
	if err := s.Store.CreateUser(u); err != nil {
		t.Fatal(err)
	}
	srv := &models.Server{OwnerID: u.ID, Slug: "database-audit", Namespace: "quetzal-srv-database-audit"}
	if err := s.Store.CreateServer(srv); err != nil {
		t.Fatal(err)
	}
	h := &models.DatabaseHost{Name: "host", Kind: models.DBHostExternal, Host: "unused", Port: 3306, AdminUser: "root", MaxDatabases: 1}
	if err := s.Store.CreateDatabaseHost(h, "adminpass"); err != nil {
		t.Fatal(err)
	}
	return s, u, srv, h
}

func databaseOperationRequest(u *models.User, serverID, databaseID uint, body string) *http.Request {
	r := asUser(httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body)), u)
	r.SetPathValue("id", fmt.Sprint(serverID))
	r.SetPathValue("dbid", fmt.Sprint(databaseID))
	return r
}

func TestConcurrentDatabaseProvisionReservesCapacity(t *testing.T) {
	s, u, srv, h := databaseOperationFixture(t)
	original := provisionDatabase
	t.Cleanup(func() { provisionDatabase = original })
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	var mu sync.Mutex
	provisioned := 0
	provisionDatabase = func(context.Context, dbprovision.Conn, string, string, string, string) error {
		mu.Lock()
		provisioned++
		mu.Unlock()
		first := false
		once.Do(func() { first = true; close(entered) })
		if first {
			<-release
		}
		return nil
	}
	call := func() *httptest.ResponseRecorder {
		rr := httptest.NewRecorder()
		s.handleCreateServerDatabase(rr, databaseOperationRequest(u, srv.ID, 0, fmt.Sprintf(`{"hostId":%d}`, h.ID)))
		return rr
	}
	first := make(chan *httptest.ResponseRecorder, 1)
	go func() { first <- call() }()
	<-entered
	second := call()
	close(release)
	one := <-first
	if one.Code != http.StatusCreated {
		t.Errorf("first = %d %s", one.Code, one.Body)
	}
	if second.Code != http.StatusConflict {
		t.Errorf("second = %d %s, want 409", second.Code, second.Body)
	}
	if provisioned != 1 {
		t.Errorf("remote databases created = %d, want 1", provisioned)
	}
	if n, err := s.Store.CountDatabasesOnHost(h.ID); err != nil || n != 1 {
		t.Errorf("stored count = %d, %v", n, err)
	}
}

func TestConcurrentDatabaseRotationsKeepRemoteAndStoredSecretTogether(t *testing.T) {
	s, u, srv, h := databaseOperationFixture(t)
	d := &models.ServerDatabase{ServerID: srv.ID, HostID: h.ID, DatabaseName: "database", Username: "dbuser", Remote: "%"}
	if err := s.Store.CreateServerDatabase(d, "oldpassword"); err != nil {
		t.Fatal(err)
	}
	original := rotateDatabasePassword
	t.Cleanup(func() { rotateDatabasePassword = original })
	entered, secondDone := make(chan struct{}), make(chan struct{})
	var mu sync.Mutex
	remotePassword, calls := "oldpassword", 0
	rotateDatabasePassword = func(_ context.Context, _ dbprovision.Conn, _, _, password string) error {
		mu.Lock()
		remotePassword = password
		calls++
		first := calls == 1
		mu.Unlock()
		if first {
			close(entered)
			// The old handler lets the second rotation commit before the first.
			// A serialized handler instead makes it wait until this call returns.
			select {
			case <-secondDone:
			case <-time.After(500 * time.Millisecond):
			}
		}
		return nil
	}
	call := func() *httptest.ResponseRecorder {
		rr := httptest.NewRecorder()
		s.handleRotateServerDatabase(rr, databaseOperationRequest(u, srv.ID, d.ID, ""))
		return rr
	}
	first := make(chan *httptest.ResponseRecorder, 1)
	go func() { first <- call() }()
	<-entered
	two := call()
	close(secondDone)
	one := <-first
	for _, rr := range []*httptest.ResponseRecorder{one, two} {
		if rr.Code != http.StatusOK {
			t.Errorf("rotate = %d %s", rr.Code, rr.Body)
		}
	}
	stored, err := s.Store.GetServerDatabase(d.ID)
	if err != nil {
		t.Fatal(err)
	}
	password, err := s.Store.ServerDatabasePassword(stored)
	if err != nil {
		t.Fatal(err)
	}
	if password != remotePassword {
		t.Error("stored password no longer authenticates against the remote account")
	}
}

func TestDatabaseRotationRestoresSecretAfterUncertainRemoteFailure(t *testing.T) {
	s, u, srv, h := databaseOperationFixture(t)
	d := &models.ServerDatabase{ServerID: srv.ID, HostID: h.ID, DatabaseName: "database", Username: "dbuser", Remote: "%"}
	if err := s.Store.CreateServerDatabase(d, "oldpassword"); err != nil {
		t.Fatal(err)
	}
	original := rotateDatabasePassword
	t.Cleanup(func() { rotateDatabasePassword = original })
	remotePassword := "oldpassword"
	calls := 0
	rotateDatabasePassword = func(_ context.Context, _ dbprovision.Conn, _, _, password string) error {
		remotePassword = password
		calls++
		if calls == 1 {
			return fmt.Errorf("response lost after ALTER")
		}
		return nil
	}
	rr := httptest.NewRecorder()
	s.handleRotateServerDatabase(rr, databaseOperationRequest(u, srv.ID, d.ID, ""))
	if rr.Code != http.StatusBadGateway {
		t.Errorf("rotate = %d %s, want 502", rr.Code, rr.Body)
	}
	stored, err := s.Store.GetServerDatabase(d.ID)
	if err != nil {
		t.Fatal(err)
	}
	password, err := s.Store.ServerDatabasePassword(stored)
	if err != nil {
		t.Fatal(err)
	}
	if password != "oldpassword" || remotePassword != password {
		t.Error("failed rotation did not restore the previous usable secret")
	}
}

func TestFailedDatabaseProvisionReleasesOnlyConfirmedCleanup(t *testing.T) {
	for _, cleanupFails := range []bool{false, true} {
		t.Run(fmt.Sprint(cleanupFails), func(t *testing.T) {
			s, u, srv, h := databaseOperationFixture(t)
			oldProvision, oldDeprovision := provisionDatabase, deprovisionDatabase
			t.Cleanup(func() { provisionDatabase, deprovisionDatabase = oldProvision, oldDeprovision })
			exists := false
			provisionDatabase = func(context.Context, dbprovision.Conn, string, string, string, string) error {
				exists = true
				return fmt.Errorf("lost provisioning response")
			}
			deprovisionDatabase = func(ctx context.Context, _ dbprovision.Conn, _, _, _ string) error {
				if ctx.Err() != nil {
					return ctx.Err()
				}
				if cleanupFails {
					return fmt.Errorf("cleanup unavailable")
				}
				exists = false
				return nil
			}
			rr := httptest.NewRecorder()
			s.handleCreateServerDatabase(rr, databaseOperationRequest(u, srv.ID, 0, fmt.Sprintf(`{"hostId":%d}`, h.ID)))
			if rr.Code != http.StatusBadGateway {
				t.Errorf("create = %d %s, want 502", rr.Code, rr.Body)
			}
			n, err := s.Store.CountDatabasesOnHost(h.ID)
			if err != nil {
				t.Fatal(err)
			}
			if cleanupFails {
				if !exists || n != 1 {
					t.Errorf("uncertain remote database lost reservation: exists=%v count=%d", exists, n)
				}
			} else if exists || n != 0 {
				t.Errorf("confirmed cleanup left remote database or reservation: exists=%v count=%d", exists, n)
			}
		})
	}
}
