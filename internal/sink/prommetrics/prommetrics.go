// Package prommetrics exposes readings and exporter health on a Prometheus
// scrape endpoint.
//
// This is the most robust of the sinks and the one worth reaching for first. It
// needs no credentials, it performs no I/O on the poll path so it cannot stall
// or fail a poll, and it keeps working unchanged when every push-based backend
// is reconfigured. It is also the only sink that reports on the others: the
// fan-out hands it each publish outcome through sink.Observer, so a silently
// failing InfluxDB or MQTT sink is visible in the same scrape as the readings.
package prommetrics

import (
	"context"
	"errors"
	"net/http"
	"runtime"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/RealDougEubanks/gmc-exporter/internal/config"
	"github.com/RealDougEubanks/gmc-exporter/internal/reading"
	"github.com/RealDougEubanks/gmc-exporter/internal/sink"
)

// Name is the sink's stable identifier. It appears in logs and, because this
// sink observes the whole fan-out, as its own value of the sink label.
const Name = "prometheus"

// DefaultPath is used when the configuration leaves the scrape path empty.
const DefaultPath = "/metrics"

// Build identifies the running binary in gmc_build_info.
//
// It is passed in rather than read from a package variable so that the metric
// is testable and so that this package does not dictate how the main package
// stamps its version.
type Build struct {
	Version string
	Commit  string
}

// Descriptors for the reading metrics.
//
// The reading gauges are emitted by a custom collector rather than held in
// prometheus.Gauge values, because a registered Gauge is always present in a
// scrape. A field the device did not supply must be absent, not zero and not
// the value from a previous cycle: a stale temperature reported as current is
// worse than no temperature at all.
var (
	cpmDesc = prometheus.NewDesc(
		"gmc_cpm",
		"Counts per minute reported by the device on the most recent poll.",
		nil, nil,
	)
	usvDesc = prometheus.NewDesc(
		"gmc_usv_per_hour",
		"Dose rate in microsieverts per hour, converted through the device calibration table.",
		nil, nil,
	)
	averageDesc = prometheus.NewDesc(
		"gmc_average_cpm",
		"Running mean count rate over the configured averaging window.",
		nil, nil,
	)
	voltsDesc = prometheus.NewDesc(
		"gmc_battery_volts",
		"Battery voltage reported by the device. Absent on models that do not supply it.",
		nil, nil,
	)
	temperatureDesc = prometheus.NewDesc(
		"gmc_temperature_celsius",
		"Internal temperature in degrees Celsius. Absent on models that do not supply it.",
		nil, nil,
	)
)

// Sink publishes readings into a dedicated Prometheus registry.
//
// It implements both sink.Sink and sink.Observer.
type Sink struct {
	path     string
	registry *prometheus.Registry

	readings *readingCollector

	pollSuccess     prometheus.Counter
	pollFailure     prometheus.Counter
	publishTotal    *prometheus.CounterVec
	publishDuration *prometheus.HistogramVec
	lastRead        prometheus.Gauge
	buildInfo       *prometheus.GaugeVec
}

var (
	_ sink.Sink     = (*Sink)(nil)
	_ sink.Observer = (*Sink)(nil)
)

// New builds the sink and its registry.
//
// The registry is dedicated rather than prometheus.DefaultRegisterer so that
// tests are isolated from each other, and so that no library linked into the
// binary can inject its own metrics into this exporter's scrape output.
func New(cfg config.Prometheus, build Build) *Sink {
	path := cfg.Path
	if path == "" {
		path = DefaultPath
	}

	s := &Sink{
		path:     path,
		registry: prometheus.NewRegistry(),
		readings: &readingCollector{},
		pollSuccess: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "gmc_poll_success_total",
			Help: "Polls of the device that returned a complete reading.",
		}),
		pollFailure: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "gmc_poll_failure_total",
			Help: "Polls of the device that failed.",
		}),
		publishTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "gmc_sink_publish_total",
			Help: "Publish attempts per sink, by outcome.",
		}, []string{"sink", "result"}),
		publishDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name: "gmc_sink_publish_duration_seconds",
			Help: "Time each sink took to publish one reading.",
			// The buckets span five milliseconds to a few seconds, which is
			// the range a network sink occupies between a healthy local
			// backend and one that is about to hit the fan-out timeout.
			Buckets: prometheus.ExponentialBuckets(0.005, 2, 10),
		}, []string{"sink"}),
		lastRead: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "gmc_last_successful_read_timestamp_seconds",
			Help: "Unix time of the most recent successful read from the device.",
		}),
		buildInfo: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "gmc_build_info",
			Help: "Build information for the running binary, always 1.",
		}, []string{"version", "commit", "goversion"}),
	}

	// MustRegister would panic, and this process must never die over a metric.
	// Registration can only fail on a duplicate, which a fresh registry makes
	// impossible, so a failure here is a programming error and the collector is
	// simply left out rather than taking the exporter with it.
	for _, c := range []prometheus.Collector{
		s.readings, s.pollSuccess, s.pollFailure,
		s.publishTotal, s.publishDuration, s.lastRead, s.buildInfo,
	} {
		_ = s.registry.Register(c)
	}

	version := build.Version
	if version == "" {
		version = "unknown"
	}
	commit := build.Commit
	if commit == "" {
		commit = "unknown"
	}
	s.buildInfo.WithLabelValues(version, commit, runtime.Version()).Set(1)

	return s
}

