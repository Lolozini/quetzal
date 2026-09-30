package api_test

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/lolozini/quetzal/internal/hibernate"
	"github.com/lolozini/quetzal/internal/models"
	"github.com/lolozini/quetzal/internal/reconciler"
)

// A server nobody had joined for a day went to sleep five seconds after
// hibernation was turned on: the idle timer ran from its last activity, not
// from the moment it was asked to sleep when idle. The countdown starts when
// hibernation is turned on, and again when its policy changes.
func TestTurningHibernationOnStartsTheCountdown(t *testing.T) {
	ts, c, st := newTestServerStore(t)
	setupAdmin(t, ts.URL, c)
	dayAgo := time.Now().Add(-24 * time.Hour)
	srv := &models.Server{
		Slug: "idle-a1b2", DisplayName: "idle", Namespace: reconciler.NamespaceFor("idle-a1b2"),
		DesiredState: models.StateRunning, LastActiveAt: &dayAgo,
		Ports: []models.PortSpec{{Name: "game", Port: 25565, Protocol: "TCP", Primary: true}},
	}
	if err := st.CreateServer(srv); err != nil {
		t.Fatal(err)
	}
	patch := func(body string) {
		t.Helper()
		req, _ := http.NewRequest(http.MethodPatch, ts.URL+"/api/servers/"+itoa(srv.ID), strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		r, err := c.Do(req)
		if err != nil || r.StatusCode != http.StatusOK {
			t.Fatalf("patch %s = %v %v", body, err, r.StatusCode)
		}
	}
	// Nobody connected, as the probe tells it.
	m := hibernate.New(st, func(context.Context, *models.Server) (int, error) { return 0, nil })
	asleepAt := func(at time.Time) bool {
		t.Helper()
		m.Now = func() time.Time { return at }
		m.Tick(context.Background())
		got, err := st.GetServer(srv.ID)
		if err != nil {
			t.Fatal(err)
		}
		return got.Hibernated
	}

	patch(`{"hibernation":{"enabled":true,"idleMinutes":15}}`)
	if asleepAt(time.Now().Add(time.Minute)) {
		t.Fatal("asleep a minute after hibernation was turned on, with a 15-minute idle window")
	}
	if !asleepAt(time.Now().Add(16 * time.Minute)) {
		t.Fatal("still awake 16 minutes after hibernation was turned on")
	}

	// A new idle window counts from when it is set too.
	if err := st.Wake(srv.ID, dayAgo); err != nil {
		t.Fatal(err)
	}
	patch(`{"hibernation":{"enabled":true,"idleMinutes":30}}`)
	if asleepAt(time.Now().Add(time.Minute)) {
		t.Error("asleep a minute after the idle window changed")
	}
}
