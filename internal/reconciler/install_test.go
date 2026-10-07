package reconciler

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"

	"github.com/lolozini/quetzal/internal/models"
)

// execInstall runs the wrapped install script against a real temp dir as the
// "mount" with the given generation/wipe env.
func execInstall(t *testing.T, mount, gen, wipe string) {
	t.Helper()
	userScript := `echo x >> "` + mount + `/ran.log"`
	cmd := exec.Command("sh", "-c", buildInstallScript(mount))
	cmd.Env = append(os.Environ(),
		"QUETZAL_INSTALL_GEN="+gen, "QUETZAL_INSTALL_WIPE="+wipe,
		"QUETZAL_INSTALL_RESOLVED_SHELL=sh",
		"QUETZAL_INSTALL_USER_SCRIPT="+userScript)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("install script: %v\n%s", err, out)
	}
}

// runInstall reports whether the user script ran this invocation (it appends a
// line to ran.log; we compare the count before/after). Not valid across a wipe,
// which deletes ran.log.
func runInstall(t *testing.T, mount, gen, wipe string) (ran bool) {
	t.Helper()
	before := lineCount(filepath.Join(mount, "ran.log"))
	execInstall(t, mount, gen, wipe)
	return lineCount(filepath.Join(mount, "ran.log")) > before
}

func lineCount(path string) int {
	b, err := os.ReadFile(path)
	if err != nil {
		return 0
	}
	n := 0
	for _, c := range b {
		if c == '\n' {
			n++
		}
	}
	return n
}

func TestInstallScriptGenerationFlow(t *testing.T) {
	mount := t.TempDir()

	// Fresh server, generation 1: installs.
	if !runInstall(t, mount, "1", "0") {
		t.Fatal("first install should run")
	}
	// Restart at the same generation: skips.
	if runInstall(t, mount, "1", "0") {
		t.Error("same-generation restart should not re-run install")
	}
	// Reinstall (generation bumped to 2): runs again.
	if !runInstall(t, mount, "2", "0") {
		t.Error("generation bump should re-run install")
	}
	if runInstall(t, mount, "2", "0") {
		t.Error("restart after reinstall should skip again")
	}
}

