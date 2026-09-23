package api_test

import (
	"encoding/json"
	"net/http"
	"reflect"
	"strings"
	"testing"

	"github.com/lolozini/quetzal/internal/models"
	"github.com/lolozini/quetzal/internal/store"
)

func seedSwitchTemplates(t *testing.T, st *store.Store) {
	t.Helper()
	install := &models.InstallScript{Image: "alpine:3.20", Script: "echo install"}
	for _, tpl := range []*models.Template{
		{
			Slug: "egg-paper", Name: "Paper", Startup: "java -jar {{SERVER_JARFILE}}", DataPath: "/home/container",
			Install: install,
			Images: []models.TemplateImage{
				{DisplayName: "Java 17", Ref: "paper-java17", Default: true},
				{DisplayName: "Java 21", Ref: "shared-java21"},
			},
			Variables: []models.TemplateVariable{
				{Name: "Version", EnvVariable: "MC_VERSION", Type: models.VarString, Default: "latest", Editable: true},
				{Name: "Jar", EnvVariable: "SERVER_JARFILE", Type: models.VarString, Default: "server.jar", Editable: false},
				{Name: "RCON", EnvVariable: "RCON_PASSWORD", Type: models.VarString, Editable: true, Secret: true},
				{Name: "Paper only", EnvVariable: "PAPER_ONLY", Type: models.VarString, Default: "x", Editable: true},
				{Name: "Difficulty", EnvVariable: "DIFFICULTY", Type: models.VarEnum, Options: []string{"easy", "hard"}, Default: "easy", Editable: true},
			},
		},
		{
			Slug: "egg-fabric", Name: "Fabric", Startup: "java -jar {{SERVER_JARFILE}}", DataPath: "/home/container",
			Install: install,
			Images: []models.TemplateImage{
				{DisplayName: "Java 21", Ref: "fabric-java21", Default: true},
				{DisplayName: "Java 21 (shared)", Ref: "shared-java21"},
			},
			Variables: []models.TemplateVariable{
				{Name: "Version", EnvVariable: "MC_VERSION", Type: models.VarString, Default: "latest", Editable: true},
				{Name: "Jar", EnvVariable: "SERVER_JARFILE", Type: models.VarString, Default: "fabric.jar", Editable: false},
				{Name: "RCON", EnvVariable: "RCON_PASSWORD", Type: models.VarString, Editable: true, Secret: true},
				{Name: "Difficulty", EnvVariable: "DIFFICULTY", Type: models.VarEnum, Options: []string{"peaceful", "normal"}, Default: "normal", Editable: true},
				{Name: "Loader", EnvVariable: "LOADER_VERSION", Type: models.VarString, Default: "0.16", Editable: true},
			},
		},
		{
			Slug: "egg-modpack", Name: "Modpack", Startup: "run", DataPath: "/home/container", Install: install,
			Images:    []models.TemplateImage{{DisplayName: "img", Ref: "modpack-img", Default: true}},
			Variables: []models.TemplateVariable{{Name: "Project", EnvVariable: "PROJECT_ID", Type: models.VarString, Required: true, Editable: true}},
		},
		{
			Slug: "no-install", Name: "Image-driven", DataPath: "/data",
			Images: []models.TemplateImage{{DisplayName: "img", Ref: "itzg-like", Default: true}},
		},
	} {
		if _, err := st.UpsertTemplate(tpl); err != nil {
			t.Fatalf("seed %s: %v", tpl.Slug, err)
		}
	}
}

type reinstallReply struct {
	Status   string   `json:"status"`
	Template string   `json:"template"`
	Image    string   `json:"image"`
	Reset    []string `json:"reset"`
	Error    string   `json:"error"`
}

func reinstall(t *testing.T, c *http.Client, url string, body any) (int, reinstallReply) {
	t.Helper()
	r := post(t, c, url+"/reinstall", body)
	defer r.Body.Close()
	var rep reinstallReply
	_ = json.NewDecoder(r.Body).Decode(&rep)
	return r.StatusCode, rep
}

