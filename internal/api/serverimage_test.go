package api_test

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/lolozini/quetzal/internal/models"
	"github.com/lolozini/quetzal/internal/reconciler"
)

// Going from Java 21 to Java 25 took a reinstall, which re-ran the install
// script and downloaded the game again: PATCH refused an image ("invalid
// body"). Pterodactyl makes it a choice in the startup settings. The image is
// a setting now, among the template's images as at creation, and it does not
// reinstall anything.
func TestAServerSwitchesImageWithoutAReinstall(t *testing.T) {
	ts, admin, st := newTestServerStore(t)
	setupAdmin(t, ts.URL, admin)
	createUser(t, admin, ts.URL, map[string]any{"username": "alice", "password": "alicepw12"})
	alice := loginAs(t, ts.URL, "alice", "alicepw12")
	owner, err := st.GetUserByUsername("alice")
	if err != nil {
		t.Fatal(err)
	}
	tpl, err := st.UpsertTemplate(&models.Template{
		Slug: "paper-java", Name: "Paper",
		Images: []models.TemplateImage{
			{DisplayName: "Java 21", Ref: "ghcr.io/parkervcp/yolks:java_21", Default: true},
			{DisplayName: "Java 25", Ref: "ghcr.io/parkervcp/yolks:java_25"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	srv := &models.Server{
		Slug: "survival-a1b2", DisplayName: "survival", Namespace: reconciler.NamespaceFor("survival-a1b2"),
		OwnerID: owner.ID, TemplateID: tpl.ID, Image: "ghcr.io/parkervcp/yolks:java_21", InstallGeneration: 1,
	}
	if err := st.CreateServer(srv); err != nil {
		t.Fatal(err)
	}
	setImage := func(c *http.Client, image string) (int, string) {
		t.Helper()
		r := doMethod(t, c, http.MethodPatch, ts.URL+"/api/servers/"+itoa(srv.ID), map[string]string{"image": image})
		var body struct{ Image, Error string }
		_ = json.NewDecoder(r.Body).Decode(&body)
		return r.StatusCode, body.Image + body.Error
	}

	if code, got := setImage(alice, "ghcr.io/parkervcp/yolks:java_25"); code != http.StatusOK || got != "ghcr.io/parkervcp/yolks:java_25" {
		t.Fatalf("switching to the template's other image = %d %q", code, got)
	}
	after, _ := st.GetServer(srv.ID)
	if after.Image != "ghcr.io/parkervcp/yolks:java_25" || after.InstallGeneration != 1 || after.InstallWipe {
		t.Errorf("after the switch: image %s, install generation %d, wipe %v -- want the new image and no reinstall", after.Image, after.InstallGeneration, after.InstallWipe)
	}
	if code, _ := setImage(alice, "docker.io/someone/else:latest"); code != http.StatusBadRequest {
		t.Errorf("an image off the template's list, by its owner = %d, want 400", code)
	}
	if code, _ := setImage(admin, "ghcr.io/parkervcp/yolks:java_26"); code != http.StatusOK {
		t.Errorf("an image off the list, by an administrator = %d, want 200", code)
	}
}
