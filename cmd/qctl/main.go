// Command qctl is a minimal admin CLI for Quetzal, used to drive the database
// (the source of truth) before the API server exists. Handy for Phase 0 testing.
package main

import (
	"fmt"
	"os"
	"strings"
	"text/tabwriter"

	"github.com/lolozini/quetzal/internal/egg"
	"github.com/lolozini/quetzal/internal/models"
	"github.com/lolozini/quetzal/internal/reconciler"
	"github.com/lolozini/quetzal/internal/store"
	"github.com/lolozini/quetzal/templates"
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	st := openStore()

	switch os.Args[1] {
	case "templates":
		cmdTemplates(st)
	case "import-egg":
		cmdImportEgg(st, os.Args[2:])
	case "create":
		cmdCreate(st, os.Args[2:])
	case "ls":
		cmdLs(st)
	case "set-state":
		cmdSetState(st, os.Args[2:])
	case "rm":
		cmdRm(st, os.Args[2:])
	default:
		usage()
		os.Exit(2)
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `qctl - Quetzal admin CLI

Usage:
  qctl templates                       List templates
  qctl import-egg <file.json>          Import a Pterodactyl egg as a template
  qctl create --template <slug> --name <name> [opts]
        [--image <ref>] [--memory 4G] [--cpu 2]
        [--storage pvc|hostpath] [--size 10Gi] [--hostpath /path]
        [--env KEY=VALUE ...] [--start]
  qctl ls                              List servers and status
  qctl set-state --slug <slug> --state Running|Stopped|Suspended
  qctl rm --slug <slug>                Delete a server (controller tears it down)

DB config via env: QUETZAL_DB_DRIVER (sqlite), QUETZAL_DB_DSN (quetzal.db)
`)
}

func openStore() *store.Store {
	st, err := store.Open(store.Config{
		Driver: store.Driver(env("QUETZAL_DB_DRIVER", "sqlite")),
		DSN:    env("QUETZAL_DB_DSN", "quetzal.db"),
		Silent: true,
	})
	must(err)
	must(st.Migrate())
	must(templates.Seed(st))
	return st
}

func cmdTemplates(st *store.Store) {
	ts, err := st.ListTemplates()
	must(err)
	w := tabwriter.NewWriter(os.Stdout, 0, 2, 2, ' ', 0)
	fmt.Fprintln(w, "SLUG\tNAME\tVERSION\tIMAGES\tPORTS")
	for _, t := range ts {
		fmt.Fprintf(w, "%s\t%s\t%d\t%d\t%d\n", t.Slug, t.Name, t.Version, len(t.Images), len(t.Ports))
	}
	w.Flush()
}

func cmdImportEgg(st *store.Store, args []string) {
	if len(args) < 1 {
		fatalf("import-egg requires a file path")
	}
	data, err := os.ReadFile(args[0])
	must(err)
	t, err := egg.ToTemplate(data)
	must(err)
	saved, err := st.UpsertTemplate(t)
	must(err)
	fmt.Printf("imported template %q (slug=%s, version=%d, %d variables)\n",
		saved.Name, saved.Slug, saved.Version, len(saved.Variables))
}

