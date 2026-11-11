// Package templates ships the built-in, game-agnostic Quetzal templates and a
// seeder that loads them into the store. Minecraft is just one example among
// several; nothing here is Minecraft-specific.
package templates

import (
	"embed"
	"encoding/json"
	"fmt"
	"io/fs"

	"github.com/lolozini/quetzal/internal/models"
)

//go:embed *.json
var builtinFS embed.FS

// Builtin parses and returns all built-in templates.
func Builtin() ([]models.Template, error) {
	entries, err := fs.Glob(builtinFS, "*.json")
	if err != nil {
		return nil, err
	}
	var out []models.Template
	for _, name := range entries {
		data, err := builtinFS.ReadFile(name)
		if err != nil {
			return nil, err
		}
		var t models.Template
		if err := json.Unmarshal(data, &t); err != nil {
			return nil, fmt.Errorf("parse builtin template %s: %w", name, err)
		}
		out = append(out, t)
	}
	return out, nil
}

// upserter is the subset of the store used to seed templates.
type upserter interface {
	UpsertTemplate(*models.Template) (*models.Template, error)
	GetTemplateBySlug(string) (*models.Template, error)
}

// Seed inserts built-in templates that are not already present (idempotent;
// does not clobber user edits to existing slugs).
func Seed(s upserter) error {
	builtins, err := Builtin()
	if err != nil {
		return err
	}
	for i := range builtins {
		t := builtins[i]
		if _, err := s.GetTemplateBySlug(t.Slug); err == nil {
			continue // already present
		}
		if _, err := s.UpsertTemplate(&t); err != nil {
			return fmt.Errorf("seed template %s: %w", t.Slug, err)
		}
	}
	return nil
}

// SeededSetting is the settings key that records an instance has been through
// SeedOnce. Without it, an administrator who deleted the built-in templates would
// get them back at every restart.
const SeededSetting = "templates.seeded"

// onceSeeder is the subset of the store SeedOnce needs.
type onceSeeder interface {
	upserter
	ListTemplates() ([]models.Template, error)
	GetSetting(key string) (string, error)
	SetSetting(key, value string) error
}

// SeedOnce gives an instance that has no template the built-in ones, the first
// time it starts, so a new install can create a first server without importing
// an egg. It only ever acts once per database: an instance that already has
// templates (every install made before this existed, an administrator's own work)
// is left as it is and marked, and so is one whose administrator has since deleted
// them all. It reports whether it seeded.
func SeedOnce(s onceSeeder) (bool, error) {
	if v, err := s.GetSetting(SeededSetting); err != nil {
		return false, err
	} else if v != "" {
		return false, nil
	}
	existing, err := s.ListTemplates()
	if err != nil {
		return false, err
	}
	seeded := len(existing) == 0
	if seeded {
		if err := Seed(s); err != nil {
			return false, err // not marked: the next start tries again
		}
	}
	return seeded, s.SetSetting(SeededSetting, "1")
}
