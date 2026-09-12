package reconciler

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// runEgg runs the install guard with `egg` as the egg's script, against mount as
// the data volume, and returns the guard's exit code.
func runEgg(t *testing.T, mount, egg, gen string) int {
	t.Helper()
	cmd := exec.Command("sh", "-c", buildInstallScript(mount))
	cmd.Env = append(os.Environ(),
		"QUETZAL_INSTALL_GEN="+gen,
		"QUETZAL_INSTALL_WIPE=0",
		"QUETZAL_INSTALL_RESOLVED_SHELL=sh",
		"QUETZAL_INSTALL_USER_SCRIPT="+egg,
	)
	out, err := cmd.CombinedOutput()
	code := 0
	if ee, ok := err.(*exec.ExitError); ok {
		code = ee.ExitCode()
	} else if err != nil {
		t.Fatalf("running the guard: %v\n%s", err, out)
	}
	t.Logf("exit %d, output:\n%s", code, out)
	return code
}

func marked(mount string) bool {
	_, err := os.Stat(filepath.Join(mount, ".quetzal-installed"))
	return err == nil
}

// An egg whose script ends in `exit 0` -- and plenty do -- must still be
// recorded as installed. Inlined into the guard, that exit ended the guard too:
// the marker was never written, so the install re-ran on every single start
// (re-downloading everything), and the ownership handover after it never ran, so
// the data stayed root-owned.
func TestEggExitZeroStillRecordsTheInstall(t *testing.T) {
	mount := t.TempDir()
	if code := runEgg(t, mount, "echo installing; exit 0", "1"); code != 0 {
		t.Fatalf("guard exited %d, want 0", code)
	}
	if !marked(mount) {
		t.Fatal("the install was not recorded: it will re-run on every start")
	}
	// And the recorded generation has to be the one asked for, or the next start
	// treats it as a mismatch and installs again anyway.
	b, err := os.ReadFile(filepath.Join(mount, ".quetzal-installed"))
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != "1" {
		t.Errorf("marker holds %q, want \"1\"", b)
	}
	// Second start at the same generation must now skip.
	mount2 := mount
	if code := runEgg(t, mount2, "echo installing >> "+filepath.Join(mount, "ran.log")+"; exit 0", "1"); code != 0 {
		t.Fatalf("skip path exited %d", code)
	}
	if _, err := os.Stat(filepath.Join(mount, "ran.log")); err == nil {
		t.Error("the egg ran again despite the marker")
	}
}

// A bare `exit` is the same trap wearing a disguise: it yields the status of the
// previous command, so an egg that means to abort ("error downloading" then
// `exit`) usually exits 0, because echo succeeded. Whatever it resolves to, the
// guard must be the thing that decides, not the egg's control flow.
func TestEggBareExitStillRecordsTheInstall(t *testing.T) {
	mount := t.TempDir()
	if code := runEgg(t, mount, "echo done\nexit", "3"); code != 0 {
		t.Fatalf("guard exited %d, want 0", code)
	}
	if !marked(mount) {
		t.Fatal("the install was not recorded")
	}
}

// A failing egg must still be refused the marker, and must now get the
// explanation it used to jump over: an `exit 1` on an error branch ended the
// guard before it could say anything.
func TestEggNonZeroExitIsRefusedAndExplained(t *testing.T) {
	mount := t.TempDir()
	cmd := exec.Command("sh", "-c", buildInstallScript(mount))
	cmd.Env = append(os.Environ(),
		"QUETZAL_INSTALL_GEN=1", "QUETZAL_INSTALL_WIPE=0",
		"QUETZAL_INSTALL_RESOLVED_SHELL=sh",
		"QUETZAL_INSTALL_USER_SCRIPT=echo cannot reach the mirror >&2\nexit 7",
	)
	out, err := cmd.CombinedOutput()
	ee, ok := err.(*exec.ExitError)
	if !ok {
		t.Fatalf("a failing egg exited 0; output:\n%s", out)
	}
	if ee.ExitCode() != 7 {
		t.Errorf("exit %d, want 7 (the egg's own status)", ee.ExitCode())
	}
	if marked(mount) {
		t.Error("a failed install was recorded as installed")
	}
	if !contains(string(out), "will run again on the next start") {
		t.Errorf("the failure was not explained; output:\n%s", out)
	}
	if !contains(string(out), "cannot reach the mirror") {
		t.Errorf("the egg's own message was lost; output:\n%s", out)
	}
}

// The egg gets the interpreter the picker resolved, not a blind sh: an egg
// written for bash would otherwise fail on syntax the picker had worked around.
func TestEggRunsUnderTheResolvedShell(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("no bash on this machine")
	}
	mount := t.TempDir()
	cmd := exec.Command("sh", "-c", buildInstallScript(mount))
	cmd.Env = append(os.Environ(),
		"QUETZAL_INSTALL_GEN=1", "QUETZAL_INSTALL_WIPE=0",
		"QUETZAL_INSTALL_RESOLVED_SHELL=bash",
		// [[ ]] is bash-only; dash rejects it outright.
		`QUETZAL_INSTALL_USER_SCRIPT=if [[ "a" == "a" ]]; then echo bashworks; fi`,
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("bash-only egg failed: %v\n%s", err, out)
	}
	if !contains(string(out), "bashworks") {
		t.Errorf("the egg did not run under bash; output:\n%s", out)
	}
	if !marked(mount) {
		t.Error("the install was not recorded")
	}
}

func contains(hay, needle string) bool {
	return len(hay) >= len(needle) && (func() bool {
		for i := 0; i+len(needle) <= len(hay); i++ {
			if hay[i:i+len(needle)] == needle {
				return true
			}
		}
		return false
	})()
}
