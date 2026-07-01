// Package egg imports Pterodactyl/Pelican "egg" JSON into Quetzal templates.
// The egg format is a de-facto interchange standard, so supporting it lets
// users migrate their existing catalog directly.
package egg

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"

	"github.com/lolozini/quetzal/internal/models"
)

// eggFile mirrors the Pterodactyl egg export (PTDL_v1/v2).
type eggFile struct {
	Name         string            `json:"name"`
	Author       string            `json:"author"`
	Description  string            `json:"description"`
	Features     []string          `json:"features"`
	DockerImages map[string]string `json:"docker_images"`
	// Older single-image eggs use "image".
	Image        string        `json:"image"`
	FileDenylist []string      `json:"file_denylist"`
	Startup      string        `json:"startup"`
	Config       eggConfig     `json:"config"`
	Scripts      eggScripts    `json:"scripts"`
	Variables    []eggVariable `json:"variables"`
}

// eggConfig sub-objects are exported as *stringified JSON*, so we keep them raw
// and decode leniently (string-wrapped or inline object both accepted).
type eggConfig struct {
	Files   json.RawMessage `json:"files"`
	Startup json.RawMessage `json:"startup"`
	Stop    json.RawMessage `json:"stop"`
}

type eggScripts struct {
	Installation eggInstall `json:"installation"`
}

type eggInstall struct {
	Script     string `json:"script"`
	Container  string `json:"container"`
	Entrypoint string `json:"entrypoint"`
}

type eggVariable struct {
	Name         string `json:"name"`
	Description  string `json:"description"`
	EnvVariable  string `json:"env_variable"`
	DefaultValue string `json:"default_value"`
	UserViewable bool   `json:"user_viewable"`
	UserEditable bool   `json:"user_editable"`
	Rules        string `json:"rules"`
	FieldType    string `json:"field_type"`
}

// ToTemplate parses an egg JSON document into a Quetzal template.
func ToTemplate(data []byte) (*models.Template, error) {
	var e eggFile
	if err := json.Unmarshal(data, &e); err != nil {
		return nil, fmt.Errorf("parse egg: %w", err)
	}
	if e.Name == "" {
		return nil, fmt.Errorf("egg has no name")
	}

	t := &models.Template{
		Slug:         Slugify(e.Name),
		Name:         e.Name,
		Author:       e.Author,
		Description:  e.Description,
		Startup:      normalizeNewlines(e.Startup),
		Features:     e.Features,
		FileDenylist: e.FileDenylist,
		// Pterodactyl eggs assume the server directory is /home/container (Wings'
		// guaranteed path): many hardcode it in config.files (e.g. Terraria's
		// worldpath) or resolve files via it. Mount the data volume there — as
		// Wings does — so those paths land on the volume. (Built-in Quetzal
		// templates set their own DataPath, e.g. /data.)
		DataPath: "/home/container",
		Console:  models.ConsoleConfig{Type: models.ConsoleAttach},
	}

	// Images.
	if len(e.DockerImages) > 0 {
		first := true
		for display, ref := range e.DockerImages {
			t.Images = append(t.Images, models.TemplateImage{
				DisplayName: display,
				Ref:         ref,
				Default:     first,
			})
			first = false
		}
	} else if e.Image != "" {
		t.Images = append(t.Images, models.TemplateImage{
			DisplayName: e.Image, Ref: e.Image, Default: true,
		})
	}

	// Stop command (config.stop may be a bare string).
	var stop string
	if len(e.Config.Stop) > 0 {
		_ = decodeMaybeString(e.Config.Stop, &stop)
		if stop == "" {
			// config.stop is often just a JSON string literal.
			_ = json.Unmarshal(e.Config.Stop, &stop)
		}
		t.StopCommand = stop
	}

	// Done regex (config.startup -> {"done": "..."}).
	if len(e.Config.Startup) > 0 {
		var su struct {
			Done json.RawMessage `json:"done"`
		}
		if err := decodeMaybeString(e.Config.Startup, &su); err == nil && len(su.Done) > 0 {
			var done string
			if json.Unmarshal(su.Done, &done) == nil {
				t.DoneRegex = done
			}
		}
	}

	// Config files (config.files -> map path -> {parser, find}).
	if len(e.Config.Files) > 0 {
		var files map[string]struct {
			Parser string            `json:"parser"`
			Find   map[string]string `json:"find"`
		}
		if err := decodeMaybeString(e.Config.Files, &files); err == nil {
			for path, spec := range files {
				t.ConfigFiles = append(t.ConfigFiles, models.ConfigFile{
					Path:   path,
					Parser: models.ConfigFileParser(spec.Parser),
					Find:   spec.Find,
				})
			}
		}
	}

	// Install script.
	if e.Scripts.Installation.Script != "" {
		t.Install = &models.InstallScript{
			Image:      e.Scripts.Installation.Container,
			Entrypoint: e.Scripts.Installation.Entrypoint,
			Script:     normalizeNewlines(e.Scripts.Installation.Script),
		}
	}

	// Variables.
	for _, v := range e.Variables {
		t.Variables = append(t.Variables, convertVariable(v))
	}

	return t, nil
}

var enumRuleRe = regexp.MustCompile(`\bin:([^|]+)`)

func convertVariable(v eggVariable) models.TemplateVariable {
	tv := models.TemplateVariable{
		Name:        v.Name,
		Description: v.Description,
		EnvVariable: v.EnvVariable,
		Default:     v.DefaultValue,
		Rules:       v.Rules,
		Viewable:    v.UserViewable,
		Editable:    v.UserEditable,
		Required:    strings.Contains(v.Rules, "required"),
		Type:        models.VarString,
	}

	switch {
	case v.FieldType == "select" || enumRuleRe.MatchString(v.Rules):
		tv.Type = models.VarEnum
		if m := enumRuleRe.FindStringSubmatch(v.Rules); m != nil {
			for _, opt := range strings.Split(m[1], ",") {
				if opt = strings.TrimSpace(opt); opt != "" {
					tv.Options = append(tv.Options, opt)
				}
			}
		}
	case strings.Contains(v.Rules, "boolean"):
		tv.Type = models.VarBool
	case strings.Contains(v.Rules, "integer") || strings.Contains(v.Rules, "numeric"):
		tv.Type = models.VarInt
	}
	return tv
}

// decodeMaybeString unmarshals raw into v, transparently unwrapping a JSON
// string that itself contains JSON (the Pterodactyl egg export convention).
func decodeMaybeString(raw json.RawMessage, v any) error {
	trimmed := strings.TrimSpace(string(raw))
	if strings.HasPrefix(trimmed, "\"") {
		var inner string
		if err := json.Unmarshal(raw, &inner); err != nil {
			return err
		}
		return json.Unmarshal([]byte(inner), v)
	}
	return json.Unmarshal(raw, v)
}

var nonSlug = regexp.MustCompile(`[^a-z0-9]+`)

// Slugify converts a name into a DNS-friendly slug.
// normalizeNewlines converts Windows/old-Mac line endings to '\n'. Pterodactyl
// panel egg exports frequently carry CRLF, which breaks POSIX shells when the
// startup/install script is run (a stray '\r' makes `then\r` not a keyword).
func normalizeNewlines(s string) string {
	s = strings.ReplaceAll(s, "\r\n", "\n")
	return strings.ReplaceAll(s, "\r", "\n")
}

func Slugify(s string) string {
	s = strings.ToLower(s)
	s = nonSlug.ReplaceAllString(s, "-")
	return strings.Trim(s, "-")
}
