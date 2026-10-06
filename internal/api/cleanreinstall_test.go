package api_test

import (
	"encoding/json"
	"net/http"
	"reflect"
	"testing"

	"github.com/lolozini/quetzal/internal/models"
)

type cleanReply struct {
	Status   string   `json:"status"`
	WipeData bool     `json:"wipeData"`
	Keep     []string `json:"keep"`
	Error    string   `json:"error"`
}

func cleanReinstall(t *testing.T, c *http.Client, url string, body any) (int, cleanReply) {
	t.Helper()
	r := post(t, c, url+"/reinstall", body)
	defer r.Body.Close()
	var rep cleanReply
	_ = json.NewDecoder(r.Body).Decode(&rep)
	return r.StatusCode, rep
}

// A clean reinstall wipes all but the paths it keeps, and the server
// remembers them for the next one.
func TestReinstallCanKeepPaths(t *testing.T) {
	ts, admin, st := newTestServerStore(t)
	seedSwitchTemplates(t, st)
	post(t, admin, ts.URL+"/api/setup", map[string]string{"username": "admin", "password": "supersecret"})
	var created struct{ ID uint }
	r := post(t, admin, ts.URL+"/api/servers", map[string]any{"name": "atm", "template": "egg-paper"})
	if r.StatusCode != http.StatusCreated {
		t.Fatalf("create = %d", r.StatusCode)
	}
	json.NewDecoder(r.Body).Decode(&created)
	url := ts.URL + "/api/servers/" + itoa(created.ID)
	before, _ := st.GetServer(created.ID)

	// Keeping something from a reinstall that deletes nothing is a request
	// that forgot half of itself: refused rather than run as a plain one.
	if code, _ := cleanReinstall(t, admin, url, map[string]any{"keep": []string{"world"}}); code != http.StatusBadRequest {
		t.Errorf("keep without wipeData = %d, want 400", code)
	}
	for _, bad := range []string{"../other", "/etc", "."} {
		if code, _ := cleanReinstall(t, admin, url, map[string]any{"wipeData": true, "keep": []string{"world", bad}}); code != http.StatusBadRequest {
			t.Errorf("keep %q = %d, want 400", bad, code)
		}
	}
	if s, _ := st.GetServer(created.ID); s.InstallGeneration != before.InstallGeneration || s.InstallWipe {
		t.Fatal("a refused clean reinstall still scheduled something")
	}

	code, rep := cleanReinstall(t, admin, url, map[string]any{
		"wipeData": true, "keep": []string{"world*", " server.properties ", "./ops.json", "world*"},
	})
	if code != http.StatusOK || rep.Status != "reinstalling" || !rep.WipeData {
		t.Fatalf("clean reinstall = %d %+v", code, rep)
	}
	want := []string{"world*", "server.properties", "ops.json"}
	if !reflect.DeepEqual(rep.Keep, want) {
		t.Errorf("reply keeps %q, want %q", rep.Keep, want)
	}
	s, _ := st.GetServer(created.ID)
	if !s.InstallWipe || !reflect.DeepEqual(s.InstallKeep, want) || s.InstallGeneration != before.InstallGeneration+1 {
		t.Errorf("server: wipe %v keeping %q generation %d", s.InstallWipe, s.InstallKeep, s.InstallGeneration)
	}
	// The panel reads the remembered list off the server.
	var view struct {
		ReinstallKeep []string `json:"reinstallKeep"`
	}
	getJSON(t, admin, url, &view)
	if !reflect.DeepEqual(view.ReinstallKeep, want) {
		t.Errorf("server view remembers %q, want %q", view.ReinstallKeep, want)
	}

	// A full wipe still keeps nothing, and says so.
	code, rep = cleanReinstall(t, admin, url, map[string]any{"wipeData": true})
	if code != http.StatusOK || rep.Keep == nil || len(rep.Keep) != 0 {
		t.Errorf("full wipe = %d keep %v, want an empty list", code, rep.Keep)
	}
	if s, _ := st.GetServer(created.ID); len(s.InstallKeep) != 0 || !reflect.DeepEqual(s.ReinstallKeep, want) {
		t.Errorf("after a full wipe: keeping %q, remembering %q", s.InstallKeep, s.ReinstallKeep)
	}
}

