package store

import (
	"reflect"
	"strings"
	"testing"

	"github.com/lolozini/quetzal/internal/models"
)

func TestMigrationProtectsExistingCLISecrets(t *testing.T) {
	s := newTestStore(t)
	tmpl, err := s.UpsertTemplate(&models.Template{Slug: "legacy-cli", Name: "Legacy CLI", Variables: []models.TemplateVariable{
		{EnvVariable: "PRIVATE", Secret: true}, {EnvVariable: "EXISTING", Secret: true}, {EnvVariable: "EMPTY", Secret: true},
	}})
	if err != nil {
		t.Fatal(err)
	}
	sealed, err := s.SealSecrets(map[string]string{"EXISTING": "authoritative", "OTHER": "preserve-sealed"})
	if err != nil {
		t.Fatal(err)
	}
	srv := &models.Server{Slug: "legacy", TemplateID: tmpl.ID, TemplateVersion: tmpl.Version, Env: map[string]string{
		"PRIVATE": "old-cli-secret", "EXISTING": "stale-public", "EMPTY": "", "UNKNOWN": "keep-override",
	}, SecretEnvEnc: sealed}
	if err := s.CreateServer(srv); err != nil {
		t.Fatal(err)
	}
	// A running server can still use the old secret definition after an edit.
	tmpl.Variables = nil
	if _, err := s.UpsertTemplate(tmpl); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := s.Migrate(); err != nil {
			t.Fatal(err)
		}
		got, err := s.GetServer(srv.ID)
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(got.Env, map[string]string{"UNKNOWN": "keep-override"}) {
			t.Fatalf("public environment = %v", got.Env)
		}
		if !strings.HasPrefix(got.SecretEnvEnc, "enc:") || strings.Contains(got.SecretEnvEnc, "old-cli-secret") {
			t.Fatal("legacy secret not encrypted")
		}
		secrets, err := s.OpenSecrets(got.SecretEnvEnc)
		if err != nil {
			t.Fatal(err)
		}
		want := map[string]string{"PRIVATE": "old-cli-secret", "EXISTING": "authoritative", "EMPTY": "", "OTHER": "preserve-sealed"}
		if !reflect.DeepEqual(secrets, want) {
			t.Fatalf("secrets changed: %v", secrets)
		}
	}
}

func TestSecretMigrationFailsWithoutDestroyingUnreadableSecrets(t *testing.T) {
	s := newTestStore(t)
	tmpl, err := s.UpsertTemplate(&models.Template{Slug: "secret", Name: "Secret", Variables: []models.TemplateVariable{{EnvVariable: "PRIVATE", Secret: true}}})
	if err != nil {
		t.Fatal(err)
	}
	sealed, err := s.SealSecrets(map[string]string{"OTHER": "keep"})
	if err != nil {
		t.Fatal(err)
	}
	srv := &models.Server{Slug: "legacy", TemplateID: tmpl.ID, Env: map[string]string{"PRIVATE": "legacy-secret"}, SecretEnvEnc: sealed}
	if err := s.CreateServer(srv); err != nil {
		t.Fatal(err)
	}
	s.key = []byte(strings.Repeat("x", 32))
	if err := s.Migrate(); err == nil {
		t.Fatal("migration accepted the wrong encryption key")
	}
	got, err := s.GetServer(srv.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got.Env, srv.Env) || got.SecretEnvEnc != sealed {
		t.Fatal("failed migration changed the environment")
	}
}
