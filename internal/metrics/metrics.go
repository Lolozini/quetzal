// Package metrics exposes Prometheus metrics for Quetzal, including a
// store-backed collector reporting server counts by phase.
package metrics

import (
	"net/http"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/lolozini/quetzal/internal/store"
)

type serverCollector struct {
	store *store.Store
	desc  *prometheus.Desc
}

func (c *serverCollector) Describe(ch chan<- *prometheus.Desc) { ch <- c.desc }

func (c *serverCollector) Collect(ch chan<- prometheus.Metric) {
	servers, err := c.store.ListServers()
	if err != nil {
		return
	}
	type key struct{ phase, desired string }
	counts := map[key]int{}
	for _, s := range servers {
		counts[key{string(s.Status.Phase), string(s.DesiredState)}]++
	}
	for k, v := range counts {
		ch <- prometheus.MustNewConstMetric(c.desc, prometheus.GaugeValue, float64(v), k.phase, k.desired)
	}
}

// Handler returns an HTTP handler exposing Go/process metrics plus Quetzal's
// store-backed server metrics.
func Handler(st *store.Store) http.Handler {
	reg := prometheus.NewRegistry()
	reg.MustRegister(
		collectors.NewGoCollector(),
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
		&serverCollector{
			store: st,
			desc: prometheus.NewDesc(
				"quetzal_servers",
				"Number of game servers by observed phase and desired state.",
				[]string{"phase", "desired"}, nil,
			),
		},
	)
	return promhttp.HandlerFor(reg, promhttp.HandlerOpts{})
}
