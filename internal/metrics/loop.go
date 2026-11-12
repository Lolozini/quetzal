package metrics

import (
	"fmt"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

// StallLimit is how long the controller's loop may go without a sign of life
// before its /healthz fails and Kubernetes restarts it. Every step and every
// server of a pass is a sign of life, so only a step stuck on one call (an exec
// with no deadline, a cluster that never answers) runs past it; a long pass that
// is making progress does not.
const StallLimit = 10 * time.Minute

// Loop is the controller's account of its own resync loop: what it exports as
// metrics and what its liveness probe reads. Before it, /healthz answered "ok"
// for as long as the process could serve HTTP, so a loop stuck on one call, with
// every game server left unattended, was neither seen nor restarted, and the only
// metric said how many servers there were.
//
// A replica that is not leading has no loop to watch: it is healthy by being
// ready to take over. So is one waiting for its next pass: between two passes the
// loop sits on its ticker and makes no progress by design, and with a `resync` of
// fifteen minutes that is longer than the stall limit.
type Loop struct {
	mu          sync.Mutex
	now         func() time.Time
	running     bool
	waiting     bool // between two passes, on the ticker
	lastBeat    time.Time
	lastTickEnd time.Time
	lastTick    time.Duration
	ticks       uint64
	errors      map[string]uint64
	stepTime    map[string]time.Duration
}

// NewLoop returns a Loop that is not running yet.
func NewLoop() *Loop {
	return &Loop{now: time.Now, errors: map[string]uint64{}, stepTime: map[string]time.Duration{}}
}

// Start marks the loop as running, as a replica does when it takes the lead.
func (l *Loop) Start() {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.running, l.waiting, l.lastBeat = true, false, l.now()
}

// Stop marks it as not running (leadership lost, shutting down).
func (l *Loop) Stop() {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.running = false
}

// Beat says the loop is alive: it has just finished something, and is working.
func (l *Loop) Beat() {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.waiting, l.lastBeat = false, l.now()
}

// Wait says the loop has nothing to do until its next pass: it is not stalled for
// as long as it waits. The next Beat, a step or a server, ends the wait.
func (l *Loop) Wait() {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.waiting = true
}

// Step records that a step of a pass took d and ended with err, and beats.
func (l *Loop) Step(name string, d time.Duration, err error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.stepTime[name] = d
	if err != nil {
		l.errors[name]++
	}
	l.waiting, l.lastBeat = false, l.now()
}

// Error counts a failure inside a step, one server's reconcile among a pass's.
func (l *Loop) Error(step string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.errors[step]++
}

// TickDone records a whole pass, which took d.
func (l *Loop) TickDone(d time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.ticks++
	l.lastTick = d
	l.lastTickEnd = l.now()
	l.lastBeat = l.lastTickEnd
	l.waiting = true // a pass is done: the loop waits for the next one
}

// Healthy is nil unless the loop is running, is not waiting for its next pass, and
// has not beaten for StallLimit.
func (l *Loop) Healthy() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if !l.running || l.waiting {
		return nil
	}
	if since := l.now().Sub(l.lastBeat); since > StallLimit {
		return fmt.Errorf("the controller loop has not made progress for %s", since.Round(time.Second))
	}
	return nil
}

var (
	descLeader    = prometheus.NewDesc("quetzal_controller_leader", "1 while this replica runs the controller loop, 0 while it stands by.", nil, nil)
	descTicks     = prometheus.NewDesc("quetzal_controller_ticks_total", "Resync passes the controller has completed.", nil, nil)
	descTickTime  = prometheus.NewDesc("quetzal_controller_last_tick_duration_seconds", "How long the last resync pass took.", nil, nil)
	descTickEnd   = prometheus.NewDesc("quetzal_controller_last_tick_timestamp_seconds", "When the last resync pass ended, in Unix seconds (0 before the first).", nil, nil)
	descSinceBeat = prometheus.NewDesc("quetzal_controller_seconds_since_progress", "Seconds since the loop last finished a step or a server; what the liveness probe watches (0 while waiting for the next pass, and when not leading).", nil, nil)
	descStepTime  = prometheus.NewDesc("quetzal_controller_step_duration_seconds", "How long each step of the last pass took.", []string{"step"}, nil)
	descStepErrs  = prometheus.NewDesc("quetzal_controller_step_errors_total", "Failures by step: an error, a panic, or one server's reconcile failing.", []string{"step"}, nil)
)

// Describe and Collect make a Loop a prometheus.Collector.
func (l *Loop) Describe(ch chan<- *prometheus.Desc) {
	for _, d := range []*prometheus.Desc{descLeader, descTicks, descTickTime, descTickEnd, descSinceBeat, descStepTime, descStepErrs} {
		ch <- d
	}
}

func (l *Loop) Collect(ch chan<- prometheus.Metric) {
	l.mu.Lock()
	defer l.mu.Unlock()
	leader, since := 0.0, 0.0
	if l.running {
		leader = 1
		if !l.waiting {
			since = l.now().Sub(l.lastBeat).Seconds()
		}
	}
	end := 0.0
	if !l.lastTickEnd.IsZero() {
		end = float64(l.lastTickEnd.UnixNano()) / 1e9
	}
	ch <- prometheus.MustNewConstMetric(descLeader, prometheus.GaugeValue, leader)
	ch <- prometheus.MustNewConstMetric(descTicks, prometheus.CounterValue, float64(l.ticks))
	ch <- prometheus.MustNewConstMetric(descTickTime, prometheus.GaugeValue, l.lastTick.Seconds())
	ch <- prometheus.MustNewConstMetric(descTickEnd, prometheus.GaugeValue, end)
	ch <- prometheus.MustNewConstMetric(descSinceBeat, prometheus.GaugeValue, since)
	for step, d := range l.stepTime {
		ch <- prometheus.MustNewConstMetric(descStepTime, prometheus.GaugeValue, d.Seconds(), step)
	}
	for step, n := range l.errors {
		ch <- prometheus.MustNewConstMetric(descStepErrs, prometheus.CounterValue, float64(n), step)
	}
}
