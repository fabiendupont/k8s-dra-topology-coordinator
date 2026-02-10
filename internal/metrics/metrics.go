// Package metrics provides Prometheus metrics for the topology coordinator.
package metrics

import (
	"net/http"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

var (
	ReconciliationDuration = prometheus.NewHistogram(prometheus.HistogramOpts{
		Namespace: "nodepartition",
		Subsystem: "controller",
		Name:      "reconciliation_duration_seconds",
		Help:      "Duration of reconciliation cycles in seconds.",
		Buckets:   prometheus.DefBuckets,
	})

	ReconciliationErrors = prometheus.NewCounter(prometheus.CounterOpts{
		Namespace: "nodepartition",
		Subsystem: "controller",
		Name:      "reconciliation_errors_total",
		Help:      "Total number of reconciliation errors.",
	})

	NodesTotal = prometheus.NewGauge(prometheus.GaugeOpts{
		Namespace: "nodepartition",
		Subsystem: "controller",
		Name:      "nodes_total",
		Help:      "Number of nodes with topology information.",
	})

	DeviceClassesTotal = prometheus.NewGauge(prometheus.GaugeOpts{
		Namespace: "nodepartition",
		Subsystem: "controller",
		Name:      "deviceclasses_total",
		Help:      "Number of managed DeviceClasses.",
	})

	TopologyRulesTotal = prometheus.NewGauge(prometheus.GaugeOpts{
		Namespace: "nodepartition",
		Subsystem: "controller",
		Name:      "topology_rules_total",
		Help:      "Number of active topology rules.",
	})

	WebhookExpansions = prometheus.NewCounter(prometheus.CounterOpts{
		Namespace: "nodepartition",
		Subsystem: "webhook",
		Name:      "expansions_total",
		Help:      "Total number of partition claims expanded.",
	})

	WebhookErrors = prometheus.NewCounter(prometheus.CounterOpts{
		Namespace: "nodepartition",
		Subsystem: "webhook",
		Name:      "errors_total",
		Help:      "Total number of webhook errors.",
	})
)

func init() {
	prometheus.MustRegister(
		ReconciliationDuration,
		ReconciliationErrors,
		NodesTotal,
		DeviceClassesTotal,
		TopologyRulesTotal,
		WebhookExpansions,
		WebhookErrors,
	)
}

// Handler returns an HTTP handler for the /metrics endpoint.
func Handler() http.Handler {
	return promhttp.Handler()
}
