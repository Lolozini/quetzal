package store

import (
	"testing"

	"github.com/lolozini/quetzal/internal/models"
)

func TestDatabaseHostSealsAdminPassword(t *testing.T) {
	s := newTestStore(t)
	h := &models.DatabaseHost{Name: "ext", Kind: models.DBHostExternal, Host: "db.example", Port: 3306, AdminUser: "root"}
	if err := s.CreateDatabaseHost(h, "s3cretAdmin"); err != nil {
		t.Fatal(err)
	}
	if h.AdminPasswordEnc == "" || h.AdminPasswordEnc == "s3cretAdmin" {
		t.Errorf("admin password not sealed: %q", h.AdminPasswordEnc)
	}
	got, err := s.GetDatabaseHost(h.ID)
	if err != nil {
		t.Fatal(err)
	}
	pw, err := s.DatabaseHostAdminPassword(got)
	if err != nil || pw != "s3cretAdmin" {
		t.Fatalf("round-trip admin password = %q, %v", pw, err)
	}
}

func TestServerDatabaseCRUDAndCount(t *testing.T) {
	s := newTestStore(t)
	h := &models.DatabaseHost{Name: "h", Kind: models.DBHostExternal, Host: "x", Port: 3306, AdminUser: "root"}
	if err := s.CreateDatabaseHost(h, "p"); err != nil {
		t.Fatal(err)
	}
	d := &models.ServerDatabase{ServerID: 7, HostID: h.ID, DatabaseName: "s7_abc", Username: "u7_abc", Remote: "%"}
	if err := s.CreateServerDatabase(d, "userpass1"); err != nil {
		t.Fatal(err)
	}
	if n, _ := s.CountDatabasesOnHost(h.ID); n != 1 {
		t.Errorf("count = %d, want 1", n)
	}
	ds, err := s.ListServerDatabases(7)
	if err != nil || len(ds) != 1 {
		t.Fatalf("list = %v, %v", ds, err)
	}
	pw, err := s.ServerDatabasePassword(&ds[0])
	if err != nil || pw != "userpass1" {
		t.Fatalf("password round-trip = %q, %v", pw, err)
	}
	if err := s.UpdateServerDatabasePassword(d.ID, "rotated99"); err != nil {
		t.Fatal(err)
	}
	got, _ := s.GetServerDatabase(d.ID)
	if pw, _ := s.ServerDatabasePassword(got); pw != "rotated99" {
		t.Errorf("rotated password = %q, want rotated99", pw)
	}
	if err := s.DeleteServerDatabase(d.ID); err != nil {
		t.Fatal(err)
	}
	if n, _ := s.CountDatabasesOnHost(h.ID); n != 0 {
		t.Errorf("count after delete = %d, want 0", n)
	}
}
