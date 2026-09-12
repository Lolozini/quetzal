package reconciler

import (
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/lolozini/quetzal/internal/models"
)

// runPicker executes installShellPicker the way the init container does, and
// returns its exit code and combined output.
func runPicker(t *testing.T, script, timeout string) (int, string) {
	t.Helper()
	cmd := exec.Command("/bin/sh", "-c", installShellPicker)
	cmd.Env = append(cmd.Environ(),
		"QUETZAL_INSTALL_SHELL=sh",
		"QUETZAL_INSTALL_SCRIPT="+script,
		"QUETZAL_INSTALL_TIMEOUT="+timeout,
	)
	// Output goes to a real file, not a buffer: with a buffer, os/exec pipes the
	// output and Wait blocks until every process holding the write end is gone --
	// which is exactly what a killed-but-orphaned `sleep` does not do. A file is
	// passed as a plain fd, so Wait returns when the shell itself returns, the
	// way the kubelet observes the container's PID 1.
	f, err := os.CreateTemp(t.TempDir(), "picker")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	cmd.Stdout = f
	cmd.Stderr = f
	err = cmd.Run()
	code := 0
	if ee, ok := err.(*exec.ExitError); ok {
		code = ee.ExitCode()
	} else if err != nil {
		t.Fatalf("running the picker: %v", err)
	}
	out, rerr := os.ReadFile(f.Name())
	if rerr != nil {
		t.Fatal(rerr)
	}
	return code, string(out)
}

// An install that never finishes has to end somewhere. An init container has no
// deadline of its own, so before the watchdog a hung script left the server in
// Installing for good, indistinguishable from one that was merely slow.
func TestInstallWatchdogStopsAHangingScript(t *testing.T) {
	done := make(chan struct{})
	var code int
	var out string
	go func() {
		defer close(done)
		code, out = runPicker(t, "echo starting; sleep 60; echo never", "1")
	}()
	select {
	case <-done:
	case <-time.After(20 * time.Second):
		t.Fatal("the picker did not return: the watchdog never fired")
	}

	if code == 0 {
		t.Fatalf("a script that was killed reported success (exit 0); output:\n%s", out)
	}
	if !strings.Contains(out, "still running after") {
		t.Errorf("nothing in the log says why it stopped; output:\n%s", out)
	}
	// The reason it matters that this is non-zero: the marker is only written on
	// a zero status, so a timed-out install must re-run rather than be recorded
	// as done.
	if !strings.Contains(out, "will run again on the next start") {
		t.Errorf("the log does not say the install will be retried; output:\n%s", out)
	}
	if !strings.Contains(out, "starting") {
		t.Errorf("output written before the kill was lost; output:\n%s", out)
	}
}

// The watchdog must not change what a script that finishes on its own reports --
// neither a success nor the exit code of a failure, which is what tells the
// reconciler to put the server in Error.
func TestInstallWatchdogPreservesExitStatus(t *testing.T) {
	for _, tc := range []struct {
		name, script string
		want         int
	}{
		{"success", "echo ok; exit 0", 0},
		{"failure", "echo nope >&2; exit 7", 7},
	} {
		t.Run(tc.name, func(t *testing.T) {
			code, out := runPicker(t, tc.script, "30")
			if code != tc.want {
				t.Fatalf("exit %d, want %d; output:\n%s", code, tc.want, out)
			}
			if strings.Contains(out, "still running after") {
				t.Errorf("the watchdog reported a timeout that did not happen:\n%s", out)
			}
		})
	}
}

// A script that finishes must not be made to wait for the watchdog's sleep.
func TestInstallWatchdogDoesNotDelayAQuickScript(t *testing.T) {
	start := time.Now()
	if code, out := runPicker(t, "exit 0", "600"); code != 0 {
		t.Fatalf("exit %d; output:\n%s", code, out)
	}
	if d := time.Since(start); d > 10*time.Second {
		t.Fatalf("a script that exits immediately took %s", d)
	}
}

// The deadline reaches the container as environment; without it the watchdog's
// sleep gets an empty argument and the guarantee is silently gone.
func TestInstallContainerCarriesTheTimeout(t *testing.T) {
	srv := &models.Server{Slug: "s1", Namespace: "ns"}
	tpl := &models.Template{
		Name:    "t",
		Install: &models.InstallScript{Image: "alpine:3.20", Script: "echo hi"},
	}
	cs := installInitContainers(srv, tpl, nil)
	if len(cs) != 1 {
		t.Fatalf("got %d install containers, want 1", len(cs))
	}
	var got string
	for _, e := range cs[0].Env {
		if e.Name == "QUETZAL_INSTALL_TIMEOUT" {
			got = e.Value
		}
	}
	if got == "" {
		t.Fatal("QUETZAL_INSTALL_TIMEOUT is not set on the install container")
	}
}
