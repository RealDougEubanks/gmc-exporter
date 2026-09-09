package prommetrics

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"runtime"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/RealDougEubanks/gmc-exporter/internal/config"
	"github.com/RealDougEubanks/gmc-exporter/internal/reading"
	"github.com/RealDougEubanks/gmc-exporter/internal/sink"
)

// gather collects the registry and flattens it to series name plus sorted
// labels against a value, which is what the assertions actually care about.
//
// A counter and a gauge contribute their value; a histogram contributes its
// sample count, since the point of asserting on it is that an observation was
// recorded against the right sink.
func gather(t *testing.T, s *Sink) map[string]float64 {
	t.Helper()

	families, err := s.Registry().Gather()
	if err != nil {
		t.Fatalf("Gather returned %v", err)
	}

	out := make(map[string]float64)
	for _, family := range families {
		for _, m := range family.GetMetric() {
			parts := make([]string, 0, len(m.GetLabel()))
			for _, l := range m.GetLabel() {
				parts = append(parts, fmt.Sprintf("%s=%q", l.GetName(), l.GetValue()))
			}
			sort.Strings(parts)

			key := family.GetName()
			if len(parts) > 0 {
				key += "{" + strings.Join(parts, ",") + "}"
			}

			switch {
			case m.GetGauge() != nil:
				out[key] = m.GetGauge().GetValue()
			case m.GetCounter() != nil:
				out[key] = m.GetCounter().GetValue()
			case m.GetHistogram() != nil:
				out[key] = float64(m.GetHistogram().GetSampleCount())
			default:
				t.Fatalf("metric %s has an unexpected type", key)
			}
		}
	}
	return out
}

// assertSeries checks a series is present with an exact value.
func assertSeries(t *testing.T, got map[string]float64, key string, want float64) {
	t.Helper()
	v, ok := got[key]
	if !ok {
		t.Errorf("series %s is absent, want value %v", key, want)
		return
	}
	if v != want {
		t.Errorf("series %s is %v, want %v", key, v, want)
	}
}

// assertAbsent checks no series exists with the given name, whatever its
// labels.
func assertAbsent(t *testing.T, got map[string]float64, name string) {
	t.Helper()
	for key := range got {
		if key == name || strings.HasPrefix(key, name+"{") {
			t.Errorf("series %s is present with value %v, want absent", key, got[key])
		}
	}
}

// newTestSink builds a sink with a fixed build stamp so gmc_build_info is
// predictable.
func newTestSink(t *testing.T) *Sink {
	t.Helper()
	return New(config.Prometheus{Enabled: true, Path: "/metrics"},
		Build{Version: "1.2.3", Commit: "abc1234"})
}

// fullReading is a reading with every optional field supplied.
func fullReading() reading.Reading {
	return reading.Reading{
		Timestamp:            time.Unix(1700000000, 0).UTC(),
		CPM:                  42,
		AverageCPM:           37.5,
		MicroSievertsPerHour: 0.273,
		Voltage:              reading.Some(4.7),
		TemperatureC:         reading.Some(21.5),
	}
}