func cmdCreate(st *store.Store, args []string) {
	f := newFlags(args)
	tmplSlug := f.str("template", "")
	name := f.str("name", "")
	image := f.str("image", "")
	memory := f.str("memory", "")
	cpu := f.str("cpu", "")
	storageType := f.str("storage", "pvc")
	size := f.str("size", "10Gi")
	hostpath := f.str("hostpath", "")
	start := f.bool("start")
	envs := f.multi("env")

	if tmplSlug == "" || name == "" {
		fatalf("create requires --template and --name")
	}
	tmpl, err := st.GetTemplateBySlug(tmplSlug)
	if err != nil {
		fatalf("template %q not found", tmplSlug)
	}
	if image == "" {
		image = defaultImage(tmpl)
	}

	slug := egg.Slugify(name)
	envMap := map[string]string{}
	// Pre-fill template variable defaults, then apply overrides.
	for _, v := range tmpl.Variables {
		if v.Default != "" {
			envMap[v.EnvVariable] = v.Default
		}
	}
	for _, kv := range envs {
		k, val, ok := strings.Cut(kv, "=")
		if !ok {
			fatalf("invalid --env %q (want KEY=VALUE)", kv)
		}
		envMap[k] = val
	}

	state := models.StateStopped
	if start {
		state = models.StateRunning
	}

	storage := models.Storage{Type: models.StorageType(storageType)}
	if storage.Type == models.StorageHostPath {
		if hostpath == "" {
			fatalf("--hostpath required when --storage hostpath")
		}
		storage.HostPath = hostpath
	} else {
		storage.Size = size
	}

	srv := &models.Server{
		Slug:            slug,
		DisplayName:     name,
		TemplateID:      tmpl.ID,
		TemplateVersion: tmpl.Version,
		Image:           image,
		Namespace:       reconciler.NamespaceFor(slug),
		DesiredState:    state,
		Resources:       models.Resources{Memory: memory, CPU: cpu},
		Env:             envMap,
		Storage:         storage,
		Ports:           tmpl.Ports,
		Status:          models.Status{Phase: models.PhaseStopped},
	}
	must(st.CreateServer(srv))
	fmt.Printf("created server %q (slug=%s, namespace=%s, state=%s)\n",
		name, slug, srv.Namespace, state)
}

func cmdLs(st *store.Store) {
	srvs, err := st.ListServers()
	must(err)
	w := tabwriter.NewWriter(os.Stdout, 0, 2, 2, ' ', 0)
	fmt.Fprintln(w, "SLUG\tDESIRED\tPHASE\tNAMESPACE\tENDPOINTS")
	for _, s := range srvs {
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n",
			s.Slug, s.DesiredState, s.Status.Phase, s.Namespace, strings.Join(s.Status.Endpoints, ","))
	}
	w.Flush()
}

func cmdSetState(st *store.Store, args []string) {
	f := newFlags(args)
	slug := f.str("slug", "")
	state := f.str("state", "")
	if slug == "" || state == "" {
		fatalf("set-state requires --slug and --state")
	}
	srv, err := st.GetServerBySlug(slug)
	if err != nil {
		fatalf("server %q not found", slug)
	}
	srv.DesiredState = models.DesiredState(state)
	must(st.UpdateServer(srv))
	fmt.Printf("server %s desiredState=%s\n", slug, state)
}

func cmdRm(st *store.Store, args []string) {
	f := newFlags(args)
	slug := f.str("slug", "")
	if slug == "" {
		fatalf("rm requires --slug")
	}
	srv, err := st.GetServerBySlug(slug)
	if err != nil {
		fatalf("server %q not found", slug)
	}
	must(st.DeleteServer(srv.ID))
	fmt.Printf("deleted server %s (controller will tear down namespace %s)\n", slug, srv.Namespace)
}

func defaultImage(t *models.Template) string {
	for _, img := range t.Images {
		if img.Default {
			return img.Ref
		}
	}
	if len(t.Images) > 0 {
		return t.Images[0].Ref
	}
	return ""
}

// ---- tiny flag helper ----

type flags struct {
	vals   map[string]string
	multiM map[string][]string
	boolM  map[string]bool
}

func newFlags(args []string) *flags {
	f := &flags{vals: map[string]string{}, multiM: map[string][]string{}, boolM: map[string]bool{}}
	for i := 0; i < len(args); i++ {
		a := args[i]
		if !strings.HasPrefix(a, "--") {
			continue
		}
		key := strings.TrimPrefix(a, "--")
		// bool flags have no value or are followed by another --flag
		if i+1 >= len(args) || strings.HasPrefix(args[i+1], "--") {
			f.boolM[key] = true
			continue
		}
		val := args[i+1]
		i++
		f.vals[key] = val
		f.multiM[key] = append(f.multiM[key], val)
	}
	return f
}

func (f *flags) str(key, def string) string {
	if v, ok := f.vals[key]; ok {
		return v
	}
	return def
}
func (f *flags) bool(key string) bool      { return f.boolM[key] }
func (f *flags) multi(key string) []string { return f.multiM[key] }

func env(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func must(err error) {
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func fatalf(format string, a ...any) {
	fmt.Fprintf(os.Stderr, "error: "+format+"\n", a...)
	os.Exit(1)
}
