package reconciler

import (
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"

	"github.com/lolozini/quetzal/internal/models"
)

// An egg's install script runs as an init container. Nothing used to read
// InitContainerStatuses, so a failing install left the main container never
// started, nothing in CrashLoopBackOff, and the server reported "Starting"
// forever — no message, no event, while the reason sat in the init container's
// log. These cases pin what the observation must now yield.
func TestNoteInitReadsTheSetupContainers(t *testing.T) {
	terminated := func(name string, code int32, msg string) corev1.ContainerStatus {
		return corev1.ContainerStatus{
			Name: name,
			State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{
				ExitCode: code, Message: msg,
			}},
		}
	}
	backoff := func(name string, lastCode int32) corev1.ContainerStatus {
		return corev1.ContainerStatus{
			Name: name,
			State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{
				Reason: "CrashLoopBackOff",
			}},
			LastTerminationState: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{
				ExitCode: lastCode,
			}},
		}
	}

	t.Run("a failed install is a failure", func(t *testing.T) {
		var h podHealth
		noteInit(&h, []corev1.ContainerStatus{terminated("install", 7, "could not reach the download server")})
		if !h.installFailed {
			t.Fatal("an install container that exited 7 must read as failed")
		}
		if h.installStep != "install" || h.installExit != 7 {
			t.Errorf("step/exit = %q/%d, want install/7", h.installStep, h.installExit)
		}
		msg := installFailureMessage(h)
		for _, want := range []string{"install", "7", "could not reach the download server"} {
			if !strings.Contains(msg, want) {
				t.Errorf("message %q should name %q", msg, want)
			}
		}
	})

	t.Run("a retried install is still a failure", func(t *testing.T) {
		// Kubernetes retries an init container in place; between attempts the exit
		// code is only on the previous state, which is where it used to be missed.
		var h podHealth
		noteInit(&h, []corev1.ContainerStatus{backoff("install", 7)})
		if !h.installFailed || h.installExit != 7 {
			t.Errorf("backing-off install: failed=%v exit=%d, want true/7", h.installFailed, h.installExit)
		}
	})

	t.Run("a finished install is not a failure", func(t *testing.T) {
		// Every successful pod reports its init containers as terminated 0. Reading
		// that as a failure would mark every healthy server as broken.
		var h podHealth
		noteInit(&h, []corev1.ContainerStatus{
			terminated("install", 0, ""),
			terminated("render-copy", 0, ""),
			terminated("render-config", 0, ""),
		})
		if h.installFailed || h.installing {
			t.Errorf("completed setup: failed=%v installing=%v, want both false", h.installFailed, h.installing)
		}
	})

	t.Run("a running install is reported as installing", func(t *testing.T) {
		var h podHealth
		noteInit(&h, []corev1.ContainerStatus{{
			Name:  "install",
			State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}},
		}})
		if !h.installing || h.installFailed {
			t.Errorf("running install: installing=%v failed=%v", h.installing, h.installFailed)
		}
		if h.installStep != "install" {
			t.Errorf("step = %q, want install", h.installStep)
		}
	})

	t.Run("a failing render step is caught too", func(t *testing.T) {
		// config.files is rendered by its own init container and was equally silent.
		var h podHealth
		noteInit(&h, []corev1.ContainerStatus{
			terminated("install", 0, ""),
			terminated("render-config", 2, ""),
		})
		if !h.installFailed || h.installStep != "render-config" {
			t.Errorf("render failure: failed=%v step=%q", h.installFailed, h.installStep)
		}
	})
}

// An egg that names an interpreter its own install image does not carry used to
// fail before any process ran: runc answered "exec: \"ash\": executable file not
// found in $PATH" with exit 128, which mentions neither the egg nor the install
// and leaves an empty log. The container now runs a shell that exists and execs
// the requested one, so a mismatch is a line in the log rather than a dead pod.
func TestInstallRunsUnderAShellThatExists(t *testing.T) {
	s, tmpl := testServerAndTemplate()
	tmpl.Install = &models.InstallScript{
		Image:      "eclipse-temurin:8-jdk-jammy", // Debian-based: no ash
		Entrypoint: "ash",
		Script:     "apk add curl\n",
	}
	cs := installInitContainers(s, tmpl, nil)
	if len(cs) != 1 {
		t.Fatalf("want one install container, got %d", len(cs))
	}
	c := cs[0]

	// The command must not name the egg's interpreter: that is the part that
	// fails before the container starts.
	if len(c.Command) < 1 || c.Command[0] != "/bin/sh" {
		t.Errorf("command = %v, want it to start with /bin/sh", c.Command)
	}
	for _, arg := range c.Command {
		if arg == "ash" {
			t.Error("the egg's interpreter is still in Command, so a missing one still kills the container")
		}
	}

	env := map[string]string{}
	for _, e := range c.Env {
		env[e.Name] = e.Value
	}
	if env["QUETZAL_INSTALL_SHELL"] != "ash" {
		t.Errorf("QUETZAL_INSTALL_SHELL = %q, want ash", env["QUETZAL_INSTALL_SHELL"])
	}
	if !strings.Contains(env["QUETZAL_INSTALL_SCRIPT"], "apk add curl") {
		t.Error("the egg's script did not reach the container")
	}
	// The picker has to try the named shell first, then fall back, and say so.
	for _, want := range []string{`"$QUETZAL_INSTALL_SHELL" bash ash sh`, "exec", "instead"} {
		if !strings.Contains(installShellPicker, want) {
			t.Errorf("the picker is missing %q", want)
		}
	}
}

// An egg that names nothing still gets a shell.
func TestInstallDefaultsToSh(t *testing.T) {
	s, tmpl := testServerAndTemplate()
	tmpl.Install = &models.InstallScript{Script: "echo hi\n"}
	cs := installInitContainers(s, tmpl, nil)
	if len(cs) != 1 {
		t.Fatalf("want one install container, got %d", len(cs))
	}
	env := map[string]string{}
	for _, e := range cs[0].Env {
		env[e.Name] = e.Value
	}
	if env["QUETZAL_INSTALL_SHELL"] != "sh" {
		t.Errorf("default shell = %q, want sh", env["QUETZAL_INSTALL_SHELL"])
	}
	if cs[0].Image != "alpine:3.20" {
		t.Errorf("default install image = %q", cs[0].Image)
	}
}