// scrape fetches the exposition text through a real HTTP server.
func scrape(t *testing.T, s *Sink) (int, string) {
	t.Helper()

	srv := httptest.NewServer(s.Handler())
	defer srv.Close()

	resp, err := http.Get(srv.URL + s.Path())
	if err != nil {
		t.Fatalf("GET returned %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("reading body returned %v", err)
	}
	return resp.StatusCode, string(body)
}

func TestPublishExposesEveryReadingMetric(t *testing.T) {
	s := newTestSink(t)

	if err := s.Publish(context.Background(), fullReading()); err != nil {
		t.Fatalf("Publish returned %v, want nil", err)
	}

	got := gather(t, s)
	assertSeries(t, got, "gmc_cpm", 42)
	assertSeries(t, got, "gmc_usv_per_hour", 0.273)
	assertSeries(t, got, "gmc_average_cpm", 37.5)
	assertSeries(t, got, "gmc_battery_volts", 4.7)
	assertSeries(t, got, "gmc_temperature_celsius", 21.5)
}

func TestPublishOmitsOptionalMetricsWhenNotValid(t *testing.T) {
	s := newTestSink(t)

	r := fullReading()
	r.Voltage = reading.None[float64]()
	r.TemperatureC = reading.None[float64]()

	if err := s.Publish(context.Background(), r); err != nil {
		t.Fatalf("Publish returned %v, want nil", err)
	}

	got := gather(t, s)
	assertAbsent(t, got, "gmc_battery_volts")
	assertAbsent(t, got, "gmc_temperature_celsius")

	// The mandatory metrics must still be there, so the absence is specific to
	// the optional fields rather than a collector that stopped reporting.
	assertSeries(t, got, "gmc_cpm", 42)
	assertSeries(t, got, "gmc_average_cpm", 37.5)
}

func TestPublishDropsAnOptionalThatStopsBeingSupplied(t *testing.T) {
	s := newTestSink(t)

	if err := s.Publish(context.Background(), fullReading()); err != nil {
		t.Fatalf("Publish returned %v, want nil", err)
	}

	r := fullReading()
	r.TemperatureC = reading.None[float64]()
	if err := s.Publish(context.Background(), r); err != nil {
		t.Fatalf("Publish returned %v, want nil", err)
	}

	// The earlier temperature must not linger. A stale value reported as
	// current is worse than no value at all.
	assertAbsent(t, gather(t, s), "gmc_temperature_celsius")
}

func TestReadingMetricsAbsentBeforeFirstPublish(t *testing.T) {
	got := gather(t, newTestSink(t))

	for _, name := range []string{
		"gmc_cpm", "gmc_usv_per_hour", "gmc_average_cpm",
		"gmc_battery_volts", "gmc_temperature_celsius",
	} {
		assertAbsent(t, got, name)
	}
}

func TestObservePublishCountsOutcomesPerSink(t *testing.T) {
	s := newTestSink(t)

	s.ObservePublish("influx2", 12*time.Millisecond, nil)
	s.ObservePublish("influx2", 9*time.Millisecond, nil)
	s.ObservePublish("influx2", 30*time.Millisecond, errors.New("connection refused"))
	s.ObservePublish("safecast", time.Millisecond, fmt.Errorf("no temperature: %w", sink.ErrSkipped))

	got := gather(t, s)
	assertSeries(t, got, `gmc_sink_publish_total{result="success",sink="influx2"}`, 2)
	assertSeries(t, got, `gmc_sink_publish_total{result="failure",sink="influx2"}`, 1)
	assertSeries(t, got, `gmc_sink_publish_total{result="skipped",sink="safecast"}`, 1)

	// A skipped publish must not be counted as a failure: a sink that had
	// nothing to send behaved correctly.
	assertAbsent(t, got, "gmc_sink_publish_total_unexpected")
	if _, ok := got[`gmc_sink_publish_total{result="failure",sink="safecast"}`]; ok {
		t.Error("a skipped publish was counted as a failure")
	}

	assertSeries(t, got, `gmc_sink_publish_duration_seconds{sink="influx2"}`, 3)
	assertSeries(t, got, `gmc_sink_publish_duration_seconds{sink="safecast"}`, 1)
}

func TestObservePublishWorksThroughTheSinkObserverInterface(t *testing.T) {
	s := newTestSink(t)

	// The fan-out only ever holds a sink.Observer, so the concrete type has to
	// be usable through that interface with no cast.
	var observer sink.Observer = s
	observer.ObservePublish("mqtt", 5*time.Millisecond, nil)

	assertSeries(t, gather(t, s), `gmc_sink_publish_total{result="success",sink="mqtt"}`, 1)
}

func TestRecordPollSuccessCountsAndTimestamps(t *testing.T) {
	s := newTestSink(t)

	s.RecordPollSuccess(time.Unix(1700000000, 0))
	s.RecordPollSuccess(time.Unix(1700000060, 500000000))

	got := gather(t, s)
	assertSeries(t, got, "gmc_poll_success_total", 2)
	assertSeries(t, got, "gmc_last_successful_read_timestamp_seconds", 1700000060.5)
}

func TestRecordPollFailureLeavesTheReadTimestampAlone(t *testing.T) {
	s := newTestSink(t)

	s.RecordPollSuccess(time.Unix(1700000000, 0))
	s.RecordPollFailure()
	s.RecordPollFailure()

	got := gather(t, s)
	assertSeries(t, got, "gmc_poll_failure_total", 2)
	assertSeries(t, got, "gmc_poll_success_total", 1)
	assertSeries(t, got, "gmc_last_successful_read_timestamp_seconds", 1700000000)
}

func TestBuildInfoIsAConstantOne(t *testing.T) {
	s := newTestSink(t)

	key := fmt.Sprintf(`gmc_build_info{commit="abc1234",goversion=%q,version="1.2.3"}`, runtime.Version())
	assertSeries(t, gather(t, s), key, 1)
}

func TestBuildInfoFallsBackWhenUnstamped(t *testing.T) {
	s := New(config.Prometheus{}, Build{})

	key := fmt.Sprintf(`gmc_build_info{commit="unknown",goversion=%q,version="unknown"}`, runtime.Version())
	assertSeries(t, gather(t, s), key, 1)
}

func TestRegistryIsIsolatedFromTheDefaultOne(t *testing.T) {
	got := gather(t, newTestSink(t))

	// The Go runtime and process collectors live on the default registry.
	// Their absence is what proves the scrape carries only this exporter's
	// metrics.
	for _, name := range []string{"go_goroutines", "process_open_fds", "promhttp_metric_handler_requests_total"} {
		assertAbsent(t, got, name)
	}
}

func TestHandlerServesTheMetrics(t *testing.T) {
	s := newTestSink(t)
	if err := s.Publish(context.Background(), fullReading()); err != nil {
		t.Fatalf("Publish returned %v, want nil", err)
	}
	s.RecordPollSuccess(time.Unix(1700000000, 0))
	s.ObservePublish("radmon", 20*time.Millisecond, nil)

	status, page := scrape(t, s)
	if status != http.StatusOK {
		t.Fatalf("status %d, want %d", status, http.StatusOK)
	}

	for _, line := range []string{
		"# TYPE gmc_cpm gauge",
		"gmc_cpm 42",
		"gmc_usv_per_hour 0.273",
		"gmc_average_cpm 37.5",
		"gmc_battery_volts 4.7",
		"gmc_temperature_celsius 21.5",
		"gmc_last_successful_read_timestamp_seconds 1.7e+09",
		"gmc_poll_success_total 1",
		"gmc_poll_failure_total 0",
		`gmc_sink_publish_total{result="success",sink="radmon"} 1`,
		`gmc_sink_publish_duration_seconds_count{sink="radmon"} 1`,
		"gmc_build_info{",
	} {
		if !strings.Contains(page, line) {
			t.Errorf("scrape output is missing %q\n%s", line, page)
		}
	}
}

func TestHandlerOmitsOptionalMetricsWhenNotValid(t *testing.T) {
	s := newTestSink(t)

	r := fullReading()
	r.TemperatureC = reading.None[float64]()
	if err := s.Publish(context.Background(), r); err != nil {
		t.Fatalf("Publish returned %v, want nil", err)
	}

	_, page := scrape(t, s)
	if strings.Contains(page, "gmc_temperature_celsius") {
		t.Errorf("scrape output carries gmc_temperature_celsius for an invalid Optional\n%s", page)
	}
}

func TestConcurrentPublishObserveAndScrapeAreRaceFree(t *testing.T) {
	s := newTestSink(t)

	srv := httptest.NewServer(s.Handler())
	defer srv.Close()

	const workers = 8
	const iterations = 50

	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(worker int) {
			defer wg.Done()
			for j := 0; j < iterations; j++ {
				r := fullReading()
				r.CPM = uint16(worker*iterations + j)
				// Alternate validity so the collector is exercised both with
				// and without the optional fields while being scraped.
				if j%2 == 0 {
					r.TemperatureC = reading.None[float64]()
				}
				if err := s.Publish(context.Background(), r); err != nil {
					t.Errorf("Publish returned %v, want nil", err)
					return
				}
				s.ObservePublish("influx2", time.Millisecond, nil)
				s.RecordPollSuccess(time.Unix(int64(1700000000+j), 0))
				s.RecordPollFailure()
			}
		}(i)
	}

	wg.Add(1)
	go func() {
		defer wg.Done()
		for j := 0; j < iterations; j++ {
			resp, err := http.Get(srv.URL + s.Path())
			if err != nil {
				t.Errorf("GET returned %v", err)
				return
			}
			_, _ = io.Copy(io.Discard, resp.Body)
			_ = resp.Body.Close()
		}
	}()

	wg.Wait()

	got := gather(t, s)
	assertSeries(t, got, `gmc_sink_publish_total{result="success",sink="influx2"}`, workers*iterations)
	assertSeries(t, got, "gmc_poll_success_total", workers*iterations)
	assertSeries(t, got, "gmc_poll_failure_total", workers*iterations)
}

