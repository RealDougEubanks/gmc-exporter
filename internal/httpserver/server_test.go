package httpserver

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/RealDougEubanks/gmc-exporter/internal/config"
	"github.com/RealDougEubanks/gmc-exporter/internal/poller"
)

// fakeStatus is a scriptable StatusProvider.
type fakeStatus struct {
	status poller.Status
	age    time.Duration
	ever   bool
}

func (f *fakeStatus) Status() poller.Status { return f.status }

func (f *fakeStatus) SinceLastSuccess() (time.Duration, bool) { return f.age, f.ever }

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// healthyStatus is a connected device with a recent successful reading.
func healthyStatus() *fakeStatus {
	return &fakeStatus{
		status: poller.Status{
			Connected:   true,
			Device:      poller.DeviceInfo{Model: "GMC-320Re 4.62", Firmware: "4.62", Serial: "f628c40009888c"},
			Calibration: "60cpm=0.39uSv/h",
		},
		age:  5 * time.Second,
		ever: true,
	}
}

func newTestServer(t *testing.T, status StatusProvider, stale time.Duration) http.Handler {
	t.Helper()
	return New(Options{
		Config:     config.HTTP{Addr: ":0", ReadTimeout: time.Second, ShutdownTimeout: time.Second},
		StaleAfter: stale,
		Status:     status,
		Build:      BuildInfo{Version: "1.2.3", Commit: "abc1234", Built: "2026-01-02T03:04:05Z"},
		SinkNames:  []string{"prometheus", "mqtt"},
		Log:        discardLogger(),
	}).Handler()
}

func get(t *testing.T, h http.Handler, path string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
	return rec
}

func decode[T any](t *testing.T, rec *httptest.ResponseRecorder) T {
	t.Helper()
	var out T
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decoding response %q: %v", rec.Body.String(), err)
	}
	return out
}

// assertJSONHeaders checks the headers every endpoint must set. Health state is
// live and instance-specific, so a cached answer is a wrong answer.
func assertJSONHeaders(t *testing.T, rec *httptest.ResponseRecorder) {
	t.Helper()
	if got := rec.Header().Get("Cache-Control"); got != "no-store" {
		t.Fatalf("Cache-Control = %q, want no-store", got)
	}
	if got := rec.Header().Get("Content-Type"); !strings.HasPrefix(got, "application/json") {
		t.Fatalf("Content-Type = %q, want a JSON content type", got)
	}
}

// TestLivenessIgnoresBackendState is deliberate behaviour, not an oversight:
// liveness that fails on a backend outage turns the outage into a restart loop.
func TestLivenessIgnoresBackendState(t *testing.T) {
	cases := []struct {
		name   string
		status StatusProvider
	}{
		{"no status provider", nil},
		{"healthy", healthyStatus()},
		{"disconnected device", &fakeStatus{status: poller.Status{Connected: false}}},
		{"no reading ever", &fakeStatus{status: poller.Status{Connected: true}}},
		{"stale readings", &fakeStatus{
			status: poller.Status{Connected: true},
			age:    72 * time.Hour,
			ever:   true,
		}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newTestServer(t, tc.status, time.Minute)
			rec := get(t, h, "/healthz")
			if rec.Code != http.StatusOK {
				t.Fatalf("/healthz = %d, want 200 even when %s", rec.Code, tc.name)
			}
			assertJSONHeaders(t, rec)
			body := decode[map[string]string](t, rec)
			if body["status"] != "ok" {
				t.Fatalf("/healthz body = %v, want status ok", body)
			}
		})
	}
}

func TestReadiness(t *testing.T) {
	cases := []struct {
		name       string
		status     StatusProvider
		stale      time.Duration
		wantCode   int
		wantReason string
	}{
		{
			name:     "connected and fresh",
			status:   healthyStatus(),
			stale:    time.Minute,
			wantCode: http.StatusOK,
		},
		{
			name:       "device disconnected",
			status:     &fakeStatus{status: poller.Status{Connected: false}, age: time.Second, ever: true},
			stale:      time.Minute,
			wantCode:   http.StatusServiceUnavailable,
			wantReason: "device is not connected",
		},
		{
			name:       "no reading yet",
			status:     &fakeStatus{status: poller.Status{Connected: true}},
			stale:      time.Minute,
			wantCode:   http.StatusServiceUnavailable,
			wantReason: "no successful reading yet",
		},
		{
			name:       "readings are stale",
			status:     &fakeStatus{status: poller.Status{Connected: true}, age: 10 * time.Minute, ever: true},
			stale:      time.Minute,
			wantCode:   http.StatusServiceUnavailable,
			wantReason: "last successful reading was 10m0s ago, threshold is 1m0s",
		},
		{
			// A zero threshold disables the staleness check entirely, so an
			// old reading must not fail readiness.
			name:     "staleness check disabled",
			status:   &fakeStatus{status: poller.Status{Connected: true}, age: 10 * time.Minute, ever: true},
			stale:    0,
			wantCode: http.StatusOK,
		},
		{
			// With no poller wired in there is nothing to be unready about.
			name:     "no status provider",
			status:   nil,
			stale:    time.Minute,
			wantCode: http.StatusOK,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newTestServer(t, tc.status, tc.stale)
			rec := get(t, h, "/readyz")
			if rec.Code != tc.wantCode {
				t.Fatalf("/readyz = %d, want %d (body %s)", rec.Code, tc.wantCode, rec.Body.String())
			}
			assertJSONHeaders(t, rec)

			body := decode[map[string]string](t, rec)
			if tc.wantCode == http.StatusOK {
				if body["status"] != "ready" {
					t.Fatalf("/readyz body = %v, want status ready", body)
				}
				return
			}
			if body["status"] != "not ready" {
				t.Fatalf("/readyz body = %v, want status \"not ready\"", body)
			}
			if body["reason"] != tc.wantReason {
				t.Fatalf("/readyz reason = %q, want %q", body["reason"], tc.wantReason)
			}
		})
	}
}

