package reconciler

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// execInstall runs the wrapped install script against a real temp dir as the
// "mount" with the given generation/wipe env.
func execInstall(t *testing.T, mount, gen, wipe string) {
	t.Helper()
	userScript := `echo x >> "` + mount + `/ran.log"`
	script := buildInstallScript(mount, userScript)
	cmd := exec.Command("sh", "-c", script)
	cmd.Env = append(os.Environ(), "QUETZAL_INSTALL_GEN="+gen, "QUETZAL_INSTALL_WIPE="+wipe)
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
