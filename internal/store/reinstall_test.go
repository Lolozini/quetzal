package store

import (
	"reflect"
	"testing"

	"github.com/lolozini/quetzal/internal/models"
)

// A clean reinstall stores what its wipe spares, remembers it for the next
// one, and both go: the spared list with the wipe, the remembered one never.
func TestCleanReinstallKeepLifecycle(t *testing.T) {
	s := newTestStore(t)
	srv := &models.Server{Slug: "atm", Namespace: "ns", InstallGeneration: 1}
	if err := s.CreateServer(srv); err != nil {
		t.Fatal(err)
	}
	keep := []string{"world*", "server.properties"}
	reinstall := func(r ServerReinstall) *models.Server {
		t.Helper()
		r.TemplateID, r.Image = 1, "img"
		if err := s.ReinstallServer(srv.ID, r); err != nil {
			t.Fatal(err)
		}
		got, err := s.GetServer(srv.ID)
		if err != nil {
			t.Fatal(err)
		}
		return got
	}

	got := reinstall(ServerReinstall{Install: true, Wipe: true, Keep: keep})
	if !got.InstallWipe || !reflect.DeepEqual(got.InstallKeep, keep) {
		t.Fatalf("pending wipe = %v keeping %q", got.InstallWipe, got.InstallKeep)
	}
	if !reflect.DeepEqual(got.ReinstallKeep, keep) {
		t.Errorf("remembered = %q, want %q", got.ReinstallKeep, keep)
	}
	if got.InstallGeneration != 2 {
		t.Errorf("generation = %d, want 2", got.InstallGeneration)
	}

	// The reinstall ran: the wipe and its list are retired, the memory stays.
	if err := s.ClearInstallWipe(srv.ID); err != nil {
		t.Fatal(err)
	}
	got, _ = s.GetServer(srv.ID)
	if got.InstallWipe || len(got.InstallKeep) != 0 {
		t.Errorf("after the reinstall: wipe %v keeping %q", got.InstallWipe, got.InstallKeep)
	}
	if !reflect.DeepEqual(got.ReinstallKeep, keep) {
		t.Errorf("the remembered list went with the wipe: %q", got.ReinstallKeep)
	}

	// A full wipe keeps nothing, and leaves the memory for the next clean one.
	got = reinstall(ServerReinstall{Install: true, Wipe: true})
	if !got.InstallWipe || len(got.InstallKeep) != 0 {
		t.Errorf("full wipe: wipe %v keeping %q", got.InstallWipe, got.InstallKeep)
	}
	if !reflect.DeepEqual(got.ReinstallKeep, keep) {
		t.Errorf("a full wipe changed the remembered list: %q", got.ReinstallKeep)
	}

	// A plain reinstall over a pending clean one replaces it: nothing is wiped.
	reinstall(ServerReinstall{Install: true, Wipe: true, Keep: keep})
	got = reinstall(ServerReinstall{Install: true})
	if got.InstallWipe || len(got.InstallKeep) != 0 {
		t.Errorf("plain reinstall left wipe %v keeping %q", got.InstallWipe, got.InstallKeep)
	}

	// A template with nothing to install wipes nothing, whatever was asked.
	got = reinstall(ServerReinstall{Wipe: true, Keep: []string{"other"}})
	if got.InstallWipe || len(got.InstallKeep) != 0 || !reflect.DeepEqual(got.ReinstallKeep, keep) {
		t.Errorf("no install: wipe %v keeping %q remembering %q", got.InstallWipe, got.InstallKeep, got.ReinstallKeep)
	}
}
