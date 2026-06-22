package hibernate

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/lolozini/quetzal/internal/models"
	"github.com/lolozini/quetzal/internal/store"
)

func TestCountEstablished(t *testing.T) {
	// Port 25565 = 0x63DD. Two established (st 01) on it, one listen (0A), and
	// one established on another port — only the two should count.
	const proc = `  sl  local_address rem_address   st tx_queue rx_queue
   0: 0100007F:63DD 00000000:0000 0A 00000000:00000000
   1: 0100007F:63DD 0A00020F:C3A2 01 00000000:00000000
   2: 0100007F:63DD 0A00020F:C3A3 01 00000000:00000000
   3: 0100007F:1F90 0A00020F:C3A4 01 00000000:00000000
`
	if got := CountEstablished(proc, map[int32]bool{25565: true}); got != 2 {
		t.Errorf("established = %d, want 2", got)
	}
	if got := CountEstablished(proc, map[int32]bool{8080: true}); got != 1 {
		t.Errorf("established on 8080 = %d, want 1", got)
	}
}

func testStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open(store.Config{Driver: store.DriverSQLite, DSN: filepath.Join(t.TempDir(), "h.db"), Silent: true})
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	if err := st.Migrate(); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return st
}

func runningHibernatable() *models.Server {
	return &models.Server{
		Slug: "s", Namespace: "ns", DesiredState: models.StateRunning,
		Ports:       []models.PortSpec{{Name: "game", Port: 25565, Protocol: "TCP"}},
		Hibernation: models.Hibernation{Enabled: true, IdleMinutes: 1},
	}
}

func TestManagerHibernatesAfterIdleWindow(t *testing.T) {
	st := testStore(t)
	srv := runningHibernatable()
	_ = st.CreateServer(srv)

	now := time.Now()
	m := New(st, func(context.Context, *models.Server) (int, error) { return 0, nil })
	m.Now = func() time.Time { return now }

	// First tick: no prior activity -> arm the timer, don't hibernate yet.
	m.Tick(context.Background())
	if got, _ := st.GetServer(srv.ID); got.Hibernated {
		t.Fatal("should not hibernate on first idle observation")
	}
	// After the idle window elapses with zero connections -> hibernate.
	now = now.Add(2 * time.Minute)
	m.Tick(context.Background())
	if got, _ := st.GetServer(srv.ID); !got.Hibernated {
		t.Fatal("should have hibernated after idle window")
	}
}

func TestManagerKeepsActiveServerAwake(t *testing.T) {
	st := testStore(t)
	srv := runningHibernatable()
	_ = st.CreateServer(srv)
	now := time.Now()
	m := New(st, func(context.Context, *models.Server) (int, error) { return 3, nil }) // players online
	m.Now = func() time.Time { return now }

	m.Tick(context.Background())
	now = now.Add(time.Hour)
	m.Tick(context.Background())
	if got, _ := st.GetServer(srv.ID); got.Hibernated {
		t.Error("active server must not hibernate")
	}
}

func TestManagerProxyModeUsesHeartbeat(t *testing.T) {
	st := testStore(t)
	now := time.Now()

	// A UDP, proxy-mode server is eligible (proxy measures activity) and must NOT
	// be probed — its idle timer is driven by the activity heartbeat.
	stale := now.Add(-time.Hour)
	udp := &models.Server{
		Slug: "udp", Namespace: "nu", DesiredState: models.StateRunning,
		Ports:        []models.PortSpec{{Name: "game", Port: 2456, Protocol: "UDP"}},
		Hibernation:  models.Hibernation{Enabled: true, IdleMinutes: 1, Proxy: true},
		LastActiveAt: &stale,
	}
	_ = st.CreateServer(udp)

	// A second proxy server with a fresh heartbeat must stay awake.
	fresh := now.Add(-10 * time.Second)
	live := &models.Server{
		Slug: "udp-live", Namespace: "nl", DesiredState: models.StateRunning,
		Ports:        []models.PortSpec{{Name: "game", Port: 2456, Protocol: "UDP"}},
		Hibernation:  models.Hibernation{Enabled: true, IdleMinutes: 1, Proxy: true},
		LastActiveAt: &fresh,
	}
	_ = st.CreateServer(live)

	m := New(st, func(context.Context, *models.Server) (int, error) {
		t.Error("probe must not be called for a proxy-mode server")
		return 0, nil
	})
	m.Now = func() time.Time { return now }
	m.Tick(context.Background())

	if got, _ := st.GetServerBySlug("udp"); !got.Hibernated {
		t.Error("stale proxy UDP server should have hibernated")
	}
	if got, _ := st.GetServerBySlug("udp-live"); got.Hibernated {
		t.Error("proxy UDP server with a fresh heartbeat must stay awake")
	}
}

func TestManagerFailSafeAndIneligible(t *testing.T) {
	st := testStore(t)
	now := time.Now().Add(-time.Hour) // lastActive far in the past

	// Probe error -> never hibernate (can't measure).
	failing := runningHibernatable()
	failing.LastActiveAt = &now
	_ = st.CreateServer(failing)
	m := New(st, func(context.Context, *models.Server) (int, error) { return 0, context.DeadlineExceeded })
	m.Tick(context.Background())
	if got, _ := st.GetServer(failing.ID); got.Hibernated {
		t.Error("must not hibernate when activity can't be measured")
	}

	// Disabled / portless / stopped / UDP servers are ineligible. UDP is the
	// important one: its players are invisible to the TCP probe, so it must
	// never auto-sleep (it would kick everyone).
	udpPorts := []models.PortSpec{{Name: "game", Port: 2456, Protocol: "UDP"}}
	mixedPorts := []models.PortSpec{{Name: "tcp", Port: 25565, Protocol: "TCP"}, {Name: "query", Port: 2457, Protocol: "UDP"}}
	for _, srv := range []*models.Server{
		{Slug: "off", Namespace: "n1", DesiredState: models.StateRunning, Ports: failing.Ports, LastActiveAt: &now},
		{Slug: "noport", Namespace: "n2", DesiredState: models.StateRunning, Hibernation: models.Hibernation{Enabled: true}, LastActiveAt: &now},
		{Slug: "stopped", Namespace: "n3", DesiredState: models.StateStopped, Ports: failing.Ports, Hibernation: models.Hibernation{Enabled: true}, LastActiveAt: &now},
		{Slug: "udp", Namespace: "n4", DesiredState: models.StateRunning, Ports: udpPorts, Hibernation: models.Hibernation{Enabled: true}, LastActiveAt: &now},
		{Slug: "mixed", Namespace: "n5", DesiredState: models.StateRunning, Ports: mixedPorts, Hibernation: models.Hibernation{Enabled: true}, LastActiveAt: &now},
	} {
		_ = st.CreateServer(srv)
	}
	idle := New(st, func(context.Context, *models.Server) (int, error) { return 0, nil })
	idle.Tick(context.Background())
	for _, slug := range []string{"off", "noport", "stopped", "udp", "mixed"} {
		if got, _ := st.GetServerBySlug(slug); got.Hibernated {
			t.Errorf("%s should be ineligible for hibernation", slug)
		}
	}
}
