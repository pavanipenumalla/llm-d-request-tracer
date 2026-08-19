package requesttracer

import "github.com/prometheus/client_golang/prometheus"

const metricsSubsystem = "request_tracer"

// collectors holds the tracer's own health metrics.
type collectors struct {
	emitted     prometheus.Counter
	dropped     prometheus.Counter
	writeErrors prometheus.Counter
	inProgress  prometheus.Gauge
	swept       prometheus.Counter
}

func newCollectors() *collectors {
	return &collectors{
		emitted: prometheus.NewCounter(prometheus.CounterOpts{
			Subsystem: metricsSubsystem,
			Name:      "traces_emitted_total",
			Help:      "Traces successfully written to the sink.",
		}),
		dropped: prometheus.NewCounter(prometheus.CounterOpts{
			Subsystem: metricsSubsystem,
			Name:      "traces_dropped_total",
			Help:      "Traces dropped because the sink buffer was full.",
		}),
		writeErrors: prometheus.NewCounter(prometheus.CounterOpts{
			Subsystem: metricsSubsystem,
			Name:      "write_errors_total",
			Help:      "Errors marshalling or writing a trace.",
		}),
		inProgress: prometheus.NewGauge(prometheus.GaugeOpts{
			Subsystem: metricsSubsystem,
			Name:      "in_progress",
			Help:      "Traces currently being assembled.",
		}),
		swept: prometheus.NewCounter(prometheus.CounterOpts{
			Subsystem: metricsSubsystem,
			Name:      "traces_swept_total",
			Help:      "Incomplete traces finalized by the staleness sweeper.",
		}),
	}
}

// all returns every collector for registration.
func (c *collectors) all() []prometheus.Collector {
	return []prometheus.Collector{c.emitted, c.dropped, c.writeErrors, c.inProgress, c.swept}
}