// The panel offers what to keep: a template's own list, the Minecraft Java
// default for the templates with the eula feature, nothing for the others.
func TestTemplatesOfferWhatToKeep(t *testing.T) {
	ts, admin, st := newTestServerStore(t)
	post(t, admin, ts.URL+"/api/setup", map[string]string{"username": "admin", "password": "supersecret"})
	img := []models.TemplateImage{{DisplayName: "i", Ref: "i", Default: true}}
	for _, tpl := range []*models.Template{
		{Slug: "mc", Name: "MC", Images: img, Features: []string{"eula"}},
		{Slug: "bedrock", Name: "Bedrock", Images: img, ReinstallKeep: []string{"worlds", "allowlist.json"}},
		{Slug: "other", Name: "Other", Images: img},
	} {
		if _, err := st.UpsertTemplate(tpl); err != nil {
			t.Fatal(err)
		}
	}
	want := map[string][]string{
		"mc": models.MinecraftJavaKeep, "bedrock": {"worlds", "allowlist.json"}, "other": nil,
	}
	var list []struct {
		Slug                   string   `json:"slug"`
		EffectiveReinstallKeep []string `json:"effectiveReinstallKeep"`
	}
	getJSON(t, admin, ts.URL+"/api/templates", &list)
	for _, tpl := range list {
		if w, ok := want[tpl.Slug]; ok && !reflect.DeepEqual(tpl.EffectiveReinstallKeep, w) {
			t.Errorf("list: %s offers %q, want %q", tpl.Slug, tpl.EffectiveReinstallKeep, w)
		}
	}
	for slug, w := range want {
		var one struct {
			EffectiveReinstallKeep []string `json:"effectiveReinstallKeep"`
		}
		getJSON(t, admin, ts.URL+"/api/templates/"+slug, &one)
		if !reflect.DeepEqual(one.EffectiveReinstallKeep, w) {
			t.Errorf("get: %s offers %q, want %q", slug, one.EffectiveReinstallKeep, w)
		}
	}
}

// An administrator sets a template's list in its JSON; it is checked like a
// reinstall's, and what the panel computes is not stored.
func TestTemplateUpdateChecksWhatToKeep(t *testing.T) {
	ts, admin, st := newTestServerStore(t)
	post(t, admin, ts.URL+"/api/setup", map[string]string{"username": "admin", "password": "supersecret"})
	img := []models.TemplateImage{{DisplayName: "i", Ref: "i", Default: true}}
	if _, err := st.UpsertTemplate(&models.Template{Slug: "valheim", Name: "Valheim", Images: img}); err != nil {
		t.Fatal(err)
	}
	url := ts.URL + "/api/templates/valheim"
	doc := map[string]any{"name": "Valheim", "images": img, "reinstallKeep": []string{"../escape"}}
	if r := put(t, admin, url, doc); r.StatusCode != http.StatusBadRequest {
		t.Errorf("a path out of the volume = %d, want 400", r.StatusCode)
	}
	doc["reinstallKeep"] = []string{"./worlds_local/", "adminlist.txt"}
	doc["effectiveReinstallKeep"] = []string{"ignored"}
	if r := put(t, admin, url, doc); r.StatusCode != http.StatusOK {
		t.Fatalf("update = %d", r.StatusCode)
	}
	got, err := st.GetTemplateBySlug("valheim")
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got.ReinstallKeep, []string{"worlds_local", "adminlist.txt"}) {
		t.Errorf("stored %q", got.ReinstallKeep)
	}
	if got.EffectiveKeep != nil {
		t.Errorf("the computed list was stored: %q", got.EffectiveKeep)
	}
}
