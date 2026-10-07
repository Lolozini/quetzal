package store

import (
	"errors"
	"fmt"
	"testing"

	"github.com/lolozini/quetzal/internal/models"
	"github.com/lolozini/quetzal/internal/testdb"
)

func TestManagedDatabaseNamespaceUniqueness(t *testing.T) {
	s := newTestStore(t)
	first := &models.DatabaseHost{Name: "default", Kind: models.DBHostManaged}
	if err := s.CreateDatabaseHost(first, "password"); err != nil {
		t.Fatal(err)
	}
	namespace := fmt.Sprintf("quetzal-db-%d", first.ID)
	duplicate := &models.DatabaseHost{Name: "explicit", Kind: models.DBHostManaged, Namespace: namespace, ClusterID: 99}
	if err := s.CreateDatabaseHost(duplicate, "password"); !errors.Is(err, ErrDatabaseHostNamespaceInUse) {
		t.Errorf("explicit namespace collision: %v", err)
	}
	other := &models.DatabaseHost{Name: "other", Kind: models.DBHostManaged, Namespace: "quetzal-db-other"}
	if err := s.CreateDatabaseHost(other, "password"); err != nil {
		t.Fatal(err)
	}
	other.Namespace = namespace
	if err := s.UpdateDatabaseHost(other, nil); !errors.Is(err, ErrDatabaseHostNamespaceInUse) {
		t.Errorf("namespace update collision: %v", err)
	}
	stored, err := s.GetDatabaseHost(other.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Namespace != "quetzal-db-other" {
		t.Errorf("rejected update changed namespace to %q", stored.Namespace)
	}
}

func TestManagedDatabaseDerivedNamespaceCannotTakeExplicitNamespace(t *testing.T) {
	s := newTestStore(t)
	explicit := &models.DatabaseHost{Name: "explicit", Kind: models.DBHostManaged, Namespace: "quetzal-db-500"}
	if err := s.CreateDatabaseHost(explicit, "password"); err != nil {
		t.Fatal(err)
	}
	derived := &models.DatabaseHost{ID: 500, Name: "derived", Kind: models.DBHostManaged}
	if err := s.CreateDatabaseHost(derived, "password"); !errors.Is(err, ErrDatabaseHostNamespaceInUse) {
		t.Errorf("derived namespace collision: %v", err)
	}
	hosts, err := s.ListDatabaseHosts()
	if err != nil {
		t.Fatal(err)
	}
	if len(hosts) != 1 {
		t.Errorf("refused host remained stored: %d hosts", len(hosts))
	}
}

func TestManagedDatabaseNamespaceAcrossStoreConnections(t *testing.T) {
	cfg := Config{Driver: Driver(testdb.Driver()), DSN: testdb.DSN(t, "namespaces.db"), Silent: true}
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
	start := make(chan struct{})
	results := make(chan error, 2)
	for i, st := range []*Store{first, second} {
		go func(i int, st *Store) {
			<-start
			results <- st.CreateDatabaseHost(&models.DatabaseHost{Name: fmt.Sprintf("host%d", i), Kind: models.DBHostManaged, Namespace: "quetzal-db-shared", ClusterID: uint(i)}, "password")
		}(i, st)
	}
	close(start)
	succeeded := 0
	for range 2 {
		if err := <-results; err == nil {
			succeeded++
		} else if !errors.Is(err, ErrDatabaseHostNamespaceInUse) {
			t.Errorf("unexpected namespace reservation error: %v", err)
		}
	}
	if succeeded != 1 {
		t.Errorf("created %d hosts sharing one managed namespace", succeeded)
	}
}