func TestReinstallCanChangeTheTemplate(t *testing.T) {
	ts, admin, st := newTestServerStore(t)
	seedSwitchTemplates(t, st)
	post(t, admin, ts.URL+"/api/setup", map[string]string{"username": "admin", "password": "supersecret"})
	createUser(t, admin, ts.URL, map[string]any{"username": "alice", "password": "alicepw12"})
	createUser(t, admin, ts.URL, map[string]any{"username": "mallory", "password": "mallorypw1"})
	alice := loginAs(t, ts.URL, "alice", "alicepw12")
	mallory := loginAs(t, ts.URL, "mallory", "mallorypw1")

	var created struct{ ID uint }
	r := post(t, alice, ts.URL+"/api/servers", map[string]any{
		"name": "world", "template": "egg-paper",
		"env": map[string]string{"MC_VERSION": "1.20.4", "RCON_PASSWORD": "s3cret", "DIFFICULTY": "hard", "PAPER_ONLY": "y"},
	})
	if r.StatusCode != http.StatusCreated {
		t.Fatalf("create = %d", r.StatusCode)
	}
	json.NewDecoder(r.Body).Decode(&created)
	url := ts.URL + "/api/servers/" + itoa(created.ID)
	gen := func() int { s, _ := st.GetServer(created.ID); return s.InstallGeneration }
	if rr := post(t, alice, url+"/access", map[string]any{"username": "mallory", "permissions": []string{"view", "settings"}}); rr.StatusCode != http.StatusNoContent {
		t.Fatalf("grant = %d", rr.StatusCode)
	}

	// A subuser trusted with the settings can still reinstall...
	if code, _ := reinstall(t, mallory, url, map[string]any{}); code != http.StatusOK {
		t.Fatalf("plain reinstall by a subuser = %d, want 200", code)
	}
	g := gen()
	// ...but turning the server into another game is the owner's call.
	if code, _ := reinstall(t, mallory, url, map[string]any{"template": "egg-fabric"}); code != http.StatusForbidden {
		t.Errorf("template change by a subuser = %d, want 403", code)
	}
	// A body that does not parse is refused, not read as a bare reinstall.
	if code, _ := reinstall(t, alice, url, map[string]any{"templat": "egg-fabric"}); code != http.StatusBadRequest {
		t.Errorf("misspelt field = %d, want 400", code)
	}
	if code, _ := reinstall(t, alice, url, map[string]any{"env": map[string]string{"MC_VERSION": "1.21"}}); code != http.StatusBadRequest {
		t.Errorf("env without a template change = %d, want 400", code)
	}
	if code, _ := reinstall(t, alice, url, map[string]any{"template": "egg-fabric", "image": "anything:latest"}); code != http.StatusBadRequest {
		t.Errorf("image off the template's list = %d, want 400", code)
	}
	if gen() != g {
		t.Fatalf("a refused request still scheduled an install (generation %d -> %d)", g, gen())
	}

	// The change itself.
	code, rep := reinstall(t, alice, url, map[string]any{"template": "egg-fabric"})
	if code != http.StatusOK {
		t.Fatalf("template change = %d (%s)", code, rep.Error)
	}
	if rep.Status != "reinstalling" || rep.Template != "egg-fabric" {
		t.Errorf("reply = %+v", rep)
	}
	// paper-java17 is not on Fabric's list, so its default is taken.
	if rep.Image != "fabric-java21" {
		t.Errorf("image = %q, want the new template's default", rep.Image)
	}
	// SERVER_JARFILE is fixed by the new template; "hard" is not one of its
	// difficulties. Both take the new defaults and are reported.
	if !reflect.DeepEqual(rep.Reset, []string{"DIFFICULTY", "SERVER_JARFILE"}) {
		t.Errorf("reset = %v, want [DIFFICULTY SERVER_JARFILE]", rep.Reset)
	}
	s, _ := st.GetServer(created.ID)
	fabric, _ := st.GetTemplateBySlug("egg-fabric")
	if s.TemplateID != fabric.ID || s.Image != "fabric-java21" {
		t.Errorf("server now on template %d image %q", s.TemplateID, s.Image)
	}
	if s.InstallGeneration != g+1 {
		t.Errorf("install generation = %d, want %d: the new template's install must run", s.InstallGeneration, g+1)
	}
	wantEnv := map[string]string{"MC_VERSION": "1.20.4", "SERVER_JARFILE": "fabric.jar", "DIFFICULTY": "normal", "LOADER_VERSION": "0.16"}
	if !reflect.DeepEqual(s.Env, wantEnv) {
		t.Errorf("env = %v\nwant  %v", s.Env, wantEnv)
	}
	secrets, err := st.OpenSecrets(s.SecretEnvEnc)
	if err != nil || secrets["RCON_PASSWORD"] != "s3cret" {
		t.Errorf("the RCON password was not carried over as a secret: %v %v", secrets, err)
	}
}

