package reconciler

import (
	"testing"

	"github.com/lolozini/quetzal/internal/models"
)

// A server put to sleep by hibernation is scaled to zero like a stopped one, and
// gets its stop command the same way. It used to be skipped because it is still
// desired Running, so the game only saw SIGTERM.
func TestGracefulStopCoversHibernation(t *testing.T) {
	tmpl := &models.Template{StopCommand: "stop"}
	cases := []struct {
		name string
		srv  models.Server
		want bool
	}{
		{"running", models.Server{DesiredState: models.StateRunning}, false},
		{"stopped", models.Server{DesiredState: models.StateStopped}, true},
		{"suspended", models.Server{DesiredState: models.StateSuspended}, true},
		{"hibernated", models.Server{DesiredState: models.StateRunning, Hibernated: true}, true},
	}
	for _, c := range cases {
		if got := needsGracefulStop(&c.srv, tmpl); got != c.want {
			t.Errorf("%s: needsGracefulStop = %v, want %v", c.name, got, c.want)
		}
	}
	// A signal-style stop (^C) is left to the pod's SIGTERM.
	hib := models.Server{DesiredState: models.StateRunning, Hibernated: true}
	if needsGracefulStop(&hib, &models.Template{StopCommand: "^C"}) {
		t.Error("a caret stop was sent as console input")
	}
}

// An activator fronts a server that is meant to run. Stopping or suspending a
// sleeping server leaves Hibernated set, and the activator used to stay up in
// front of it, accepting connections and calling wake.
func TestActivatorOnlyFrontsRunningServers(t *testing.T) {
	r := &Reconciler{ActivatorImage: "img", WakeURL: "http://wake"}
	tmpl := &models.Template{}
	ports := []models.PortSpec{{Name: "p25565", Port: 25565, Protocol: "TCP"}}
	for _, state := range []models.DesiredState{models.StateRunning, models.StateStopped, models.StateSuspended} {
		drop := &models.Server{DesiredState: state, Hibernated: true, Ports: ports,
			Hibernation: models.Hibernation{Enabled: true, WakeOnConnect: true}}
		proxy := &models.Server{DesiredState: state, Ports: ports,
			Hibernation: models.Hibernation{Enabled: true, Proxy: true}}
		want := state == models.StateRunning
		if got := r.dropActive(drop, tmpl); got != want {
			t.Errorf("drop activator for a %s server = %v, want %v", state, got, want)
		}
		if got := r.proxyActive(proxy, tmpl); got != want {
			t.Errorf("proxy activator for a %s server = %v, want %v", state, got, want)
		}
	}
}

// A reinstall's wipe is retired once its generation has come up and the server
// is down, and not before.
func TestWipeConsumed(t *testing.T) {
	base := func() models.Server {
		return models.Server{InstallGeneration: 3, InstallWipe: true, DesiredState: models.StateStopped,
			Status: models.Status{InstalledGeneration: 3}}
	}
	s := base()
	if !wipeConsumed(&s) {
		t.Error("a wipe whose reinstall ran was not retired")
	}
	s = base()
	s.Status.InstalledGeneration = 2 // the reinstall has not run yet
	if wipeConsumed(&s) {
		t.Error("a pending wipe was retired before its reinstall ran")
	}
	s = base()
	s.DesiredState = models.StateRunning // clearing it would roll the live pod
	if wipeConsumed(&s) {
		t.Error("the wipe was retired on a running server")
	}
	s = base()
	s.InstallWipe = false
	if wipeConsumed(&s) {
		t.Error("nothing to retire, yet retired")
	}
}
