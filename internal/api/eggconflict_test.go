package api_test

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/lolozini/quetzal/internal/models"
)

// Two different eggs share a name, and so a slug: Pelican's Paper and
// Pterodactyl's. Importing the second replaced the first without a word, and
// the servers created afterwards lost Java 25 with it. The second import is
// refused, saying what is there, unless the admin asks to replace the template
// or to add the egg beside it.
func TestAnEggImportDoesNotSilentlyReplaceATemplate(t *testing.T) {
	ts, c, st := newTestServerStore(t)
	setupAdmin(t, ts.URL, c)
	egg := func(images string) string {
		return `{"name": "Paper", "author": "a@b.c", "docker_images": {` + images + `}, "startup": "java -jar server.jar"}`
	}
	pelican := egg(`"Java 25": "ghcr.io/parkervcp/yolks:java_25", "Java 21": "ghcr.io/parkervcp/yolks:java_21"`)
	ptero := egg(`"Java 21": "ghcr.io/parkervcp/yolks:java_21", "Java 17": "ghcr.io/parkervcp/yolks:java_17"`)
	imp := func(body, query string) *http.Response {
		t.Helper()
		r, err := c.Post(ts.URL+"/api/templates/import"+query, "application/json", strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		return r
	}
	defaultImage := func(slug string) string {
		t.Helper()
		tpl, err := st.GetTemplateBySlug(slug)
		if err != nil {
			t.Fatalf("%s: %v", slug, err)
		}
		for _, i := range tpl.Images {
			if i.Default {
				return i.Ref
			}
		}
		return ""
	}

	if r := imp(pelican, ""); r.StatusCode != http.StatusCreated {
		t.Fatalf("first import = %d", r.StatusCode)
	}
	paper, _ := st.GetTemplateBySlug("paper")
	if err := st.CreateServer(&models.Server{Slug: "survival-a1b2", DisplayName: "survival", Namespace: "qz-survival-a1b2", TemplateID: paper.ID}); err != nil {
		t.Fatal(err)
	}

	r := imp(ptero, "")
	var conflict struct {
		Error    string
		Existing struct {
			Slug    string
			Version int
			Servers int
		}
	}
	_ = json.NewDecoder(r.Body).Decode(&conflict)
	if r.StatusCode != http.StatusConflict {
		t.Fatalf("second egg with the same name = %d, want 409", r.StatusCode)
	}
	if conflict.Existing.Slug != "paper" || conflict.Existing.Version != 1 || conflict.Existing.Servers != 1 || !strings.Contains(conflict.Error, "used by 1 server") {
		t.Errorf("the refusal does not say what is there: %+v", conflict)
	}
	if got := defaultImage("paper"); got != "ghcr.io/parkervcp/yolks:java_25" {
		t.Errorf("the refused import changed the template: default image %s", got)
	}

	// Beside it, under the next free slug.
	r = imp(ptero, "?ifExists=copy")
	var copied struct{ Slug, Name string }
	_ = json.NewDecoder(r.Body).Decode(&copied)
	if r.StatusCode != http.StatusCreated || copied.Slug != "paper-2" || copied.Name != "Paper (2)" {
		t.Errorf("copy = %d %+v, want 201 paper-2 \"Paper (2)\"", r.StatusCode, copied)
	}
	if defaultImage("paper") != "ghcr.io/parkervcp/yolks:java_25" || defaultImage("paper-2") != "ghcr.io/parkervcp/yolks:java_21" {
		t.Errorf("after the copy: paper %s, paper-2 %s", defaultImage("paper"), defaultImage("paper-2"))
	}

	// Or in its place, when that is what the admin wants.
	if r := imp(ptero, "?ifExists=replace"); r.StatusCode != http.StatusCreated {
		t.Fatalf("replace = %d", r.StatusCode)
	}
	if tpl, _ := st.GetTemplateBySlug("paper"); tpl.Version != 2 || defaultImage("paper") != "ghcr.io/parkervcp/yolks:java_21" {
		t.Errorf("after replacing: version %d, default %s", tpl.Version, defaultImage("paper"))
	}

	if r := imp(ptero, "?ifExists=overwrite"); r.StatusCode != http.StatusBadRequest {
		t.Errorf("ifExists=overwrite = %d, want 400", r.StatusCode)
	}
}
