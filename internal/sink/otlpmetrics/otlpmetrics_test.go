package otlpmetrics

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	collectorpb "go.opentelemetry.io/proto/otlp/collector/metrics/v1"
	"google.golang.org/protobuf/proto"

	"github.com/RealDougEubanks/gmc-exporter/internal/config"
	"github.com/RealDougEubanks/gmc-exporter/internal/reading"
	"github.com/RealDougEubanks/gmc-exporter/internal/sink"
)

// unreachableEndpoint is a port nothing listens on. Port 1 is privileged and
// unused, so a connection there fails immediately rather than hanging.
const unreachableEndpoint = "127.0.0.1:1"

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

// export is one request the fake collector received.
type export struct {
	headers http.Header
	request *collectorpb.ExportMetricsServiceRequest
}

// fakeCollector serves the OTLP/HTTP metrics endpoint and records what it was
// sent. It stands in for Alloy, so no live collector is needed.
func fakeCollector(t *testing.T) (*httptest.Server, <-chan export) {
	t.Helper()

	// Buffered so the exporter never blocks on a test that stops reading.
	got := make(chan export, 8)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}

		req := &collectorpb.ExportMetricsServiceRequest{}
		if err := proto.Unmarshal(body, req); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}

		select {
		case got <- export{headers: r.Header.Clone(), request: req}:
		default:
		}

		resp, err := proto.Marshal(&collectorpb.ExportMetricsServiceResponse{})
		if err != nil {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/x-protobuf")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(resp)
	}))
	t.Cleanup(srv.Close)

	return srv, got
}

// waitForExport returns the next export or fails the test.
func waitForExport(t *testing.T, ch <-chan export) export {
	t.Helper()
	select {
	case e := <-ch:
		return e
	case <-time.After(10 * time.Second):
		t.Fatal("the collector received no export")
		return export{}
	}
}

// metricValues flattens an export to metric name against its first gauge data
// point.
func metricValues(t *testing.T, e export) map[string]float64 {
	t.Helper()

	out := make(map[string]float64)
	for _, rm := range e.request.GetResourceMetrics() {
		for _, sm := range rm.GetScopeMetrics() {
			for _, m := range sm.GetMetrics() {
				points := m.GetGauge().GetDataPoints()
				if len(points) == 0 {
					t.Errorf("metric %s carries no data points", m.GetName())
					continue
				}
				out[m.GetName()] = points[0].GetAsDouble()
			}
		}
	}
	return out
}

// resourceAttributes flattens the resource attributes of an export.
func resourceAttributes(e export) map[string]string {
	out := make(map[string]string)
	for _, rm := range e.request.GetResourceMetrics() {
		for _, kv := range rm.GetResource().GetAttributes() {
			out[kv.GetKey()] = kv.GetValue().GetStringValue()
		}
	}
	return out
}

func TestNewSucceedsForBothProtocols(t *testing.T) {
	for _, protocol := range []string{ProtocolGRPC, ProtocolHTTP} {
		t.Run(protocol, func(t *testing.T) {
			s, err := New(config.OTLP{
				Enabled:  true,
				Protocol: protocol,
				Endpoint: unreachableEndpoint,
				Insecure: true,
				Timeout:  time.Second,
			}, "1.2.3")
			if err != nil {
				t.Fatalf("New returned %v, want nil", err)
			}
			defer func() { _ = s.Close() }()

			if s.Name() != Name {
				t.Errorf("Name is %q, want %q", s.Name(), Name)
			}

			// The sink must satisfy the interface the fan-out holds.
			var _ sink.Sink = s
		})
	}
}

func TestNewDoesNotFailWhenTheCollectorIsDown(t *testing.T) {
	// Nothing is listening on this endpoint, and nothing ever will be during
	// the test. Construction must still succeed: a collector that is
	// temporarily unreachable is a routine condition, not a reason to refuse to
	// start an unattended exporter.
	for _, protocol := range []string{ProtocolGRPC, ProtocolHTTP} {
		t.Run(protocol, func(t *testing.T) {
			done := make(chan error, 1)
			go func() {
				s, err := New(config.OTLP{
					Protocol: protocol,
					Endpoint: unreachableEndpoint,
					Insecure: true,
					Timeout:  time.Second,
				}, "1.2.3")
				if s != nil {
					_ = s.Close()
				}
				done <- err
			}()

			select {
			case err := <-done:
				if err != nil {
					t.Fatalf("New returned %v, want nil", err)
				}
			case <-time.After(10 * time.Second):
				t.Fatal("New blocked on an unreachable collector")
			}
		})
	}
}

func TestNewRejectsAnEmptyEndpoint(t *testing.T) {
	s, err := New(config.OTLP{Protocol: ProtocolGRPC}, "1.2.3")
	if err == nil {
		_ = s.Close()
		t.Fatal("New accepted an empty endpoint, want an error")
	}
}