// An image the new template also offers is kept rather than swapped for its
// default: the Java version somebody chose is not the template's to overrule.
func TestReinstallKeepsAnImageTheNewTemplateOffers(t *testing.T) {
	ts, admin, st := newTestServerStore(t)
	seedSwitchTemplates(t, st)
	post(t, admin, ts.URL+"/api/setup", map[string]string{"username": "admin", "password": "supersecret"})
	var created struct{ ID uint }
	r := post(t, admin, ts.URL+"/api/servers", map[string]any{"name": "w", "template": "egg-paper", "image": "shared-java21"})
	json.NewDecoder(r.Body).Decode(&created)
	code, rep := reinstall(t, admin, ts.URL+"/api/servers/"+itoa(created.ID), map[string]any{"template": "egg-fabric"})
	if code != http.StatusOK || rep.Image != "shared-java21" {
		t.Errorf("= %d image %q, want the current image kept", code, rep.Image)
	}
}

// A variable the new template requires and nothing can fill has to be given.
func TestReinstallAsksForARequiredVariable(t *testing.T) {
	ts, admin, st := newTestServerStore(t)
	seedSwitchTemplates(t, st)
	post(t, admin, ts.URL+"/api/setup", map[string]string{"username": "admin", "password": "supersecret"})
	var created struct{ ID uint }
	r := post(t, admin, ts.URL+"/api/servers", map[string]any{"name": "w", "template": "egg-paper"})
	json.NewDecoder(r.Body).Decode(&created)
	url := ts.URL + "/api/servers/" + itoa(created.ID)

	code, rep := reinstall(t, admin, url, map[string]any{"template": "egg-modpack"})
	if code != http.StatusBadRequest || !strings.Contains(rep.Error, "PROJECT_ID") {
		t.Fatalf("= %d %q, want 400 naming PROJECT_ID", code, rep.Error)
	}
	if code, rep := reinstall(t, admin, url, map[string]any{"template": "egg-modpack", "env": map[string]string{"PROJECT_ID": "925200"}}); code != http.StatusOK {
		t.Fatalf("with the value = %d (%s)", code, rep.Error)
	}
	s, _ := st.GetServer(created.ID)
	if s.Env["PROJECT_ID"] != "925200" {
		t.Errorf("env = %v", s.Env)
	}
}

// A template with no install step can still be switched to -- the image does
// the work at start -- but there is nothing to run and nothing to wipe with.
func TestReinstallToATemplateWithoutAnInstallStep(t *testing.T) {
	ts, admin, st := newTestServerStore(t)
	seedSwitchTemplates(t, st)
	post(t, admin, ts.URL+"/api/setup", map[string]string{"username": "admin", "password": "supersecret"})
	var created struct{ ID uint }
	r := post(t, admin, ts.URL+"/api/servers", map[string]any{"name": "w", "template": "egg-paper"})
	json.NewDecoder(r.Body).Decode(&created)
	url := ts.URL + "/api/servers/" + itoa(created.ID)
	before, _ := st.GetServer(created.ID)

	if code, _ := reinstall(t, admin, url, map[string]any{"template": "no-install", "wipeData": true}); code != http.StatusBadRequest {
		t.Errorf("wipe with no install step = %d, want 400", code)
	}
	code, rep := reinstall(t, admin, url, map[string]any{"template": "no-install"})
	if code != http.StatusOK || rep.Status != "switched" {
		t.Fatalf("= %d %+v", code, rep)
	}
	after, _ := st.GetServer(created.ID)
	if after.InstallGeneration != before.InstallGeneration {
		t.Errorf("install generation moved (%d -> %d) with no install to run", before.InstallGeneration, after.InstallGeneration)
	}
	// Back to a template that installs: its install has to run.
	if code, _ := reinstall(t, admin, url, map[string]any{"template": "egg-fabric"}); code != http.StatusOK {
		t.Fatalf("switch back = %d", code)
	}
	if back, _ := st.GetServer(created.ID); back.InstallGeneration != before.InstallGeneration+1 {
		t.Errorf("switching back did not schedule the install")
	}
}
