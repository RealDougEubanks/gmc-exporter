// Package otlpmetrics exports readings over OpenTelemetry.
//
// It exists because Grafana Alloy is natively an OTLP receiver, so speaking
// OTLP puts the readings into an Alloy pipeline with no bridge or exporter in
// between. From there the same data can be routed onward to Prometheus, Mimir,
// or anything else Alloy is configured with, without this exporter needing to
// know about any of them.
//
// Nothing here blocks the poll loop. Publish only records the current values;
// the metric SDK's periodic reader does the exporting on its own goroutine, so
// a collector that is down or slow costs a background export attempt rather
// than a missed poll.
package otlpmetrics

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetricgrpc"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetrichttp"
	"go.opentelemetry.io/otel/metric"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/resource"
	semconv "go.opentelemetry.io/otel/semconv/v1.43.0"

	"github.com/RealDougEubanks/gmc-exporter/internal/config"
	"github.com/RealDougEubanks/gmc-exporter/internal/reading"
	"github.com/RealDougEubanks/gmc-exporter/internal/sink"
)

// Name is the sink's stable identifier, used in logs and as a metric label.
const Name = "otlp"

// ServiceName identifies this exporter in the resource attributes every
// exported metric carries.
const ServiceName = "gmc-exporter"

// ProtocolGRPC and ProtocolHTTP are the supported values of cfg.Protocol.
const (
	ProtocolGRPC = "grpc"
	ProtocolHTTP = "http"
)

// exportInterval is how often the periodic reader ships what it has.
//
// It is deliberately shorter than any sane poll interval, so a reading reaches
// the collector in the same rough timeframe it was taken rather than being held
// until the following poll.
const exportInterval = 30 * time.Second

// defaultTimeout bounds an export and the shutdown when the configuration does
// not give a timeout of its own.
const defaultTimeout = 15 * time.Second

// Sink exports readings through the OpenTelemetry metric SDK.
type Sink struct {
	provider        *sdkmetric.MeterProvider
	shutdownTimeout time.Duration

	// mu guards the values the observable-gauge callback reads. The callback
	// runs on the reader's goroutine, not the poll goroutine.
	mu      sync.RWMutex
	have    bool
	current reading.Reading

	closeOnce sync.Once
	closeErr  error
}

var _ sink.Sink = (*Sink)(nil)

// New builds the exporter, meter provider and instruments.
//
// It does not wait for the collector. The gRPC client is created lazily and the
// HTTP client does not connect until it sends, so a collector that is down at
// startup delays nothing and fails nothing: the exporter comes up, the periodic
// reader retries in the background, and readings flow as soon as the collector
// returns. Refusing to start because a downstream is briefly unreachable would
// be the worse failure for a process meant to run unattended for months.
func New(cfg config.OTLP, version string) (*Sink, error) {
	if cfg.Endpoint == "" {
		return nil, errors.New("otlpmetrics: endpoint is required")
	}

	timeout := cfg.Timeout
	if timeout <= 0 {
		timeout = defaultTimeout
	}

	exporter, err := newExporter(cfg, timeout)
	if err != nil {
		return nil, err
	}

	if version == "" {
		version = "unknown"
	}
	// The resource is built from an explicit attribute set rather than merged
	// with resource.Default(), because merging fails on a schema URL mismatch
	// and a resource detector disagreeing about semconv versions must not be
	// able to stop the exporter starting.
	res := resource.NewWithAttributes(
		semconv.SchemaURL,
		semconv.ServiceName(ServiceName),
		semconv.ServiceVersion(version),
	)

	s := &Sink{shutdownTimeout: timeout}

	s.provider = sdkmetric.NewMeterProvider(
		sdkmetric.WithResource(res),
		sdkmetric.WithReader(sdkmetric.NewPeriodicReader(exporter,
			sdkmetric.WithInterval(exportInterval),
			sdkmetric.WithTimeout(timeout),
		)),
	)

	if err := s.registerInstruments(); err != nil {
		// The provider owns the exporter, so shut it down rather than leaking
		// its goroutine and connection.
		s.shutdown()
		return nil, err
	}

	return s, nil
}