func TestNewRejectsAnUnknownProtocol(t *testing.T) {
	s, err := New(config.OTLP{Protocol: "thrift", Endpoint: unreachableEndpoint}, "1.2.3")
	if err == nil {
		_ = s.Close()
		t.Fatal("New accepted an unknown protocol, want an error")
	}
}

func TestPublishDoesNotErrorWhenTheCollectorIsUnreachable(t *testing.T) {
	s, err := New(config.OTLP{
		Protocol: ProtocolGRPC,
		Endpoint: unreachableEndpoint,
		Insecure: true,
		Timeout:  time.Second,
	}, "1.2.3")
	if err != nil {
		t.Fatalf("New returned %v, want nil", err)
	}
	defer func() { _ = s.Close() }()

	// Publish records values and performs no I/O, so an unreachable collector
	// cannot fail a poll.
	for i := 0; i < 3; i++ {
		if err := s.Publish(context.Background(), fullReading()); err != nil {
			t.Fatalf("Publish returned %v, want nil", err)
		}
	}
}

func TestPublishReportsACancelledContext(t *testing.T) {
	s, err := New(config.OTLP{
		Protocol: ProtocolGRPC,
		Endpoint: unreachableEndpoint,
		Insecure: true,
		Timeout:  time.Second,
	}, "1.2.3")
	if err != nil {
		t.Fatalf("New returned %v, want nil", err)
	}
	defer func() { _ = s.Close() }()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if err := s.Publish(ctx, fullReading()); !errors.Is(err, context.Canceled) {
		t.Errorf("Publish returned %v, want context.Canceled", err)
	}
}

func TestCloseIsBoundedWhenTheCollectorIsUnreachable(t *testing.T) {
	const timeout = time.Second

	s, err := New(config.OTLP{
		Protocol: ProtocolGRPC,
		Endpoint: unreachableEndpoint,
		Insecure: true,
		Timeout:  timeout,
	}, "1.2.3")
	if err != nil {
		t.Fatalf("New returned %v, want nil", err)
	}

	if err := s.Publish(context.Background(), fullReading()); err != nil {
		t.Fatalf("Publish returned %v, want nil", err)
	}

	done := make(chan struct{})
	start := time.Now()
	go func() {
		defer close(done)
		// The error is not asserted on: a final export to a collector that is
		// not there may legitimately fail. What matters is that Close returns.
		_ = s.Close()
	}()

	select {
	case <-done:
	case <-time.After(10 * timeout):
		t.Fatal("Close did not return; shutdown is not bounded")
	}

	if elapsed := time.Since(start); elapsed > 5*timeout {
		t.Errorf("Close took %s, want it bounded near the %s timeout", elapsed, timeout)
	}
}

func TestCloseIsCleanAndIdempotent(t *testing.T) {
	srv, exports := fakeCollector(t)

	s, err := New(config.OTLP{
		Protocol: ProtocolHTTP,
		Endpoint: srv.URL,
		Insecure: true,
		Timeout:  5 * time.Second,
	}, "1.2.3")
	if err != nil {
		t.Fatalf("New returned %v, want nil", err)
	}

	if err := s.Publish(context.Background(), fullReading()); err != nil {
		t.Fatalf("Publish returned %v, want nil", err)
	}

	// With a collector answering, shutting down must both flush and report no
	// error.
	if err := s.Close(); err != nil {
		t.Errorf("Close returned %v, want nil", err)
	}
	waitForExport(t, exports)

	// A second Close is what happens when the fan-out closes a sink the poll
	// loop already closed, and it must not report a shutdown that already
	// happened as a failure.
	if err := s.Close(); err != nil {
		t.Errorf("second Close returned %v, want nil", err)
	}
}

