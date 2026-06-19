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
