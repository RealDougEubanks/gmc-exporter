// Command gmc-exporter reads a GQ Electronics GMC-series Geiger counter over
// USB serial and publishes its readings to the configured telemetry backends.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"runtime"
	"sync"
	"syscall"
	"time"

	"github.com/RealDougEubanks/gmc-exporter/internal/config"
	"github.com/RealDougEubanks/gmc-exporter/internal/gmc"
	"github.com/RealDougEubanks/gmc-exporter/internal/httpserver"
	"github.com/RealDougEubanks/gmc-exporter/internal/poller"
	"github.com/RealDougEubanks/gmc-exporter/internal/serialport"
	"github.com/RealDougEubanks/gmc-exporter/internal/sink"
	"github.com/RealDougEubanks/gmc-exporter/internal/sink/gmcmap"
	"github.com/RealDougEubanks/gmc-exporter/internal/sink/influxv1"
	"github.com/RealDougEubanks/gmc-exporter/internal/sink/influxv2"
	"github.com/RealDougEubanks/gmc-exporter/internal/sink/mqtt"
	"github.com/RealDougEubanks/gmc-exporter/internal/sink/otlpmetrics"
	"github.com/RealDougEubanks/gmc-exporter/internal/sink/prommetrics"
	"github.com/RealDougEubanks/gmc-exporter/internal/sink/radmon"
	"github.com/RealDougEubanks/gmc-exporter/internal/sink/safecast"
)

// Build identifiers, stamped in by the linker. Keeping these visible in logs
// and as a metric means anyone can establish exactly which build is deployed
// without inferring it from an image tag.
var (
	version   = "dev"
	commit    = "unknown"
	buildDate = "unknown"
)

// Exit codes. These matter operationally: watchdogs commonly treat 0, 143 and
// 137 as a deliberate stop and anything else as a crash. Exiting non-zero after
// a clean SIGTERM causes a supervisor to restart a container that was stopped
// on purpose, so a clean shutdown must exit 0.
const (
	exitOK      = 0
	exitFailure = 1
)

func main() {
	os.Exit(run())
}

// run holds the real body so deferred cleanup executes before the process
// exits, which os.Exit would otherwise skip.
func run() int {
	cfg, err := config.Load()
	if err != nil {
		// Configuration problems are startup failures, not transient ones.
		// Fail fast and loudly, naming what is wrong.
		fmt.Fprintf(os.Stderr, "gmc-exporter: %v\n", err)
		return exitFailure
	}

	log := newLogger(cfg)
	slog.SetDefault(log)

	log.Info("starting gmc-exporter",
		"version", version,
		"commit", commit,
		"built", buildDate,
		"go", runtime.Version())
	cfg.LogEffective(log)

	// Signals are trapped before anything is constructed, so a stop request
	// during startup is still honoured.
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()

	// Identify the hardware before building sinks, because Home Assistant
	// discovery wants a stable device identity. This is best-effort: a device
	// that is not ready yet must not prevent startup, since the poll loop
	// will connect and retry on its own.
	device := probeDevice(ctx, cfg, log)

	metrics, sinks, err := buildSinks(cfg, device, log)
	if err != nil {
		log.Error("could not construct sinks", "error", err)
		return exitFailure
	}

	set := sink.NewSet(sinks, sinkTimeout(cfg), log, observerOrNil(metrics))
	defer func() {
		if err := set.Close(); err != nil {
			log.Warn("closing sinks", "error", err)
		}
	}()

	log.Info("sinks enabled", "count", set.Len(), "sinks", set.Names())

	p := poller.New(cfg, log, set, recorderOrNil(metrics), serialport.Open)

	srv := httpserver.New(httpserver.Options{
		Config:         cfg.HTTP,
		StaleAfter:     cfg.StaleThreshold(),
		Status:         p,
		Build:          httpserver.BuildInfo{Version: version, Commit: commit, Built: buildDate},
		SinkNames:      set.Names(),
		MetricsHandler: metricsHandler(metrics),
		MetricsPath:    metricsPath(metrics),
		Log:            log,
	})

	var wg sync.WaitGroup
	errCh := make(chan error, 2)

	wg.Add(1)
	go func() {
		defer wg.Done()
		if err := srv.Run(ctx); err != nil {
			errCh <- fmt.Errorf("http server: %w", err)
		}
	}()

	wg.Add(1)
	go func() {
		defer wg.Done()
		if err := p.Run(ctx); err != nil {
			errCh <- fmt.Errorf("poll loop: %w", err)
		}
	}()

	wg.Wait()
	close(errCh)

	// Only genuinely unrecoverable conditions reach here. Everything the poll
	// loop can retry was already handled without stopping.
	var failures []error
	for err := range errCh {
		failures = append(failures, err)
	}
	if len(failures) > 0 {
		log.Error("shutting down after a fatal error", "error", errors.Join(failures...))
		return exitFailure
	}

	log.Info("shutdown complete")
	return exitOK
}

// newLogger builds the structured logger.
func newLogger(cfg *config.Config) *slog.Logger {
	opts := &slog.HandlerOptions{Level: cfg.Log.Level}
	if cfg.Log.Format == "text" {
		return slog.New(slog.NewTextHandler(os.Stdout, opts))
	}
	return slog.New(slog.NewJSONHandler(os.Stdout, opts))
}

