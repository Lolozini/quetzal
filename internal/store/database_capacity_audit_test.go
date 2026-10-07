package store

import (
	"errors"
	"fmt"
	"testing"

	"github.com/lolozini/quetzal/internal/models"
	"github.com/lolozini/quetzal/internal/testdb"
)

func TestDatabaseHostCapacityAcrossStoreConnections(t *testing.T) {
	cfg := Config{Driver: Driver(testdb.Driver()), DSN: testdb.DSN(t, "capacity.db"), Silent: true}
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
	h := &models.DatabaseHost{Name: "single-slot", Kind: models.DBHostExternal, MaxDatabases: 1}
	if err := first.CreateDatabaseHost(h, "password"); err != nil {
		t.Fatal(err)
	}
	start := make(chan struct{})
	results := make(chan error, 2)
	for i, st := range []*Store{first, second} {
		go func(i int, st *Store) {
			<-start
			results <- st.CreateServerDatabase(&models.ServerDatabase{ServerID: 1, HostID: h.ID, DatabaseName: fmt.Sprintf("db%d", i), Username: fmt.Sprintf("user%d", i), Remote: "%"}, "password")
		}(i, st)
	}
	close(start)
	succeeded := 0
	for range 2 {
		if err := <-results; err == nil {
			succeeded++
		} else if !errors.Is(err, ErrDatabaseHostFull) {
			t.Errorf("unexpected reservation error: %v", err)
		}
	}
	if succeeded != 1 {
		t.Errorf("created %d databases with capacity 1", succeeded)
	}
	n, err := first.CountDatabasesOnHost(h.ID)
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("stored databases = %d, want 1", n)
	}
}