func TestSinkIdentityAndClose(t *testing.T) {
	s := newTestSink(t)

	if s.Name() != Name {
		t.Errorf("Name is %q, want %q", s.Name(), Name)
	}
	if err := s.Close(); err != nil {
		t.Errorf("Close returned %v, want nil", err)
	}
	// Close must be safe on a sink that never published.
	if err := New(config.Prometheus{}, Build{}).Close(); err != nil {
		t.Errorf("Close on an unused sink returned %v, want nil", err)
	}
}

func TestPublishHonoursContextCancellationWithoutFailing(t *testing.T) {
	s := newTestSink(t)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	// This sink does no I/O, so a cancelled context is not a reason to drop the
	// value: recording it costs nothing and keeps the scrape current.
	if err := s.Publish(ctx, fullReading()); err != nil {
		t.Errorf("Publish returned %v, want nil", err)
	}
	assertSeries(t, gather(t, s), "gmc_cpm", 42)
}

func TestPathDefaultsWhenUnset(t *testing.T) {
	if got := New(config.Prometheus{}, Build{}).Path(); got != DefaultPath {
		t.Errorf("Path is %q, want %q", got, DefaultPath)
	}
	if got := New(config.Prometheus{Path: "/gmc"}, Build{}).Path(); got != "/gmc" {
		t.Errorf("Path is %q, want %q", got, "/gmc")
	}
}