// Name implements sink.Sink.
func (s *Sink) Name() string { return Name }

// Path is where the scrape endpoint should be mounted.
func (s *Sink) Path() string { return s.path }

// Registry exposes the dedicated registry, for a caller that wants to gather
// directly or register an additional collector alongside these.
func (s *Sink) Registry() *prometheus.Registry { return s.registry }

// Handler serves the scrape endpoint from this sink's registry.
//
// A collector error is reported to the scraper rather than logged and swallowed
// or turned into a panic, so a broken metric shows up as a failed scrape
// instead of silently missing data.
func (s *Sink) Handler() http.Handler {
	return promhttp.HandlerFor(s.registry, promhttp.HandlerOpts{
		ErrorHandling: promhttp.HTTPErrorOnError,
	})
}

// Publish records the reading. It only takes a mutex and copies five numbers:
// there is no I/O here, so this sink cannot slow or fail a poll.
func (s *Sink) Publish(_ context.Context, r reading.Reading) error {
	s.readings.set(r)
	return nil
}

// Close implements sink.Sink. There is nothing to release, and the metrics stay
// available to a final scrape until the HTTP server itself stops.
func (s *Sink) Close() error { return nil }

// ObservePublish implements sink.Observer, recording one sink's outcome.
//
// A skipped publish is counted separately from a failure because it is not one:
// a sink that needs a temperature the device did not supply has behaved
// correctly, and folding that into the failure count would make a healthy
// exporter look broken.
func (s *Sink) ObservePublish(sinkName string, duration time.Duration, err error) {
	var result string
	switch {
	case err == nil:
		result = "success"
	case errors.Is(err, sink.ErrSkipped):
		result = "skipped"
	default:
		result = "failure"
	}

	s.publishTotal.WithLabelValues(sinkName, result).Inc()
	s.publishDuration.WithLabelValues(sinkName).Observe(duration.Seconds())
}

// RecordPollSuccess counts a successful poll and records when it happened.
//
// The timestamp is what makes staleness detectable: a scrape can tell a device
// reading zero counts apart from an exporter that stopped reading the device
// hours ago, which the count rate alone cannot.
func (s *Sink) RecordPollSuccess(t time.Time) {
	s.pollSuccess.Inc()
	s.lastRead.Set(float64(t.UnixNano()) / float64(time.Second))
}

// RecordPollFailure counts a failed poll.
func (s *Sink) RecordPollFailure() { s.pollFailure.Inc() }

// readingCollector holds the most recent reading and emits it on scrape.
//
// It emits nothing at all before the first successful publish, so a freshly
// started exporter does not claim a dose rate of zero it has not measured.
type readingCollector struct {
	mu      sync.RWMutex
	have    bool
	current reading.Reading
}

// set replaces the current reading.
func (c *readingCollector) set(r reading.Reading) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.current = r
	c.have = true
}

// Describe implements prometheus.Collector.
//
// The descriptors are sent even though some metrics may be absent from any
// given scrape, so the registry can still check them for conflicts at
// registration time.
func (c *readingCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- cpmDesc
	ch <- usvDesc
	ch <- averageDesc
	ch <- voltsDesc
	ch <- temperatureDesc
}

// Collect implements prometheus.Collector.
func (c *readingCollector) Collect(ch chan<- prometheus.Metric) {
	c.mu.RLock()
	r, have := c.current, c.have
	c.mu.RUnlock()

	if !have {
		return
	}

	emitGauge(ch, cpmDesc, float64(r.CPM))
	emitGauge(ch, usvDesc, r.MicroSievertsPerHour)
	emitGauge(ch, averageDesc, r.AverageCPM)
	if r.Voltage.Valid {
		emitGauge(ch, voltsDesc, r.Voltage.Value)
	}
	if r.TemperatureC.Valid {
		emitGauge(ch, temperatureDesc, r.TemperatureC.Value)
	}
}

// emitGauge sends one constant gauge, reporting a construction failure to the
// scraper instead of panicking the way MustNewConstMetric would.
func emitGauge(ch chan<- prometheus.Metric, desc *prometheus.Desc, v float64) {
	m, err := prometheus.NewConstMetric(desc, prometheus.GaugeValue, v)
	if err != nil {
		m = prometheus.NewInvalidMetric(desc, err)
	}
	ch <- m
}