func TestExportCarriesTheReadingValuesAndServiceResource(t *testing.T) {
	srv, exports := fakeCollector(t)

	s, err := New(config.OTLP{
		Protocol: ProtocolHTTP,
		Endpoint: srv.URL,
		Insecure: true,
		Timeout:  5 * time.Second,
	}, "9.9.9")
	if err != nil {
		t.Fatalf("New returned %v, want nil", err)
	}

	if err := s.Publish(context.Background(), fullReading()); err != nil {
		t.Fatalf("Publish returned %v, want nil", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("Close returned %v, want nil", err)
	}

	e := waitForExport(t, exports)

	attrs := resourceAttributes(e)
	if attrs["service.name"] != ServiceName {
		t.Errorf("service.name is %q, want %q", attrs["service.name"], ServiceName)
	}
	if attrs["service.version"] != "9.9.9" {
		t.Errorf("service.version is %q, want %q", attrs["service.version"], "9.9.9")
	}

	values := metricValues(t, e)
	for name, want := range map[string]float64{
		"gmc_cpm":                 42,
		"gmc_usv_per_hour":        0.273,
		"gmc_average_cpm":         37.5,
		"gmc_battery_volts":       4.7,
		"gmc_temperature_celsius": 21.5,
	} {
		got, ok := values[name]
		if !ok {
			t.Errorf("metric %s was not exported", name)
			continue
		}
		if got != want {
			t.Errorf("metric %s is %v, want %v", name, got, want)
		}
	}
}

func TestExportOmitsOptionalMetricsWhenNotValid(t *testing.T) {
	srv, exports := fakeCollector(t)

	s, err := New(config.OTLP{
		Protocol: ProtocolHTTP,
		Endpoint: srv.URL,
		Insecure: true,
		Timeout:  5 * time.Second,
	}, "1.2.3")
	if err != nil {
		t.Fatalf("New returned %v, want nil", err)
	}

	r := fullReading()
	r.Voltage = reading.None[float64]()
	r.TemperatureC = reading.None[float64]()
	if err := s.Publish(context.Background(), r); err != nil {
		t.Fatalf("Publish returned %v, want nil", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("Close returned %v, want nil", err)
	}

	values := metricValues(t, waitForExport(t, exports))
	for _, name := range []string{"gmc_battery_volts", "gmc_temperature_celsius"} {
		if v, ok := values[name]; ok {
			t.Errorf("metric %s was exported as %v for an invalid Optional, want omitted", name, v)
		}
	}
	if _, ok := values["gmc_cpm"]; !ok {
		t.Error("gmc_cpm was not exported")
	}
}

func TestHeadersAreSentToTheCollector(t *testing.T) {
	srv, exports := fakeCollector(t)

	s, err := New(config.OTLP{
		Protocol: ProtocolHTTP,
		Endpoint: srv.URL,
		Insecure: true,
		Headers:  map[string]string{"X-Scope-OrgID": "gmc", "Authorization": "Bearer token"},
		Timeout:  5 * time.Second,
	}, "1.2.3")
	if err != nil {
		t.Fatalf("New returned %v, want nil", err)
	}

	if err := s.Publish(context.Background(), fullReading()); err != nil {
		t.Fatalf("Publish returned %v, want nil", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("Close returned %v, want nil", err)
	}

	headers := waitForExport(t, exports).headers
	if got := headers.Get("X-Scope-OrgID"); got != "gmc" {
		t.Errorf("X-Scope-OrgID is %q, want %q", got, "gmc")
	}
	if got := headers.Get("Authorization"); got != "Bearer token" {
		t.Errorf("Authorization is %q, want %q", got, "Bearer token")
	}
}

func TestInsecureEndpointIsHonoured(t *testing.T) {
	// The fake collector serves plain HTTP. Reaching it at all proves the
	// insecure setting was threaded through, since a TLS handshake against a
	// plaintext listener would fail and nothing would arrive.
	srv, exports := fakeCollector(t)

	s, err := New(config.OTLP{
		Protocol: ProtocolHTTP,
		Endpoint: srv.Listener.Addr().String(),
		Insecure: true,
		Timeout:  5 * time.Second,
	}, "1.2.3")
	if err != nil {
		t.Fatalf("New returned %v, want nil", err)
	}

	if err := s.Publish(context.Background(), fullReading()); err != nil {
		t.Fatalf("Publish returned %v, want nil", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("Close returned %v, want nil", err)
	}

	waitForExport(t, exports)
}

func TestEndpointAcceptsBothBareHostAndURL(t *testing.T) {
	srv, exports := fakeCollector(t)

	for name, endpoint := range map[string]string{
		"bare host:port": srv.Listener.Addr().String(),
		"full URL":       srv.URL,
	} {
		t.Run(name, func(t *testing.T) {
			s, err := New(config.OTLP{
				Protocol: ProtocolHTTP,
				Endpoint: endpoint,
				Insecure: true,
				Timeout:  5 * time.Second,
			}, "1.2.3")
			if err != nil {
				t.Fatalf("New returned %v, want nil", err)
			}

			if err := s.Publish(context.Background(), fullReading()); err != nil {
				t.Fatalf("Publish returned %v, want nil", err)
			}
			if err := s.Close(); err != nil {
				t.Fatalf("Close returned %v, want nil", err)
			}

			waitForExport(t, exports)
		})
	}
}

func TestConcurrentPublishIsRaceFree(t *testing.T) {
	s, err := New(config.OTLP{
		Protocol: ProtocolGRPC,
		Endpoint: unreachableEndpoint,
		Insecure: true,
		Timeout:  time.Second,
	}, "1.2.3")
	if err != nil {
		t.Fatalf("New returned %v, want nil", err)
	}
	defer func() { _ = s.Close() }()

	done := make(chan struct{})
	for i := 0; i < 8; i++ {
		go func(worker int) {
			defer func() { done <- struct{}{} }()
			for j := 0; j < 100; j++ {
				r := fullReading()
				r.CPM = uint16(worker*100 + j)
				if j%2 == 0 {
					r.TemperatureC = reading.None[float64]()
				}
				if err := s.Publish(context.Background(), r); err != nil {
					t.Errorf("Publish returned %v, want nil", err)
					return
				}
			}
		}(i)
	}
	for i := 0; i < 8; i++ {
		<-done
	}
}
