package store

import (
	"testing"

	"github.com/lolozini/quetzal/internal/models"
)

// The dashboard shipped every server on every load, with no way to narrow it.
// The filter has to run here, and it has to treat the search text as text.
func TestSearchServers(t *testing.T) {
	st := newTestStore(t)
	owner := &models.User{Username: "o", PasswordHash: "x"}
	other := &models.User{Username: "p", PasswordHash: "x"}
	for _, u := range []*models.User{owner, other} {
		if err := st.CreateUser(u); err != nil {
			t.Fatal(err)
		}
	}
	mk := func(slug, name string, ownerID uint) *models.Server {
		s := &models.Server{Slug: slug, DisplayName: name, OwnerID: ownerID}
		if err := st.CreateServer(s); err != nil {
			t.Fatal(err)
		}
		return s
	}
	mk("survival-smp", "Survival SMP", owner.ID)
	mk("creative", "Creative Flat", owner.ID)
	mk("hundred", "100% uptime", owner.ID)
	mk("elsewhere", "Someone Else", other.ID)

	slugs := func(srvs []models.Server) []string {
		out := make([]string, 0, len(srvs))
		for _, s := range srvs {
			out = append(out, s.Slug)
		}
		return out
	}

	all, err := st.SearchServers("")
	if err != nil || len(all) != 4 {
		t.Fatalf("empty query = %v (%v), want all 4", slugs(all), err)
	}
	// Matches the slug, and the display name, and ignores case.
	for _, q := range []string{"surv", "SURVIVAL", "Survival SMP", "smp"} {
		got, err := st.SearchServers(q)
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != 1 || got[0].Slug != "survival-smp" {
			t.Errorf("q=%q gave %v, want [survival-smp]", q, slugs(got))
		}
	}
	// A LIKE metacharacter in the search text is text, not a wildcard: "100%"
	// must find the one server, not every server.
	got, err := st.SearchServers("100%")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Slug != "hundred" {
		t.Errorf("q=%q gave %v, want [hundred]", "100%", slugs(got))
	}
	if got, _ := st.SearchServers("_"); len(got) != 0 {
		t.Errorf("an underscore matched %v; it should be literal", slugs(got))
	}

	// The filter must not widen what a user may see.
	mine, err := st.SearchAccessibleServers(owner.ID, "")
	if err != nil || len(mine) != 3 {
		t.Fatalf("accessible = %v (%v), want 3", slugs(mine), err)
	}
	if got, _ := st.SearchAccessibleServers(owner.ID, "Someone"); len(got) != 0 {
		t.Errorf("search reached another owner's server: %v", slugs(got))
	}
}
