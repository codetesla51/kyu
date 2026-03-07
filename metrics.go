package kyu

import (
	"github.com/prometheus/client_golang/prometheus"
)

// metrics holds all Prometheus collectors for a Queue instance.
// Using a struct (instead of package-level vars) means multiple Queue instances
// in the same process won't collide on registration.
type metrics struct {
	jobsProcessed *prometheus.CounterVec
	queueDepth    prometheus.Gauge
	jobTotal      prometheus.Counter
	jobFailures   *prometheus.CounterVec
	jobsDeadTotal prometheus.Counter
	registry      *prometheus.Registry
}

// newMetrics creates and registers a fresh set of Prometheus metrics using a
// dedicated (non-default) registry so the library is safe to import multiple
// times in tests.
func newMetrics() *metrics {
	reg := prometheus.NewRegistry()

	m := &metrics{
		registry: reg,
		jobsProcessed: prometheus.NewCounterVec(
			prometheus.CounterOpts{
				Name: "kyu_jobs_processed_total",
				Help: "Total number of jobs processed, labelled by status.",
			},
			[]string{"status"},
		),
		queueDepth: prometheus.NewGauge(
			prometheus.GaugeOpts{
				Name: "kyu_queue_depth",
				Help: "Current number of jobs waiting in the pending queue.",
			},
		),
		jobTotal: prometheus.NewCounter(
			prometheus.CounterOpts{
				Name: "kyu_jobs_total",
				Help: "Total number of jobs ever submitted.",
			},
		),
		jobFailures: prometheus.NewCounterVec(
			prometheus.CounterOpts{
				Name: "kyu_job_failures_total",
				Help: "Total number of job execution failures, labelled by job type.",
			},
			[]string{"job_type"},
		),
		jobsDeadTotal: prometheus.NewCounter(
			prometheus.CounterOpts{
				Name: "kyu_jobs_dead_total",
				Help: "Total number of jobs that exhausted all retries.",
			},
		),
	}

	reg.MustRegister(
		m.jobsProcessed,
		m.queueDepth,
		m.jobTotal,
		m.jobFailures,
		m.jobsDeadTotal,
		// Include the standard Go runtime and process collectors.
		prometheus.NewGoCollector(),
		prometheus.NewProcessCollector(prometheus.ProcessCollectorOpts{}),
	)

	return m
}