func TestHealthWhenHealthy(t *testing.T) {
	h := newTestServer(t, healthyStatus(), time.Minute)
	rec := get(t, h, "/health")
	if rec.Code != http.StatusOK {
		t.Fatalf("/health = %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
	assertJSONHeaders(t, rec)

	var resp healthResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decoding /health: %v", err)
	}

	if resp.Status != "ok" {
		t.Fatalf("status = %q, want ok", resp.Status)
	}
	if resp.Build.Version != "1.2.3" || resp.Build.Commit != "abc1234" {
		t.Fatalf("build = %+v, want the configured build info", resp.Build)
	}
	if len(resp.Sinks) != 2 || resp.Sinks[0] != "prometheus" {
		t.Fatalf("configuredSinks = %v, want [prometheus mqtt]", resp.Sinks)
	}
	if !resp.Device.Connected {
		t.Fatal("device reported as disconnected")
	}
	if resp.Device.Model != "GMC-320Re 4.62" || resp.Device.Firmware != "4.62" {
		t.Fatalf("device = %+v, want the model and firmware from the status", resp.Device)
	}
	if resp.Device.LastReadingAgeS == nil || *resp.Device.LastReadingAgeS != 5 {
		t.Fatalf("lastReadingAgeSeconds = %v, want 5", resp.Device.LastReadingAgeS)
	}
	if len(resp.Dependencies) != 1 {
		t.Fatalf("dependencies = %v, want exactly one", resp.Dependencies)
	}
	dep := resp.Dependencies[0]
	if dep.Name != "geiger-counter" || dep.Status != "ok" {
		t.Fatalf("dependency = %+v, want geiger-counter ok", dep)
	}
	if _, err := time.Parse(time.RFC3339, dep.CheckedAt); err != nil {
		t.Fatalf("checkedAt %q is not RFC3339: %v", dep.CheckedAt, err)
	}
	if dep.LatencyMS < 0 {
		t.Fatalf("latencyMs = %d, want a non-negative value", dep.LatencyMS)
	}
}

func TestHealthUnhealthyStates(t *testing.T) {
	cases := []struct {
		name          string
		status        StatusProvider
		stale         time.Duration
		wantCode      int
		wantDepStatus string
		wantDetail    string
	}{
		{
			name:          "disconnected",
			status:        &fakeStatus{status: poller.Status{Connected: false}},
			stale:         time.Minute,
			wantCode:      http.StatusServiceUnavailable,
			wantDepStatus: "fail",
			wantDetail:    "serial port is not open",
		},
		{
			name:          "no reading yet",
			status:        &fakeStatus{status: poller.Status{Connected: true}},
			stale:         time.Minute,
			wantCode:      http.StatusServiceUnavailable,
			wantDepStatus: "degraded",
			wantDetail:    "no successful reading yet",
		},
		{
			name:          "stale reading",
			status:        &fakeStatus{status: poller.Status{Connected: true}, age: time.Hour, ever: true},
			stale:         time.Minute,
			wantCode:      http.StatusServiceUnavailable,
			wantDepStatus: "degraded",
			wantDetail:    "readings are stale",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newTestServer(t, tc.status, tc.stale)
			rec := get(t, h, "/health")
			if rec.Code != tc.wantCode {
				t.Fatalf("/health = %d, want %d", rec.Code, tc.wantCode)
			}
			assertJSONHeaders(t, rec)

			var resp healthResponse
			if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
				t.Fatalf("decoding /health: %v", err)
			}
			if resp.Status != "unhealthy" {
				t.Fatalf("status = %q, want unhealthy", resp.Status)
			}
			if len(resp.Dependencies) != 1 {
				t.Fatalf("dependencies = %v, want exactly one", resp.Dependencies)
			}
			if got := resp.Dependencies[0]; got.Status != tc.wantDepStatus || got.Detail != tc.wantDetail {
				t.Fatalf("dependency = %+v, want status %q detail %q", got, tc.wantDepStatus, tc.wantDetail)
			}
			if resp.Device.LastError == "" {
				t.Fatal("lastError is empty; the readiness reason should have filled it in")
			}
		})
	}
}