// newExporter builds the protocol-specific exporter.
func newExporter(cfg config.OTLP, timeout time.Duration) (sdkmetric.Exporter, error) {
	// A configured endpoint may be written either as a bare host:port, which is
	// what OTLP/gRPC conventionally uses, or as a full URL, which is what
	// people copy out of Alloy's HTTP receiver configuration. Both are accepted
	// rather than rejecting one and making the operator guess which.
	hasScheme := strings.Contains(cfg.Endpoint, "://")

	switch cfg.Protocol {
	case ProtocolHTTP:
		opts := []otlpmetrichttp.Option{
			otlpmetrichttp.WithTimeout(timeout),
		}
		if hasScheme {
			opts = append(opts, otlpmetrichttp.WithEndpointURL(cfg.Endpoint))
		} else {
			opts = append(opts, otlpmetrichttp.WithEndpoint(cfg.Endpoint))
		}
		if len(cfg.Headers) > 0 {
			opts = append(opts, otlpmetrichttp.WithHeaders(cfg.Headers))
		}
		if cfg.Insecure {
			opts = append(opts, otlpmetrichttp.WithInsecure())
		}
		return otlpmetrichttp.New(context.Background(), opts...)

	case ProtocolGRPC, "":
		opts := []otlpmetricgrpc.Option{
			otlpmetricgrpc.WithTimeout(timeout),
		}
		if hasScheme {
			opts = append(opts, otlpmetricgrpc.WithEndpointURL(cfg.Endpoint))
		} else {
			opts = append(opts, otlpmetricgrpc.WithEndpoint(cfg.Endpoint))
		}
		if len(cfg.Headers) > 0 {
			opts = append(opts, otlpmetricgrpc.WithHeaders(cfg.Headers))
		}
		if cfg.Insecure {
			opts = append(opts, otlpmetricgrpc.WithInsecure())
		}
		return otlpmetricgrpc.New(context.Background(), opts...)

	default:
		return nil, fmt.Errorf("otlpmetrics: unsupported protocol %q, want %q or %q",
			cfg.Protocol, ProtocolGRPC, ProtocolHTTP)
	}
}

// registerInstruments creates the observable gauges and the callback that
// reports them.
//
// The metric names match the ones the Prometheus sink exposes so that a
// dashboard written against one backend keeps working against the other.
func (s *Sink) registerInstruments() error {
	meter := s.provider.Meter(ServiceName)

	cpm, err := meter.Float64ObservableGauge("gmc_cpm",
		metric.WithDescription("Counts per minute reported by the device on the most recent poll."),
		metric.WithUnit("{count}/min"),
	)
	if err != nil {
		return fmt.Errorf("otlpmetrics: gmc_cpm: %w", err)
	}

	usv, err := meter.Float64ObservableGauge("gmc_usv_per_hour",
		metric.WithDescription("Dose rate in microsieverts per hour, converted through the device calibration table."),
		metric.WithUnit("uSv/h"),
	)
	if err != nil {
		return fmt.Errorf("otlpmetrics: gmc_usv_per_hour: %w", err)
	}

	average, err := meter.Float64ObservableGauge("gmc_average_cpm",
		metric.WithDescription("Running mean count rate over the configured averaging window."),
		metric.WithUnit("{count}/min"),
	)
	if err != nil {
		return fmt.Errorf("otlpmetrics: gmc_average_cpm: %w", err)
	}

	volts, err := meter.Float64ObservableGauge("gmc_battery_volts",
		metric.WithDescription("Battery voltage reported by the device."),
		metric.WithUnit("V"),
	)
	if err != nil {
		return fmt.Errorf("otlpmetrics: gmc_battery_volts: %w", err)
	}

	temperature, err := meter.Float64ObservableGauge("gmc_temperature_celsius",
		metric.WithDescription("Internal temperature reported by the device."),
		metric.WithUnit("Cel"),
	)
	if err != nil {
		return fmt.Errorf("otlpmetrics: gmc_temperature_celsius: %w", err)
	}

	callback := func(_ context.Context, o metric.Observer) error {
		s.mu.RLock()
		r, have := s.current, s.have
		s.mu.RUnlock()

		// Nothing is observed before the first reading, and an optional field
		// the device did not supply is not observed at all. Reporting a zero or
		// a value from an earlier cycle would put a number on the dashboard
		// that the hardware never produced.
		if !have {
			return nil
		}

		o.ObserveFloat64(cpm, float64(r.CPM))
		o.ObserveFloat64(usv, r.MicroSievertsPerHour)
		o.ObserveFloat64(average, r.AverageCPM)
		if r.Voltage.Valid {
			o.ObserveFloat64(volts, r.Voltage.Value)
		}
		if r.TemperatureC.Valid {
			o.ObserveFloat64(temperature, r.TemperatureC.Value)
		}
		return nil
	}

	if _, err := meter.RegisterCallback(callback, cpm, usv, average, volts, temperature); err != nil {
		return fmt.Errorf("otlpmetrics: register callback: %w", err)
	}
	return nil
}

// Name implements sink.Sink.
func (s *Sink) Name() string { return Name }

// Publish records the reading for the next export.
//
// It performs no I/O, so an unreachable collector produces no error here. The
// export happens on the reader's own schedule, and its failures are the SDK's
// to retry.
func (s *Sink) Publish(ctx context.Context, r reading.Reading) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	s.current = r
	s.have = true
	return nil
}

// Close shuts the meter provider down, flushing whatever has not been exported.
//
// The shutdown is bounded by its own timeout rather than inheriting an
// unbounded context: a collector that has stopped answering must not be able to
// hold the process open at exit.
func (s *Sink) Close() error {
	s.closeOnce.Do(func() { s.closeErr = s.shutdown() })
	return s.closeErr
}

// shutdown stops the provider under a bounded context.
func (s *Sink) shutdown() error {
	if s.provider == nil {
		return nil
	}

	timeout := s.shutdownTimeout
	if timeout <= 0 {
		timeout = defaultTimeout
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	if err := s.provider.Shutdown(ctx); err != nil {
		return fmt.Errorf("otlpmetrics: shutdown: %w", err)
	}
	return nil
}
