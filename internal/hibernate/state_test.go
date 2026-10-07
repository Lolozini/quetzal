package hibernate

import (
	"context"
	"testing"
	"time"

	"github.com/lolozini/quetzal/internal/models"
)

func TestIdleProbeCannotOverwriteConcurrentActivityOrPolicy(t *testing.T) {
	for _, change := range []string{"wake", "start", "disable", "extend-idle", "activity", "stop"} {
		t.Run(change, func(t *testing.T) {
			st := testStore(t)
			srv := runningHibernatable()
			now := time.Now().UTC().Truncate(time.Microsecond)
			stale := now.Add(-2 * time.Minute)
			srv.LastActiveAt = &stale
			if err := st.CreateServer(srv); err != nil {
				t.Fatal(err)
			}
			m := New(st, func(context.Context, *models.Server) (int, error) {
				var err error
				switch change {
				case "wake":
					err = st.Wake(srv.ID, now)
				case "start":
					err = st.StartServer(srv.ID, now)
				case "disable":
					err = st.UpdateServerHibernation(srv.ID, models.Hibernation{}, nil)
				case "extend-idle":
					err = st.UpdateServerHibernation(srv.ID, models.Hibernation{Enabled: true, IdleMinutes: 60}, nil)
				case "activity":
					err = st.UpdateLastActive(srv.ID, now)
				case "stop":
					err = st.SetDesiredState(srv.ID, models.StateStopped)
				}
				if err != nil {
					t.Fatalf("concurrent %s: %v", change, err)
				}
				return 0, nil
			})
			m.Now = func() time.Time { return now }
			m.Tick(context.Background())
			got, err := st.GetServer(srv.ID)
			if err != nil {
				t.Fatal(err)
			}
			if got.Hibernated {
				t.Fatalf("stale probe overwrote concurrent %s with hibernation", change)
			}
		})
	}
}

func TestActivityWritersNeverMoveTimerBackwards(t *testing.T) {
	for _, change := range []string{"probe", "wake", "start", "policy"} {
		t.Run(change, func(t *testing.T) {
			st := testStore(t)
			srv := runningHibernatable()
			now := time.Now().UTC().Truncate(time.Microsecond)
			newer := now.Add(time.Minute)
			srv.LastActiveAt = &newer
			if err := st.CreateServer(srv); err != nil {
				t.Fatal(err)
			}
			var err error
			switch change {
			case "probe":
				err = st.UpdateLastActive(srv.ID, now)
			case "wake":
				err = st.Wake(srv.ID, now)
			case "start":
				err = st.StartServer(srv.ID, now)
			case "policy":
				err = st.UpdateServerHibernation(srv.ID, srv.Hibernation, &now)
			}
			if err != nil {
				t.Fatal(err)
			}
			got, err := st.GetServer(srv.ID)
			if err != nil || got.LastActiveAt == nil || !got.LastActiveAt.Equal(newer) {
				t.Fatalf("%s moved activity backwards: %+v, %v", change, got, err)
			}
		})
	}
}

func TestProxyHeartbeatAfterSnapshotPreventsHibernation(t *testing.T) {
	st := testStore(t)
	now := time.Now().UTC().Truncate(time.Microsecond)
	stale := now.Add(-2 * time.Minute)
	tcp := runningHibernatable()
	tcp.CreatedAt = now.Add(-time.Hour)
	tcp.LastActiveAt = &stale
	if err := st.CreateServer(tcp); err != nil {
		t.Fatal(err)
	}
	proxy := runningHibernatable()
	proxy.CreatedAt = now
	proxy.Slug, proxy.Namespace = "proxy", "proxy-ns"
	proxy.Hibernation.Proxy = true
	proxy.LastActiveAt = &stale
	if err := st.CreateServer(proxy); err != nil {
		t.Fatal(err)
	}
	m := New(st, func(context.Context, *models.Server) (int, error) {
		if err := st.UpdateLastActive(proxy.ID, now); err != nil {
			t.Fatal(err)
		}
		return 1, nil
	})
	m.Now = func() time.Time { return now }
	m.Tick(context.Background())
	got, err := st.GetServer(proxy.ID)
	if err != nil || got.Hibernated {
		t.Fatalf("snapshot overwrote the proxy heartbeat: %+v, %v", got, err)
	}
}

func TestDisablingHibernationWakesInThePolicyWrite(t *testing.T) {
	st := testStore(t)
	srv := runningHibernatable()
	srv.Hibernated = true
	if err := st.CreateServer(srv); err != nil {
		t.Fatal(err)
	}
	if err := st.UpdateServerHibernation(srv.ID, models.Hibernation{}, nil); err != nil {
		t.Fatal(err)
	}
	got, err := st.GetServer(srv.ID)
	if err != nil || got.Hibernated || got.Hibernation.Enabled {
		t.Fatalf("disabled policy left server asleep: %+v, %v", got, err)
	}
}
