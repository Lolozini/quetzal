package egg

import (
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"strings"
	"time"

	"github.com/lolozini/quetzal/internal/models"
)

// defaultDataPath matches what an egg import assumes: Wings' guaranteed server
// directory, which egg scripts and config.files hardcode.
const defaultDataPath = "/home/container"

// Parse turns an uploaded template document into a template, accepting both
// shapes a user can plausibly hand the import endpoint: a Pterodactyl/Pelican
// egg, and Quetzal's own export.
//
// The second used to be rejected in the most unhelpful way possible — not with
// an error but with a success. The two formats name the same things differently
// (env_variable vs envVariable, docker_images vs images, scripts.installation vs
// install), so feeding an export to the egg parser produced a template that
// imported with 201 and had no images, no install script, and every variable's
// env name blank. The first sign of trouble came later, on creating a server:
// `variable "" is required`, which names nothing and points nowhere.
func Parse(data []byte) (*models.Template, error) {
	native, err := looksNative(data)
	if err != nil {
		return nil, err
	}
	if !native {
		return ToTemplate(data)
	}
	return fromNative(data)
}

// looksNative decides which format a document is in by the keys only one of them
// uses. Egg keys are checked first: an egg is what most imports are, and a
// hand-written one is likelier to be missing a key than to carry a Quetzal one.
func looksNative(data []byte) (bool, error) {
	var keys map[string]json.RawMessage
	if err := json.Unmarshal(data, &keys); err != nil {
		return false, fmt.Errorf("parse template: %w", err)
	}
	for _, k := range []string{"docker_images", "scripts", "config", "file_denylist"} {
		if _, ok := keys[k]; ok {
			return false, nil
		}
	}
	for _, k := range []string{"dataPath", "console", "images", "configFiles", "stopCommand"} {
		if _, ok := keys[k]; ok {
			return true, nil
		}
	}
	// Neither vocabulary showed up. Treat it as an egg so the error the user gets
	// is the egg parser's, which is the format the endpoint is documented for.
	return false, nil
}

// fromNative decodes Quetzal's own export. Identity and bookkeeping columns are
// dropped: a document carrying an id would otherwise be inserted over whatever
// row holds it here, and a stale version would fight the store's own bump.
func fromNative(data []byte) (*models.Template, error) {
	var t models.Template
	if err := json.Unmarshal(data, &t); err != nil {
		return nil, fmt.Errorf("parse template: %w", err)
	}
	t.Name = strings.TrimSpace(t.Name)
	if t.Name == "" {
		return nil, errors.New("template has no name")
	}
	t.ID, t.Version = 0, 0
	t.CreatedAt, t.UpdatedAt = time.Time{}, time.Time{}
	if strings.TrimSpace(t.Slug) == "" {
		t.Slug = Slugify(t.Name)
	}
	if p := strings.TrimSpace(t.DataPath); p != "" {
		if !strings.HasPrefix(p, "/") || path.Clean(p) != p || p == "/" {
			return nil, fmt.Errorf(
				"dataPath %q must be an absolute, already-clean directory other than \"/\"", p)
		}
	}
	// An export always carries these; a hand-edited document may not, and a
	// template with no image or no console cannot run a server.
	if len(t.Images) == 0 {
		return nil, errors.New("template has no images")
	}
	if t.Console.Type == "" {
		t.Console.Type = models.ConsoleAttach
	}
	if t.DataPath == "" {
		t.DataPath = defaultDataPath
	}
	return &t, nil
}