// probeDevice reads the device identity once, for the Home Assistant device
// block and the startup log.
//
// Failure is not fatal. The exporter's job is to keep trying, and a counter
// that is unplugged at container start must not prevent the process from
// coming up and reporting itself unhealthy.
func probeDevice(ctx context.Context, cfg *config.Config, log *slog.Logger) mqtt.Device {
	port, err := serialport.Open(serialport.Config{
		Path:        cfg.Serial.Port,
		Baud:        cfg.Serial.Baud,
		ReadTimeout: cfg.Serial.ReadTimeout,
	})
	if err != nil {
		log.Warn("could not identify the device at startup, continuing anyway",
			"port", cfg.Serial.Port, "error", err)
		return mqtt.Device{}
	}
	defer func() {
		if c, ok := port.(interface{ Close() error }); ok {
			_ = c.Close()
		}
	}()

	probeCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	dev := gmc.NewDevice(port, cfg.Serial.ResponseTimeout)
	var out mqtt.Device

	if raw, err := dev.Exchange(probeCtx, gmc.CmdGetVersion); err != nil {
		log.Warn("could not read device version", "error", err)
	} else if v, err := gmc.ParseVersion(raw); err != nil {
		log.Warn("could not parse device version", "error", err)
	} else {
		out.Model, out.Firmware = v.Model, v.Firmware
	}

	if raw, err := dev.Exchange(probeCtx, gmc.CmdGetSerial); err != nil {
		log.Warn("could not read device serial", "error", err)
	} else if s, err := gmc.ParseSerial(raw); err != nil {
		log.Warn("could not parse device serial", "error", err)
	} else {
		out.Serial = s
	}

	log.Info("identified device",
		"model", out.Model, "firmware", out.Firmware, "serial", out.Serial)
	return out
}

// buildSinks constructs every enabled sink.
//
// A construction failure is fatal, unlike a publish failure. If an operator
// enabled Safecast without an API key they have misconfigured the deployment,
// and starting anyway would silently drop the data they asked to send.
func buildSinks(cfg *config.Config, device mqtt.Device, log *slog.Logger) (*prommetrics.Sink, []sink.Sink, error) {
	var sinks []sink.Sink
	var metrics *prommetrics.Sink

	if cfg.Prometheus.Enabled {
		metrics = prommetrics.New(cfg.Prometheus, prommetrics.Build{Version: version, Commit: commit})
		sinks = append(sinks, metrics)
	}

	if cfg.Radmon.Enabled {
		s, err := radmon.New(cfg.Radmon, cfg.Location, log)
		if err != nil {
			return nil, nil, fmt.Errorf("radmon.org: %w", err)
		}
		sinks = append(sinks, s)
	}

	if cfg.InfluxV1.Enabled {
		s, err := influxv1.New(cfg.InfluxV1, log)
		if err != nil {
			return nil, nil, fmt.Errorf("influxdb 1.x: %w", err)
		}
		sinks = append(sinks, s)
	}

	if cfg.InfluxV2.Enabled {
		s, err := influxv2.New(cfg.InfluxV2, log)
		if err != nil {
			return nil, nil, fmt.Errorf("influxdb 2.x: %w", err)
		}
		sinks = append(sinks, s)
	}

	if cfg.OTLP.Enabled {
		s, err := otlpmetrics.New(cfg.OTLP, version)
		if err != nil {
			return nil, nil, fmt.Errorf("otlp: %w", err)
		}
		sinks = append(sinks, s)
	}

	if cfg.MQTT.Enabled {
		s, err := mqtt.New(cfg.MQTT, device, log)
		if err != nil {
			return nil, nil, fmt.Errorf("mqtt: %w", err)
		}
		sinks = append(sinks, s)
	}

	if cfg.GMCMap.Enabled {
		s, err := gmcmap.New(cfg.GMCMap, cfg.Location, log)
		if err != nil {
			return nil, nil, fmt.Errorf("gmcmap: %w", err)
		}
		sinks = append(sinks, s)
	}

	if cfg.Safecast.Enabled {
		s, err := safecast.New(cfg.Safecast, cfg.Location, log)
		if err != nil {
			return nil, nil, fmt.Errorf("safecast: %w", err)
		}
		sinks = append(sinks, s)
	}

	return metrics, sinks, nil
}

// sinkTimeout bounds one sink's publish. It is kept below the poll interval so
// a stalled backend cannot delay the next reading.
func sinkTimeout(cfg *config.Config) time.Duration {
	timeout := cfg.Poll.Interval / 2
	if timeout > 30*time.Second {
		timeout = 30 * time.Second
	}
	if timeout < time.Second {
		timeout = time.Second
	}
	return timeout
}

// The helpers below avoid handing a typed nil to an interface parameter, which
// would produce a non-nil interface holding a nil pointer and panic on use.

func observerOrNil(m *prommetrics.Sink) sink.Observer {
	if m == nil {
		return nil
	}
	return m
}

func recorderOrNil(m *prommetrics.Sink) poller.Recorder {
	if m == nil {
		return nil
	}
	return m
}

func metricsHandler(m *prommetrics.Sink) http.Handler {
	if m == nil {
		return nil
	}
	return m.Handler()
}

func metricsPath(m *prommetrics.Sink) string {
	if m == nil {
		return ""
	}
	return m.Path()
}
