// Package hibernate scales idle game servers to zero (no active player
// connections) and lets them be woken on demand. Idle detection is generic: it
// counts ESTABLISHED TCP connections on a server's game ports, read from the
// container's /proc/net/tcp. Probing is injected so the logic stays testable and
// fail-safe — if activity can't be measured, the server is treated as active and
// never hibernated.
package hibernate

import (
	"bufio"
	"context"
	"log"
	"strconv"
	"strings"
	"time"

	"github.com/lolozini/quetzal/internal/models"
	"github.com/lolozini/quetzal/internal/store"
)

// defaultIdleMinutes applies when a policy enables hibernation without a value.
const defaultIdleMinutes = 15

// ConnProbe returns the number of active (ESTABLISHED) connections on a server's
// game ports, or an error if it can't be measured.
type ConnProbe func(ctx context.Context, srv *models.Server) (int, error)

// Manager evaluates hibernation for all eligible servers each tick.
type Manager struct {
	Store *store.Store
	Probe ConnProbe
	Now   func() time.Time
}

// New returns a hibernation Manager.
func New(st *store.Store, probe ConnProbe) *Manager {
	return &Manager{Store: st, Probe: probe, Now: time.Now}
}

func (m *Manager) now() time.Time {
	if m.Now != nil {
		return m.Now()
	}
	return time.Now()
}

// Tick hibernates idle servers. Only Running, hibernation-enabled servers that
// expose ports and aren't already hibernated are considered.
func (m *Manager) Tick(ctx context.Context) {
	servers, err := m.Store.ListServers()
	if err != nil {
		return
	}
	now := m.now()
	for i := range servers {
		srv := &servers[i]
		if !eligible(srv) {
			continue
		}
		// Proxy-mode servers don't get probed: the in-path proxy reports activity
		// by bumping LastActiveAt (the only way to measure UDP), so we rely on
		// that timestamp's freshness. Other servers are probed for TCP
		// connections and have their timer bumped here when active.
		if !srv.Hibernation.Proxy {
			conns, err := m.Probe(ctx, srv)
			if err != nil {
				continue // fail-safe: can't measure -> assume active
			}
			if conns > 0 {
				_ = m.Store.UpdateLastActive(srv.ID, now)
				continue
			}
		}
		// Idle: start the timer on first observation, hibernate once it elapses.
		if srv.LastActiveAt == nil {
			_ = m.Store.UpdateLastActive(srv.ID, now)
			continue
		}
		if now.Sub(*srv.LastActiveAt) >= idleWindow(srv) {
			log.Printf("hibernate: scaling %s to zero (idle for %s)", srv.Slug, idleWindow(srv))
			_ = m.Store.SetHibernated(srv.ID, true)
		}
	}
}

func eligible(srv *models.Server) bool {
	if !srv.Hibernation.Enabled || srv.DesiredState != models.StateRunning || srv.Hibernated {
		return false
	}
	// Proxy mode measures activity (incl. UDP) via the in-path proxy, so any
	// ported server qualifies; otherwise idle is read from TCP state only.
	if srv.Hibernation.Proxy {
		return len(srv.Ports) > 0
	}
	return measurablePorts(srv)
}

// measurablePorts reports whether idle can be reliably measured from TCP
// connection state. UDP is connectionless: it has no ESTABLISHED entry in
// /proc/net/tcp, so a UDP game port would always look idle even with active
// players — auto-sleeping it would kick everyone. We therefore only consider a
// server eligible when it exposes at least one port and *every* port is TCP.
// Generic UDP idle detection (and wake-on-connect) is deferred to the shared
// proxy work; until then, UDP servers simply never auto-hibernate (fail-safe).
func measurablePorts(srv *models.Server) bool {
	if len(srv.Ports) == 0 {
		return false
	}
	for _, p := range srv.Ports {
		if strings.EqualFold(p.Protocol, "UDP") {
			return false
		}
	}
	return true
}

func idleWindow(srv *models.Server) time.Duration {
	m := srv.Hibernation.IdleMinutes
	if m <= 0 {
		m = defaultIdleMinutes
	}
	return time.Duration(m) * time.Minute
}

// CountEstablished parses /proc/net/tcp(+tcp6) content and counts ESTABLISHED
// (state 01) connections whose local port is one of `ports`. It is a pure
// function so the probe can be unit-tested without a cluster.
func CountEstablished(procNetTCP string, ports map[int32]bool) int {
	const stateEstablished = "01"
	n := 0
	sc := bufio.NewScanner(strings.NewReader(procNetTCP))
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		// Columns: sl local_address rem_address st ...
		if len(fields) < 4 || fields[0] == "sl" {
			continue
		}
		if fields[3] != stateEstablished {
			continue
		}
		local := fields[1] // e.g. "0100007F:63DD"
		colon := strings.LastIndex(local, ":")
		if colon < 0 {
			continue
		}
		port, err := strconv.ParseInt(local[colon+1:], 16, 32)
		if err != nil {
			continue
		}
		if ports[int32(port)] {
			n++
		}
	}
	return n
}