// TestHealthMatchesReadinessCode keeps the two endpoints from drifting apart:
// an external monitor alerting on /health's status code must reach the same
// verdict as an orchestrator probing /readyz.
func TestHealthMatchesReadinessCode(t *testing.T) {
	states := []StatusProvider{
		healthyStatus(),
		&fakeStatus{status: poller.Status{Connected: false}},
		&fakeStatus{status: poller.Status{Connected: true}},
		&fakeStatus{status: poller.Status{Connected: true}, age: time.Hour, ever: true},
	}

	for i, st := range states {
		h := newTestServer(t, st, time.Minute)
		health := get(t, h, "/health").Code
		ready := get(t, h, "/readyz").Code
		if health != ready {
			t.Fatalf("state %d: /health = %d but /readyz = %d", i, health, ready)
		}
	}
}

// TestHealthPreservesDeviceLastError checks that a real device error is not
// overwritten by the generic readiness reason, which would hide the useful
// detail an operator actually needs.
func TestHealthPreservesDeviceLastError(t *testing.T) {
	st := &fakeStatus{status: poller.Status{
		Connected:     false,
		LastErrorText: "read /dev/ttyUSB0: input/output error",
	}}
	h := newTestServer(t, st, time.Minute)

	var resp healthResponse
	if err := json.Unmarshal(get(t, h, "/health").Body.Bytes(), &resp); err != nil {
		t.Fatalf("decoding /health: %v", err)
	}
	if resp.Device.LastError != "read /dev/ttyUSB0: input/output error" {
		t.Fatalf("lastError = %q, want the device's own error", resp.Device.LastError)
	}
}

// TestHealthLeaksNoSecrets is the important one. /health is unauthenticated so
// external monitors can reach it, which means it must not double as a
// reconnaissance tool: no broker hostnames, no URLs, no credentials.
func TestHealthLeaksNoSecrets(t *testing.T) {
	const (
		password  = "hunter2-super-secret"
		token     = "influx-token-abcdef123456"
		brokerURL = "mqtts://broker.internal.example.com:8883"
		influxURL = "https://influx.internal.example.com:8086/write?db=rad&u=doug&p=hunter2"
		accountID = "1234567890"
	)

	// The status carries credential-shaped values in the places a careless
	// change would most plausibly put them.
	st := &fakeStatus{
		status: poller.Status{
			Connected: true,
			Device: poller.DeviceInfo{
				Model:    "GMC-320Re 4.62",
				Firmware: "4.62",
				Serial:   "f628c40009888c",
			},
			Calibration: "60cpm=0.39uSv/h",
		},
		age:  time.Second,
		ever: true,
	}

	h := New(Options{
		Config: config.HTTP{
			Addr:            "10.0.0.7:9101",
			ReadTimeout:     time.Second,
			ShutdownTimeout: time.Second,
		},
		StaleAfter: time.Minute,
		Status:     st,
		Build:      BuildInfo{Version: "1.2.3", Commit: "abc1234", Built: "2026-01-02T03:04:05Z"},
		// Sink names are published; they must remain names, never endpoints.
		SinkNames: []string{"prometheus", "mqtt", "influxv2"},
		Log:       discardLogger(),
	}).Handler()

	forbidden := []string{password, token, brokerURL, influxURL, accountID, "hunter2", "broker.internal.example.com", "influx.internal.example.com", "10.0.0.7"}

	for _, path := range []string{"/health", "/healthz", "/readyz", "/"} {
		body := get(t, h, path).Body.String()
		for _, secret := range forbidden {
			if strings.Contains(body, secret) {
				t.Fatalf("%s response leaked %q:\n%s", path, secret, body)
			}
		}
	}
}

// TestHealthLeaksNoSecretsInDisconnectedState covers the failure path, where
// free text from elsewhere in the process reaches the response body.
func TestHealthLeaksNoSecretsInDisconnectedState(t *testing.T) {
	h := newTestServer(t, &fakeStatus{status: poller.Status{Connected: false}}, time.Minute)
	body := get(t, h, "/health").Body.String()
	for _, forbidden := range []string{"password", "token", "://", "@"} {
		if strings.Contains(body, forbidden) {
			t.Fatalf("/health response contains %q, which suggests a URL or credential leaked:\n%s", forbidden, body)
		}
	}
}