func TestInstallScriptLegacyMarkerTreatedAsInstalled(t *testing.T) {
	mount := t.TempDir()
	// Simulate a legacy (pre-generation) marker: an empty file.
	if err := os.WriteFile(filepath.Join(mount, ".quetzal-installed"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	// Upgrading to the generation-aware script with gen 0 must NOT re-run.
	if runInstall(t, mount, "0", "0") {
		t.Error("legacy marker with generation 0 must be treated as installed")
	}
	// But an explicit reinstall (generation 1) re-runs even over a legacy marker.
	if !runInstall(t, mount, "1", "0") {
		t.Error("reinstall of a legacy server should run")
	}
}

func TestInstallScriptWipe(t *testing.T) {
	mount := t.TempDir()
	if !runInstall(t, mount, "1", "0") {
		t.Fatal("install should run")
	}
	// A leftover data file + a dotfile.
	_ = os.WriteFile(filepath.Join(mount, "world.dat"), []byte("data"), 0o644)
	_ = os.WriteFile(filepath.Join(mount, ".hidden"), []byte("h"), 0o644)

	// Reinstall with wipe (generation 2, wipe 1): the data files are gone and the
	// script ran (ran.log recreated). Use filesystem state, not the line counter,
	// since the wipe deletes ran.log.
	execInstall(t, mount, "2", "1")
	if _, err := os.Stat(filepath.Join(mount, "world.dat")); !os.IsNotExist(err) {
		t.Error("wipe should have removed world.dat")
	}
	if _, err := os.Stat(filepath.Join(mount, ".hidden")); !os.IsNotExist(err) {
		t.Error("wipe should have removed the dotfile")
	}
	if lineCount(filepath.Join(mount, "ran.log")) == 0 {
		t.Error("wipe reinstall should have re-run the script")
	}
	// The marker is rewritten, so a normal restart skips.
	if runInstall(t, mount, "2", "1") {
		t.Error("post-wipe restart at same generation should skip")
	}
}

// cleanShells are the shells the clean reinstall's wipe is run under here: the
// install step runs it with the one its egg asks for, falling back to bash,
// ash and sh, so it has to behave the same in each that is installed.
func cleanShells() [][]string {
	var out [][]string
	for _, sh := range [][]string{{"sh"}, {"dash"}, {"bash"}, {"busybox", "ash"}} {
		if _, err := exec.LookPath(sh[0]); err == nil {
			out = append(out, sh)
		}
	}
	return out
}

// runClean runs the clean reinstall's guard under shell, generation gen, with
// keep as QUETZAL_INSTALL_KEEP and userScript as the egg's install.
func runClean(t *testing.T, shell []string, mount, gen, keep, userScript string) (string, error) {
	t.Helper()
	cmd := exec.Command(shell[0], append(shell[1:], "-c", buildCleanInstallScript(mount))...)
	cmd.Env = append(os.Environ(),
		"QUETZAL_INSTALL_GEN="+gen, "QUETZAL_INSTALL_WIPE=1", "QUETZAL_INSTALL_KEEP="+keep,
		"QUETZAL_INSTALL_RESOLVED_SHELL=sh",
		"QUETZAL_INSTALL_USER_SCRIPT="+userScript)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// tree writes files (and the directories they need) under root.
func tree(t *testing.T, root string, files ...string) {
	t.Helper()
	for _, f := range files {
		p := filepath.Join(root, f)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(f), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

// present reports whether p is there, a dangling link included.
func present(p string) bool {
	_, err := os.Lstat(p)
	return err == nil
}

// A modpack update: the world and the server's own files stay, what the old
// pack shipped goes, and the install runs on what is left.
func TestCleanReinstallKeepsOnlyWhatIsListed(t *testing.T) {
	for _, sh := range cleanShells() {
		t.Run(sh[len(sh)-1], func(t *testing.T) {
			mount := t.TempDir()
			tree(t, mount,
				"world/region/r.0.0.mca", "world_nether/level.dat", "server.properties", "ops.json",
				"mods/old-only.jar", "kubejs/server_scripts/old.js", "libraries/net/x.jar",
				"config/kept.toml", "config/old.toml", "config/sub/kept.toml", "config/sub/old.toml",
				"My World/level.dat", ".hidden/x", ".dotfile", "..odd", "plugins/Essentials/config.yml")
			_ = os.WriteFile(filepath.Join(mount, ".quetzal-installed"), []byte("1"), 0o644)
			keep := "world*\nserver.properties\nconfig/kept.toml\nconfig/sub/kept.toml\nMy World\nplugins\n.hidden\nno-such-thing"
			out, err := runClean(t, sh, mount, "2", keep, `mkdir -p "`+mount+`/mods" && echo new > "`+mount+`/mods/new.jar"`)
			if err != nil {
				t.Fatalf("clean reinstall: %v\n%s", err, out)
			}
			for _, kept := range []string{
				"world/region/r.0.0.mca", "world_nether/level.dat", "server.properties",
				"config/kept.toml", "config/sub/kept.toml", "My World/level.dat", ".hidden/x",
				"plugins/Essentials/config.yml", "mods/new.jar",
			} {
				if !present(filepath.Join(mount, kept)) {
					t.Errorf("%s is gone", kept)
				}
			}
			for _, gone := range []string{
				"mods/old-only.jar", "kubejs", "libraries", "ops.json",
				"config/old.toml", "config/sub/old.toml", ".dotfile", "..odd",
			} {
				if present(filepath.Join(mount, gone)) {
					t.Errorf("%s survived", gone)
				}
			}
			if b, _ := os.ReadFile(filepath.Join(mount, ".quetzal-installed")); string(b) != "2" {
				t.Errorf("marker = %q, want the new generation", b)
			}
			// What it kept, and what it could not find, are in the install log.
			for _, line := range []string{"  world_nether", "  config/sub/kept.toml", "nothing matches 'no-such-thing'"} {
				if !strings.Contains(out, line) {
					t.Errorf("install log lacks %q:\n%s", line, out)
				}
			}
		})
	}
}

// Keeping a directory keeps it whole, whatever else is listed inside it.
func TestCleanReinstallKeepsADirectoryWhole(t *testing.T) {
	for _, sh := range cleanShells() {
		t.Run(sh[len(sh)-1], func(t *testing.T) {
			mount := t.TempDir()
			tree(t, mount, "config/a.toml", "config/b.toml", "mods/x.jar")
			if out, err := runClean(t, sh, mount, "2", "config/a.toml\nconfig", "true"); err != nil {
				t.Fatalf("%v\n%s", err, out)
			}
			if !present(filepath.Join(mount, "config/b.toml")) {
				t.Error("config/b.toml went, though config is kept")
			}
			if present(filepath.Join(mount, "mods")) {
				t.Error("mods survived")
			}
		})
	}
}

// A path to keep is a pattern and nothing else: what it spells is never run.
func TestCleanReinstallPatternsAreNotRun(t *testing.T) {
	for _, sh := range cleanShells() {
		t.Run(sh[len(sh)-1], func(t *testing.T) {
			mount := t.TempDir()
			outside := t.TempDir()
			tree(t, mount, "$(touch x)", "a;b", "-rf", "back\\slash")
			keep := strings.Join([]string{
				"$(touch " + outside + "/ran1)", "`touch " + outside + "/ran2`",
				"a;b", "$(touch x)", "${HOME:?boom}",
			}, "\n")
			if out, err := runClean(t, sh, mount, "2", keep, "true"); err != nil {
				t.Fatalf("%v\n%s", err, out)
			}
			for _, f := range []string{"ran1", "ran2"} {
				if present(filepath.Join(outside, f)) {
					t.Errorf("a path to keep was run: %s exists", f)
				}
			}
			for _, f := range []string{"$(touch x)", "a;b"} {
				if !present(filepath.Join(mount, f)) {
					t.Errorf("%q, kept by its literal name, is gone", f)
				}
			}
			// Names rm or echo could read as something else go like any other.
			for _, f := range []string{"-rf", "back\\slash"} {
				if present(filepath.Join(mount, f)) {
					t.Errorf("%q survived", f)
				}
			}
		})
	}
}

// Links are never followed: one leading out of the volume goes, and nothing
// it leads to; one leading to a kept path is kept as it is.
func TestCleanReinstallDoesNotFollowLinks(t *testing.T) {
	for _, sh := range cleanShells() {
		t.Run(sh[len(sh)-1], func(t *testing.T) {
			mount := t.TempDir()
			outside := t.TempDir()
			tree(t, outside, "precious.txt", "inner/kept.txt", "inner/other.txt")
			tree(t, mount, "world/level.dat")
			for name, to := range map[string]string{"out": outside, "via": outside, "dangling": filepath.Join(mount, "missing")} {
				if err := os.Symlink(to, filepath.Join(mount, name)); err != nil {
					t.Fatal(err)
				}
			}
			if out, err := runClean(t, sh, mount, "2", "world\nvia/inner/kept.txt", "true"); err != nil {
				t.Fatalf("%v\n%s", err, out)
			}
			for _, f := range []string{"precious.txt", "inner/kept.txt", "inner/other.txt"} {
				if !present(filepath.Join(outside, f)) {
					t.Errorf("%s, outside the volume, is gone", f)
				}
			}
			if present(filepath.Join(mount, "out")) || present(filepath.Join(mount, "dangling")) {
				t.Error("a link nothing kept survived")
			}
			if !present(filepath.Join(mount, "via")) {
				t.Error("the link to a kept path went")
			}
		})
	}
}

// An install that fails after the wipe runs again on the next start, and the
// wipe with it: what is kept is still there, and still kept.
func TestCleanReinstallSurvivesARetry(t *testing.T) {
	for _, sh := range cleanShells() {
		t.Run(sh[len(sh)-1], func(t *testing.T) {
			mount := t.TempDir()
			tree(t, mount, "world/level.dat", "mods/old.jar")
			if _, err := runClean(t, sh, mount, "2", "world", "exit 3"); err == nil {
				t.Fatal("a failing install was reported as a success")
			}
			if present(filepath.Join(mount, ".quetzal-installed")) {
				t.Fatal("a failed install was marked installed")
			}
			tree(t, mount, "half-downloaded.zip")
			if out, err := runClean(t, sh, mount, "2", "world", "true"); err != nil {
				t.Fatalf("retry: %v\n%s", err, out)
			}
			if !present(filepath.Join(mount, "world/level.dat")) {
				t.Error("the world went on the retry")
			}
			if present(filepath.Join(mount, "half-downloaded.zip")) || present(filepath.Join(mount, "mods")) {
				t.Error("the retry did not clear what the failed attempt left")
			}
		})
	}
}

// Nothing to keep found: everything goes, and the log says so.
func TestCleanReinstallWithNothingFound(t *testing.T) {
	mount := t.TempDir()
	tree(t, mount, "World/level.dat", "mods/x.jar")
	out, err := runClean(t, []string{"sh"}, mount, "2", "world", "true")
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if present(filepath.Join(mount, "World")) || present(filepath.Join(mount, "mods")) {
		t.Error("files survived a wipe that kept nothing")
	}
	if !strings.Contains(out, "none of the paths to keep exists") {
		t.Errorf("install log does not say nothing was kept:\n%s", out)
	}
}

// The list of paths to keep reaches the install step only while a clean
// reinstall is pending. Every other server's pod is rendered exactly as before
// clean reinstalls existed, so the release that brings them restarts none.
func TestCleanReinstallRendersOnlyWhenPending(t *testing.T) {
	tmpl := &models.Template{
		Slug: "t", DataPath: "/data", Startup: "run",
		Images:  []models.TemplateImage{{Ref: "img", Default: true}},
		Console: models.ConsoleConfig{Type: models.ConsoleAttach},
		Install: &models.InstallScript{Image: "alpine:3.24", Script: "echo hi"},
	}
	install := func(s *models.Server) corev1.Container {
		t.Helper()
		for _, c := range BuildDeployment(s, tmpl, "panel:1", nil).Spec.Template.Spec.InitContainers {
			if c.Name == InstallContainer {
				return c
			}
		}
		t.Fatal("no install container")
		return corev1.Container{}
	}
	has := func(env []corev1.EnvVar, name string) bool {
		for _, e := range env {
			if e.Name == name {
				return true
			}
		}
		return false
	}
	base := func() *models.Server {
		return &models.Server{Slug: "s", Namespace: "ns", Image: "img", InstallGeneration: 2,
			Storage: models.Storage{Type: models.StoragePVC, Size: "1Gi"}}
	}

	for name, s := range map[string]*models.Server{
		"no wipe":               base(),
		"a full wipe":           func() *models.Server { s := base(); s.InstallWipe = true; return s }(),
		"a list, but no wipe":   func() *models.Server { s := base(); s.InstallKeep = []string{"world"}; return s }(),
		"a list kept for later": func() *models.Server { s := base(); s.ReinstallKeep = []string{"world"}; return s }(),
	} {
		c := install(s)
		if has(c.Env, "QUETZAL_INSTALL_KEEP") {
			t.Errorf("%s: the install step was given a list to keep", name)
		}
		if !strings.HasPrefix(envValue(c.Env, "QUETZAL_INSTALL_SCRIPT"), buildInstallScript(installMountPath)) {
			t.Errorf("%s: the install step does not run the usual guard", name)
		}
	}

	s := base()
	s.InstallWipe, s.InstallKeep = true, []string{"world*", "My World", "server.properties"}
	c := install(s)
	if got := envValue(c.Env, "QUETZAL_INSTALL_KEEP"); got != "world*\nMy World\nserver.properties\n.quetzalignore" {
		t.Errorf("QUETZAL_INSTALL_KEEP = %q", got)
	}
	if !strings.HasPrefix(envValue(c.Env, "QUETZAL_INSTALL_SCRIPT"), buildCleanInstallScript(installMountPath)) {
		t.Error("a clean reinstall does not run the guard that keeps")
	}
}

// A clean reinstall deleted .quetzalignore, because it is not a path anyone
// thinks to list: the server's backups then quietly held everything the list
// had been leaving out, and nothing said so until a restore came up short.
func TestCleanReinstallSparesTheIgnoreFile(t *testing.T) {
	tmpl := &models.Template{
		Slug: "t", DataPath: "/data", Startup: "run",
		Images:  []models.TemplateImage{{Ref: "img", Default: true}},
		Console: models.ConsoleConfig{Type: models.ConsoleAttach},
		Install: &models.InstallScript{Image: "alpine:3.24", Script: "echo hi"},
	}
	keepFor := func(keep []string) string {
		t.Helper()
		s := &models.Server{Slug: "s", Namespace: "ns", Image: "img", InstallGeneration: 2,
			Storage:     models.Storage{Type: models.StoragePVC, Size: "1Gi"},
			InstallWipe: true, InstallKeep: keep}
		for _, c := range BuildDeployment(s, tmpl, "panel:1", nil).Spec.Template.Spec.InitContainers {
			if c.Name == InstallContainer {
				return envValue(c.Env, "QUETZAL_INSTALL_KEEP")
			}
		}
		t.Fatal("no install container")
		return ""
	}

	if got, want := keepFor([]string{"world"}), "world\n"+models.IgnoreFile; got != want {
		t.Errorf("QUETZAL_INSTALL_KEEP = %q, want %q", got, want)
	}
	// Listed by hand as well: it is spared once, not twice.
	if got, want := keepFor([]string{"world", models.IgnoreFile}), "world\n"+models.IgnoreFile; got != want {
		t.Errorf("listed by hand: QUETZAL_INSTALL_KEEP = %q, want %q", got, want)
	}
	// The server's own list is left as it was; the file is added for the wipe only.
	s := &models.Server{Slug: "s", Namespace: "ns", Image: "img", InstallGeneration: 2,
		Storage:     models.Storage{Type: models.StoragePVC, Size: "1Gi"},
		InstallWipe: true, InstallKeep: []string{"world"}}
	BuildDeployment(s, tmpl, "panel:1", nil)
	if len(s.InstallKeep) != 1 || s.InstallKeep[0] != "world" {
		t.Errorf("the server's list was modified: %q", s.InstallKeep)
	}
}
