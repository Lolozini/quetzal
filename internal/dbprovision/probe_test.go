package dbprovision

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/lolozini/quetzal/internal/models"
)

type fakeHosts struct {
	hosts  []models.DatabaseHost
	status map[uint]string // "ok" or the message
}

func (f *fakeHosts) ListDatabaseHosts() ([]models.DatabaseHost, error) { return f.hosts, nil }
func (f *fakeHosts) DatabaseHostAdminPassword(*models.DatabaseHost) (string, error) {
	return "pw", nil
}
func (f *fakeHosts) SetDatabaseHostStatus(id uint, reachable bool, msg string) error {
	if reachable {
		f.status[id] = "ok"
	} else {
		f.status[id] = msg
	}
	return nil
}

// A managed host whose MariaDB was ready read "unreachable, never checked"
// until an administrator pressed "test", and nothing noticed an external host
// going down. The prober checks a host that was never checked or is not
// reachable on every pass, and a reachable one again once Recheck has passed.
func TestHostsAreCheckedWithoutAnyonePressingTest(t *testing.T) {
	now := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	recent, stale := now.Add(-time.Minute), now.Add(-10*time.Minute)
	f := &fakeHosts{status: map[uint]string{}, hosts: []models.DatabaseHost{
		{ID: 1, Name: "managed, just up", Host: "db-1.quetzal-db-1.svc", Port: 3306},
		{ID: 2, Name: "checked a minute ago", Host: "10.0.0.2", Port: 3306, Reachable: true, LastCheckedAt: &recent},
		{ID: 3, Name: "checked ten minutes ago", Host: "10.0.0.3", Port: 3306, Reachable: true, LastCheckedAt: &stale},
		{ID: 4, Name: "down a minute ago", Host: "10.0.0.4", Port: 3306, LastCheckedAt: &recent},
	}}
	pinged := map[string]bool{}
	p := &Prober{
		Store: f, Now: func() time.Time { return now }, Recheck: 5 * time.Minute, Timeout: time.Second,
		Ping: func(_ context.Context, c Conn) error {
			pinged[c.Host] = true
			if c.Host == "10.0.0.3" {
				return errors.New("dial tcp 10.0.0.3:3306: connect: connection refused")
			}
			return nil
		},
	}
	p.Tick(context.Background())

	if f.status[1] != "ok" {
		t.Errorf("the managed host that came up: %q, want reachable", f.status[1])
	}
	if pinged["10.0.0.2"] {
		t.Error("a host found reachable a minute ago was checked again")
	}
	if f.status[3] == "ok" || f.status[3] == "" {
		t.Errorf("the host that went down: %q, want its error", f.status[3])
	}
	if f.status[4] != "ok" {
		t.Errorf("the host that was down a minute ago and is back: %q, want reachable", f.status[4])
	}
}
