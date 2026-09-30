package dbprovision

import (
	"context"
	"time"

	"github.com/lolozini/quetzal/internal/models"
)

// HostStore is what the prober reads and writes.
type HostStore interface {
	ListDatabaseHosts() ([]models.DatabaseHost, error)
	DatabaseHostAdminPassword(h *models.DatabaseHost) (string, error)
	SetDatabaseHostStatus(id uint, reachable bool, msg string) error
}

// Prober keeps each database host's reachability current. It was set only
// when an administrator pressed "test": a managed host whose MariaDB had been
// ready for minutes read unreachable and never checked, and an external host
// that went down read reachable until somebody looked.
type Prober struct {
	Store HostStore
	// Ping is Ping, replaced in tests.
	Ping func(ctx context.Context, c Conn) error
	Now  func() time.Time
	// Recheck is how long a reachable host goes before it is checked again. A
	// host that is not reachable, or was never checked, is tried on every pass:
	// a managed one is starting, and the panel should say so once it is up.
	Recheck time.Duration
	// Timeout bounds one host's check.
	Timeout time.Duration
}

// NewProber returns a Prober with the real Ping.
func NewProber(st HostStore) *Prober {
	return &Prober{Store: st, Ping: Ping, Now: time.Now, Recheck: 5 * time.Minute, Timeout: 5 * time.Second}
}

// Run checks the hosts every interval until ctx ends.
func (p *Prober) Run(ctx context.Context, interval time.Duration) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		p.Tick(ctx)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// Tick checks the hosts that are due.
func (p *Prober) Tick(ctx context.Context) {
	hosts, err := p.Store.ListDatabaseHosts()
	if err != nil {
		return
	}
	now := p.Now()
	for i := range hosts {
		h := &hosts[i]
		if h.Reachable && h.LastCheckedAt != nil && now.Sub(*h.LastCheckedAt) < p.Recheck {
			continue
		}
		p.Check(ctx, h)
	}
}

// Check pings one host with its admin credentials and records the outcome.
func (p *Prober) Check(ctx context.Context, h *models.DatabaseHost) {
	pw, err := p.Store.DatabaseHostAdminPassword(h)
	if err != nil {
		return
	}
	host, port := h.AdminAddr()
	cctx, cancel := context.WithTimeout(ctx, p.Timeout)
	defer cancel()
	msg := ""
	if err := p.Ping(cctx, Conn{Host: host, Port: port, User: h.AdminUser, Password: pw}); err != nil {
		if ctx.Err() != nil {
			return // shutting down: that says nothing about the host
		}
		msg = err.Error()
	}
	_ = p.Store.SetDatabaseHostStatus(h.ID, msg == "", msg)
}