func TestRootIndex(t *testing.T) {
	h := newTestServer(t, healthyStatus(), time.Minute)
	rec := get(t, h, "/")
	if rec.Code != http.StatusOK {
		t.Fatalf("/ = %d, want 200", rec.Code)
	}
	assertJSONHeaders(t, rec)

	body := decode[map[string]any](t, rec)
	if body["service"] != "gmc-exporter" {
		t.Fatalf("service = %v, want gmc-exporter", body["service"])
	}
	endpoints, ok := body["endpoints"].([]any)
	if !ok || len(endpoints) != 4 {
		t.Fatalf("endpoints = %v, want the four documented paths", body["endpoints"])
	}
}

func TestUnknownPathIs404(t *testing.T) {
	h := newTestServer(t, healthyStatus(), time.Minute)
	for _, path := range []string{"/nope", "/health/extra", "/metrics"} {
		rec := get(t, h, path)
		if rec.Code != http.StatusNotFound {
			t.Fatalf("%s = %d, want 404", path, rec.Code)
		}
	}
}

func TestMetricsHandlerIsMountedWhenSupplied(t *testing.T) {
	metrics := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("# HELP gmc_cpm\n"))
	})

	h := New(Options{
		Config:         config.HTTP{Addr: ":0", ReadTimeout: time.Second},
		Status:         healthyStatus(),
		MetricsHandler: metrics,
		MetricsPath:    "/custom-metrics",
		Log:            discardLogger(),
	}).Handler()

	if rec := get(t, h, "/custom-metrics"); rec.Code != http.StatusOK {
		t.Fatalf("/custom-metrics = %d, want 200", rec.Code)
	}

	// An empty path must fall back to the conventional /metrics rather than
	// failing to register.
	def := New(Options{
		Config:         config.HTTP{Addr: ":0", ReadTimeout: time.Second},
		Status:         healthyStatus(),
		MetricsHandler: metrics,
		Log:            discardLogger(),
	}).Handler()
	if rec := get(t, def, "/metrics"); rec.Code != http.StatusOK {
		t.Fatalf("/metrics = %d, want 200", rec.Code)
	}
}

// TestServeOverRealListener exercises the handler through a real HTTP round
// trip, so header and status handling are checked against net/http rather than
// only against the recorder.
func TestServeOverRealListener(t *testing.T) {
	srv := httptest.NewServer(newTestServer(t, healthyStatus(), time.Minute))
	defer srv.Close()

	resp, err := srv.Client().Get(srv.URL + "/readyz")
	if err != nil {
		t.Fatalf("GET /readyz: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("/readyz = %d, want 200", resp.StatusCode)
	}
	if got := resp.Header.Get("Cache-Control"); got != "no-store" {
		t.Fatalf("Cache-Control = %q, want no-store", got)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("reading body: %v", err)
	}
	if !strings.Contains(string(body), `"ready"`) {
		t.Fatalf("body = %q, want a ready status", body)
	}
}

// TestRunShutsDownCleanly checks the lifecycle the process depends on: Run
// returns nil once the context is cancelled, so a signal produces a clean exit
// rather than an error.
func TestRunShutsDownCleanly(t *testing.T) {
	s := New(Options{
		Config: config.HTTP{Addr: "127.0.0.1:0", ReadTimeout: time.Second, ShutdownTimeout: 5 * time.Second},
		Status: healthyStatus(),
		Log:    discardLogger(),
	})

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- s.Run(ctx) }()

	// Give the listener a moment to come up before asking it to stop.
	time.Sleep(50 * time.Millisecond)
	cancel()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run = %v, want nil on a cancelled context", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return after the context was cancelled")
	}
}

// TestRunReportsListenFailure proves a port that cannot be bound surfaces as an
// error rather than a silently dead server.
func TestRunReportsListenFailure(t *testing.T) {
	s := New(Options{
		Config: config.HTTP{Addr: "127.0.0.1:-1", ReadTimeout: time.Second, ShutdownTimeout: time.Second},
		Status: healthyStatus(),
		Log:    discardLogger(),
	})

	err := s.Run(context.Background())
	if err == nil {
		t.Fatal("Run = nil, want an error for an unbindable address")
	}
	if !strings.Contains(err.Error(), "http server") {
		t.Fatalf("Run error = %v, want it to name the http server", err)
	}
}

// TestNewWithoutLoggerDoesNotPanic covers the nil-logger default; New is called
// from main with whatever the caller supplies.
func TestNewWithoutLoggerDoesNotPanic(t *testing.T) {
	s := New(Options{Config: config.HTTP{Addr: ":0"}})
	if s.log == nil {
		t.Fatal("logger is nil after New")
	}
	if rec := get(t, s.Handler(), "/healthz"); rec.Code != http.StatusOK {
		t.Fatalf("/healthz = %d, want 200", rec.Code)
	}
}
