package metrics

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
)

// clock is a Loop's time, moved by the test.
func newTestLoop() (*Loop, *time.Time) {
	now := time.Date(2026, 11, 11, 23, 0, 0, 0, time.UTC)
	l := NewLoop()
	l.now = func() time.Time { return now }
	return l, &now
}

// Before the loop was watched, /healthz said "ok" for as long as the process could
// answer HTTP, so a loop stuck on one call looked healthy for good.
func TestLoopIsUnhealthyOnlyWhenARunningLoopStalls(t *testing.T) {
	l, now := newTestLoop()

	*now = now.Add(time.Hour)
	if err := l.Healthy(); err != nil {
		t.Fatalf("a replica that is not leading is unhealthy: %v", err)
	}

	l.Start()
	*now = now.Add(StallLimit - time.Second)
	if err := l.Healthy(); err != nil {
		t.Fatalf("unhealthy before the stall limit: %v", err)
	}
	*now = now.Add(2 * time.Second)
	if err := l.Healthy(); err == nil {
		t.Fatal("a loop that has made no progress for longer than the limit is healthy")
	}

	l.Beat()
	if err := l.Healthy(); err != nil {
		t.Fatalf("still unhealthy after a sign of life: %v", err)
	}

	*now = now.Add(2 * StallLimit)
	l.Stop()
	if err := l.Healthy(); err != nil {
		t.Fatalf("a replica that lost the lead is unhealthy: %v", err)
	}
}

// A pass that is making progress, however long, is not a stall: every step and
// every server beats.
func TestALongPassThatProgressesStaysHealthy(t *testing.T) {
	l, now := newTestLoop()
	l.Start()
	for i := 0; i < 20; i++ { // 20 servers of 3 minutes each: an hour
		*now = now.Add(3 * time.Minute)
		if err := l.Healthy(); err != nil {
			t.Fatalf("server %d: %v", i, err)
		}
		l.Beat()
	}
}

func gather(t *testing.T, c prometheus.Collector) map[string]*dto.MetricFamily {
	t.Helper()
	reg := prometheus.NewRegistry()
	reg.MustRegister(c)
	fams, err := reg.Gather()
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]*dto.MetricFamily{}
	for _, f := range fams {
		out[f.GetName()] = f
	}
	return out
}

func labelled(f *dto.MetricFamily, step string) *dto.Metric {
	for _, m := range f.GetMetric() {
		for _, lp := range m.GetLabel() {
			if lp.GetName() == "step" && lp.GetValue() == step {
				return m
			}
		}
	}
	return nil
}

func TestLoopExportsWhatItDoes(t *testing.T) {
	l, now := newTestLoop()
	fams := gather(t, l)
	if got := fams["quetzal_controller_leader"].GetMetric()[0].GetGauge().GetValue(); got != 0 {
		t.Errorf("leader = %v before Start, want 0", got)
	}

	l.Start()
	l.Step("reconcile", 2*time.Second, nil)
	l.Step("backups", time.Second, errors.New("boom"))
	l.Error("reconcile server")
	l.Error("reconcile server")
	*now = now.Add(5 * time.Second)
	l.TickDone(7 * time.Second)
	l.Beat() // the next pass has begun: the loop is working, no longer waiting
	*now = now.Add(30 * time.Second)

	fams = gather(t, l)
	val := func(name string) float64 {
		f, ok := fams[name]
		if !ok {
			t.Fatalf("metric %s is not exported", name)
		}
		if g := f.GetMetric()[0].GetGauge(); g != nil {
			return g.GetValue()
		}
		return f.GetMetric()[0].GetCounter().GetValue()
	}
	if val("quetzal_controller_leader") != 1 {
		t.Error("leader is not 1 while running")
	}
	if val("quetzal_controller_ticks_total") != 1 || val("quetzal_controller_last_tick_duration_seconds") != 7 {
		t.Error("ticks or last tick duration are wrong")
	}
	if got := val("quetzal_controller_seconds_since_progress"); got != 30 {
		t.Errorf("seconds since progress = %v, want 30", got)
	}
	if got := val("quetzal_controller_last_tick_timestamp_seconds"); got != float64(now.Add(-30*time.Second).Unix()) {
		t.Errorf("last tick timestamp = %v", got)
	}
	if m := labelled(fams["quetzal_controller_step_duration_seconds"], "reconcile"); m == nil || m.GetGauge().GetValue() != 2 {
		t.Errorf("reconcile step duration missing or wrong: %v", m)
	}
	errs := fams["quetzal_controller_step_errors_total"]
	if m := labelled(errs, "backups"); m == nil || m.GetCounter().GetValue() != 1 {
		t.Errorf("backups errors = %v, want 1", m)
	}
	if m := labelled(errs, "reconcile server"); m == nil || m.GetCounter().GetValue() != 2 {
		t.Errorf("reconcile server errors = %v, want 2", m)
	}
	if m := labelled(errs, "reconcile"); m != nil {
		t.Error("a step that did not fail has an error counter")
	}
}

// The probe's message is read by whoever looks at why the pod restarted.
func TestUnhealthyMessageSaysHowLong(t *testing.T) {
	l, now := newTestLoop()
	l.Start()
	*now = now.Add(StallLimit + 90*time.Second)
	err := l.Healthy()
	if err == nil || !strings.Contains(err.Error(), "11m30s") {
		t.Fatalf("message = %v, want it to say 11m30s", err)
	}
}

// Between two passes the loop waits on its ticker and makes no progress by design.
// With a resync of fifteen minutes that is longer than the stall limit, and the
// liveness probe restarted a controller that was only waiting for its next pass.
func TestWaitingForTheNextPassIsNotAStall(t *testing.T) {
	l, now := newTestLoop()
	l.Start()
	l.TickDone(2 * time.Second)

	*now = now.Add(2 * time.Hour)
	if err := l.Healthy(); err != nil {
		t.Fatalf("a controller waiting for its next pass is stalled: %v", err)
	}
	if got := gather(t, l)["quetzal_controller_seconds_since_progress"].GetMetric()[0].GetGauge().GetValue(); got != 0 {
		t.Errorf("seconds since progress = %v while waiting, want 0", got)
	}

	l.Beat() // the next pass begins, and a stall is possible again
	*now = now.Add(StallLimit + time.Second)
	if err := l.Healthy(); err == nil {
		t.Fatal("a loop that began a pass and made no progress for the stall limit is healthy")
	}
	l.TickDone(time.Second)
	if err := l.Healthy(); err != nil {
		t.Fatalf("still unhealthy after the pass ended: %v", err)
	}
}

// A pass started by a console event (the kick) ends in a wait too, not in a
// stall once the limit has gone by.
func TestWaitAfterAKickedPass(t *testing.T) {
	l, now := newTestLoop()
	l.Start()
	l.TickDone(time.Second)
	l.Step("reconcile", time.Second, nil) // a kicked pass: a step, no TickDone
	l.Wait()
	*now = now.Add(time.Hour)
	if err := l.Healthy(); err != nil {
		t.Fatalf("a controller idle after a kicked pass is stalled: %v", err)
	}
}
