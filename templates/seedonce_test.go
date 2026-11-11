package templates_test

import (
	"testing"

	"github.com/lolozini/quetzal/internal/models"
	"github.com/lolozini/quetzal/internal/store"
	"github.com/lolozini/quetzal/internal/testdb"
	"github.com/lolozini/quetzal/templates"
)

func newStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open(store.Config{Driver: store.Driver(testdb.Driver()), DSN: testdb.DSN(t, "seed.db"), Silent: true})
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Migrate(); err != nil {
		t.Fatal(err)
	}
	return st
}

func count(t *testing.T, st *store.Store) int {
	t.Helper()
	ts, err := st.ListTemplates()
	if err != nil {
		t.Fatal(err)
	}
	return len(ts)
}

// A new install has no template, and nothing to create a first server from.
func TestSeedOnceGivesANewInstanceTheBuiltinTemplates(t *testing.T) {
	st := newStore(t)
	builtin, err := templates.Builtin()
	if err != nil || len(builtin) == 0 {
		t.Fatalf("builtin templates: %d, %v", len(builtin), err)
	}
	seeded, err := templates.SeedOnce(st)
	if err != nil || !seeded {
		t.Fatalf("SeedOnce = %v, %v; want true, nil", seeded, err)
	}
	if got := count(t, st); got != len(builtin) {
		t.Fatalf("%d templates after seeding, want %d", got, len(builtin))
	}
}

// An administrator who removed a built-in template does not get it back at the
// next start, and a second start changes nothing.
func TestSeedOnceNeverSeedsTwice(t *testing.T) {
	st := newStore(t)
	if _, err := templates.SeedOnce(st); err != nil {
		t.Fatal(err)
	}
	all, _ := st.ListTemplates()
	if err := st.DeleteTemplate(all[0].ID); err != nil {
		t.Fatal(err)
	}
	want := count(t, st)
	seeded, err := templates.SeedOnce(st)
	if err != nil || seeded {
		t.Fatalf("second SeedOnce = %v, %v; want false, nil", seeded, err)
	}
	if got := count(t, st); got != want {
		t.Fatalf("a deleted template came back: %d templates, want %d", got, want)
	}
}

// An install that predates SeedOnce already has its templates, whatever they are:
// they are left as they are, and the instance is marked so that deleting them
// later does not seed it either.
func TestSeedOnceLeavesAnInstanceWithTemplatesAlone(t *testing.T) {
	st := newStore(t)
	own := &models.Template{Slug: "mine", Name: "Mine", Version: 1}
	if _, err := st.UpsertTemplate(own); err != nil {
		t.Fatal(err)
	}
	seeded, err := templates.SeedOnce(st)
	if err != nil || seeded {
		t.Fatalf("SeedOnce = %v, %v; want false, nil", seeded, err)
	}
	if got := count(t, st); got != 1 {
		t.Fatalf("%d templates, want only the administrator's own", got)
	}
	saved, _ := st.GetTemplateBySlug("mine")
	if err := st.DeleteTemplate(saved.ID); err != nil {
		t.Fatal(err)
	}
	if seeded, err := templates.SeedOnce(st); err != nil || seeded || count(t, st) != 0 {
		t.Fatalf("an emptied, already-marked instance was seeded (%v, %v, %d templates)", seeded, err, count(t, st))
	}
}
