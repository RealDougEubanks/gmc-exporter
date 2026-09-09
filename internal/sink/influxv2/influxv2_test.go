package influxv2

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/RealDougEubanks/gmc-exporter/internal/config"
	"github.com/RealDougEubanks/gmc-exporter/internal/reading"
	"github.com/RealDougEubanks/gmc-exporter/internal/redact"
	"github.com/RealDougEubanks/gmc-exporter/internal/sink"
)

// testToken is deliberately unlike anything else in a log line, so a test can
// assert its absence without matching something innocent.
const testToken = "canary-t0ken-do-not-log"

// sampleTime is a fixed instant so expected line protocol can be written out in
// full rather than recomputed by the test.
var sampleTime = time.Unix(1700000000, 0).UTC()

// A sink must satisfy the interface the fan-out publishes through.
var _ sink.Sink = (*Sink)(nil)

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func testConfig(serverURL string) config.InfluxV2 {
	return config.InfluxV2{
		Enabled:     true,
		URL:         serverURL,
		Token:       redact.New(testToken),
		Org:         "home",
		Bucket:      "geiger",
		Measurement: "radiation",
		Timeout:     2 * time.Second,
		Retries:     2,
	}
}

func sampleReading() reading.Reading {
	return reading.Reading{
		Timestamp:            sampleTime,
		CPM:                  42,
		AverageCPM:           12.5,
		MicroSievertsPerHour: 0.0812,
		Voltage:              reading.Some(4.2),
		TemperatureC:         reading.Some(21.5),
	}
}

func newSink(t *testing.T, cfg config.InfluxV2, log *slog.Logger) *Sink {
	t.Helper()
	s, err := New(cfg, log)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	// Retry pacing is not what these tests are measuring.
	s.backoff = time.Millisecond
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func TestPublishSendsLineProtocol(t *testing.T) {
	var (
		gotBody   string
		gotPath   string
		gotQuery  string
		gotMethod string
		gotAuth   string
		gotType   string
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		gotBody = string(body)
		gotPath = r.URL.Path
		gotQuery = r.URL.RawQuery
		gotMethod = r.Method
		gotAuth = r.Header.Get("Authorization")
		gotType = r.Header.Get("Content-Type")
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	s := newSink(t, testConfig(srv.URL), discardLogger())
	if err := s.Publish(context.Background(), sampleReading()); err != nil {
		t.Fatalf("Publish: %v", err)
	}

	want := "radiation cpm=42i,acpm=12.5,usvh=0.0812,volts=4.2,temp_c=21.5 1700000000000000000"
	if gotBody != want {
		t.Errorf("body:\n got %q\nwant %q", gotBody, want)
	}
	if gotMethod != http.MethodPost {
		t.Errorf("method = %q, want POST", gotMethod)
	}
	// The native 2.x endpoint, not the 1.x compatibility path.
	if gotPath != "/api/v2/write" {
		t.Errorf("path = %q, want /api/v2/write", gotPath)
	}
	if gotAuth != "Token "+testToken {
		t.Errorf("Authorization = %q, want %q", gotAuth, "Token "+testToken)
	}
	if !strings.HasPrefix(gotType, "text/plain") {
		t.Errorf("Content-Type = %q, want text/plain", gotType)
	}

	q, err := url.ParseQuery(gotQuery)
	if err != nil {
		t.Fatalf("parse query: %v", err)
	}
	for key, want := range map[string]string{
		"org":       "home",
		"bucket":    "geiger",
		"precision": "ns",
	} {
		if q.Get(key) != want {
			t.Errorf("query %s = %q, want %q", key, q.Get(key), want)
		}
	}
	// The token belongs in the header only; a token in a URL ends up in server
	// access logs.
	if strings.Contains(gotQuery, testToken) {
		t.Errorf("token was sent in the query string: %q", gotQuery)
	}
}

func TestPublishPreservesBasePath(t *testing.T) {
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	// An InfluxDB behind a reverse proxy is often mounted under a prefix.
	cfg := testConfig(srv.URL + "/influx/")
	s := newSink(t, cfg, discardLogger())
	if err := s.Publish(context.Background(), sampleReading()); err != nil {
		t.Fatalf("Publish: %v", err)
	}
	if gotPath != "/influx/api/v2/write" {
		t.Errorf("path = %q, want /influx/api/v2/write", gotPath)
	}
}

func TestPublishOmitsInvalidOptionalFields(t *testing.T) {
	tests := []struct {
		name string
		r    reading.Reading
		want string
	}{
		{
			name: "both missing",
			r: reading.Reading{
				Timestamp: sampleTime, CPM: 7, AverageCPM: 7, MicroSievertsPerHour: 0.045,
				Voltage:      reading.None[float64](),
				TemperatureC: reading.None[float64](),
			},
			want: "radiation cpm=7i,acpm=7,usvh=0.045 1700000000000000000",
		},
		{
			name: "voltage only",
			r: reading.Reading{
				Timestamp: sampleTime, CPM: 7, AverageCPM: 7, MicroSievertsPerHour: 0.045,
				Voltage:      reading.Some(3.9),
				TemperatureC: reading.None[float64](),
			},
			want: "radiation cpm=7i,acpm=7,usvh=0.045,volts=3.9 1700000000000000000",
		},
		{
			name: "temperature only",
			r: reading.Reading{
				Timestamp: sampleTime, CPM: 7, AverageCPM: 7, MicroSievertsPerHour: 0.045,
				Voltage:      reading.None[float64](),
				TemperatureC: reading.Some(0.0),
			},
			// A valid zero temperature must be written, which is the whole
			// reason the optional exists.
			want: "radiation cpm=7i,acpm=7,usvh=0.045,temp_c=0 1700000000000000000",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var gotBody string
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				body, _ := io.ReadAll(r.Body)
				gotBody = string(body)
				w.WriteHeader(http.StatusNoContent)
			}))
			defer srv.Close()

			s := newSink(t, testConfig(srv.URL), discardLogger())
			if err := s.Publish(context.Background(), tc.r); err != nil {
				t.Fatalf("Publish: %v", err)
			}
			if gotBody != tc.want {
				t.Errorf("body:\n got %q\nwant %q", gotBody, tc.want)
			}
		})
	}
}

func TestEscaping(t *testing.T) {
	tests := []struct {
		name            string
		in              string
		wantMeasurement string
		wantTag         string
		wantStringField string
	}{
		{
			name:            "plain",
			in:              "radiation",
			wantMeasurement: "radiation",
			wantTag:         "radiation",
			wantStringField: "radiation",
		},
		{
			name:            "comma",
			in:              "a,b",
			wantMeasurement: `a\,b`,
			wantTag:         `a\,b`,
			wantStringField: "a,b",
		},
		{
			name:            "space",
			in:              "a b",
			wantMeasurement: `a\ b`,
			wantTag:         `a\ b`,
			wantStringField: "a b",
		},
		{
			name: "equals",
			in:   "a=b",
			// An equals sign is not special in a measurement name, and escaping
			// it there would change the stored name.
			wantMeasurement: "a=b",
			wantTag:         `a\=b`,
			wantStringField: "a=b",
		},
		{
			name:            "double quote",
			in:              `a"b`,
			wantMeasurement: `a"b`,
			wantTag:         `a"b`,
			wantStringField: `a\"b`,
		},
		{
			name:            "backslash",
			in:              `a\b`,
			wantMeasurement: `a\\b`,
			wantTag:         `a\\b`,
			wantStringField: `a\\b`,
		},
		{
			name:            "trailing backslash",
			in:              `ab\`,
			wantMeasurement: `ab\\`,
			wantTag:         `ab\\`,
			wantStringField: `ab\\`,
		},
		{
			name:            "newline",
			in:              "a\nb",
			wantMeasurement: `a\nb`,
			wantTag:         `a\nb`,
			wantStringField: `a\nb`,
		},
		{
			name:            "all at once",
			in:              `a, b="c"\`,
			wantMeasurement: `a\,\ b="c"\\`,
			wantTag:         `a\,\ b\="c"\\`,
			wantStringField: `a, b=\"c\"\\`,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := escapeMeasurement(tc.in); got != tc.wantMeasurement {
				t.Errorf("escapeMeasurement(%q) = %q, want %q", tc.in, got, tc.wantMeasurement)
			}
			if got := escapeTag(tc.in); got != tc.wantTag {
				t.Errorf("escapeTag(%q) = %q, want %q", tc.in, got, tc.wantTag)
			}
			if got := escapeStringField(tc.in); got != tc.wantStringField {
				t.Errorf("escapeStringField(%q) = %q, want %q", tc.in, got, tc.wantStringField)
			}
			if got, want := quoteStringField(tc.in), `"`+tc.wantStringField+`"`; got != want {
				t.Errorf("quoteStringField(%q) = %q, want %q", tc.in, got, want)
			}
		})
	}
}

func TestLineProtocolEscapesMeasurement(t *testing.T) {
	r := reading.Reading{Timestamp: sampleTime, CPM: 1, AverageCPM: 1, MicroSievertsPerHour: 0.0065}
	got := lineProtocol("odd name,with=stuff", r)
	want := `odd\ name\,with=stuff cpm=1i,acpm=1,usvh=0.0065 1700000000000000000`
	if got != want {
		t.Errorf("lineProtocol:\n got %q\nwant %q", got, want)
	}
}

func TestPublishRetriesAndSurfacesBody(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = io.WriteString(w, `{"code":"internal error","message":"engine: shard is disabled"}`)
	}))
	defer srv.Close()

	s := newSink(t, testConfig(srv.URL), discardLogger())
	err := s.Publish(context.Background(), sampleReading())
	if err == nil {
		t.Fatal("Publish succeeded, want failure")
	}
	if got := calls.Load(); got != 3 {
		t.Errorf("server saw %d attempts, want 3", got)
	}
	if !strings.Contains(err.Error(), "engine: shard is disabled") {
		t.Errorf("error does not surface the response body: %v", err)
	}
	if !strings.Contains(err.Error(), "500") {
		t.Errorf("error does not name the status: %v", err)
	}
}

func TestPublishDoesNotRetryBadRequest(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, `{"code":"invalid","message":"unable to parse: missing fields"}`)
	}))
	defer srv.Close()

	s := newSink(t, testConfig(srv.URL), discardLogger())
	err := s.Publish(context.Background(), sampleReading())
	if err == nil {
		t.Fatal("Publish succeeded, want failure")
	}
	if got := calls.Load(); got != 1 {
		t.Errorf("server saw %d attempts, want 1: malformed line protocol never becomes valid", got)
	}
	if !strings.Contains(err.Error(), "unable to parse: missing fields") {
		t.Errorf("error does not surface the response body: %v", err)
	}
}

func TestPublishTruncatesLongBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, strings.Repeat("x", 4096))
	}))
	defer srv.Close()

	s := newSink(t, testConfig(srv.URL), discardLogger())
	err := s.Publish(context.Background(), sampleReading())
	if err == nil {
		t.Fatal("Publish succeeded, want failure")
	}
	if len(err.Error()) > maxBodySnippet+512 {
		t.Errorf("error is %d bytes; the body should have been truncated", len(err.Error()))
	}
	if !strings.HasSuffix(err.Error(), "...") {
		t.Errorf("truncated error should be marked as truncated: %v", err)
	}
}

func TestPublishConnectionRefused(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	addr := srv.URL
	srv.Close()

	s := newSink(t, testConfig(addr), discardLogger())
	err := s.Publish(context.Background(), sampleReading())
	if err == nil {
		t.Fatal("Publish succeeded against a closed server, want failure")
	}
	if !strings.Contains(err.Error(), sinkName) {
		t.Errorf("error does not name the sink: %v", err)
	}
}

func TestPublishHonoursCancelledContext(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	s := newSink(t, testConfig(srv.URL), discardLogger())
	err := s.Publish(ctx, sampleReading())
	if err == nil {
		t.Fatal("Publish succeeded with a cancelled context, want failure")
	}
	if !strings.Contains(err.Error(), context.Canceled.Error()) {
		t.Errorf("error does not report cancellation: %v", err)
	}
	if got := calls.Load(); got != 0 {
		t.Errorf("server saw %d requests, want 0", got)
	}
}

func TestPublishHonoursCancellationDuringRetry(t *testing.T) {
	var calls atomic.Int32
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Cancel once the first attempt has failed, so cancellation lands while
		// the sink is waiting to retry.
		calls.Add(1)
		cancel()
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	cfg := testConfig(srv.URL)
	cfg.Retries = 5
	s := newSink(t, cfg, discardLogger())
	// Long enough that the sink must actually observe the cancellation rather
	// than time out into the next attempt.
	s.backoff = 5 * time.Second

	done := make(chan error, 1)
	go func() { done <- s.Publish(ctx, sampleReading()) }()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("Publish succeeded, want failure")
		}
		if !strings.Contains(err.Error(), context.Canceled.Error()) {
			t.Errorf("error does not report cancellation: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Publish ignored context cancellation")
	}
	if got := calls.Load(); got != 1 {
		t.Errorf("server saw %d attempts, want 1", got)
	}
}

// TestPublishNeverLeaksToken guards the log hygiene rule. A transport failure
// hands back a *url.Error holding the whole request URL, and the sink logs that
// error on every retry; nothing about the token may survive that path.
func TestPublishNeverLeaksToken(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	addr := srv.URL
	// Closing the server forces the transport failure that carries the URL.
	srv.Close()

	var logs bytes.Buffer
	log := slog.New(slog.NewJSONHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug}))

	s := newSink(t, testConfig(addr), log)
	err := s.Publish(context.Background(), sampleReading())
	if err == nil {
		t.Fatal("Publish succeeded against a closed server, want failure")
	}

	if strings.Contains(err.Error(), testToken) {
		t.Errorf("token leaked into the returned error: %v", err)
	}
	if strings.Contains(logs.String(), testToken) {
		t.Errorf("token leaked into log output: %s", logs.String())
	}
	if logs.Len() == 0 {
		t.Fatal("no retry was logged, so this test proved nothing")
	}
	// The endpoint must still be identifiable, or the redaction has made the
	// failure undiagnosable.
	if !strings.Contains(logs.String(), "/api/v2/write") {
		t.Errorf("log does not identify the endpoint: %s", logs.String())
	}
}

// TestSecretIsNotLoggableByAccident records that the token is held as a
// redact.Secret, so even a careless structured log of the value prints a
// placeholder.
func TestSecretIsNotLoggableByAccident(t *testing.T) {
	var logs bytes.Buffer
	log := slog.New(slog.NewJSONHandler(&logs, nil))
	log.Info("careless", "token", redact.New(testToken))
	if strings.Contains(logs.String(), testToken) {
		t.Errorf("redact.Secret printed its value: %s", logs.String())
	}
}

func TestNewRejectsInvalidConfig(t *testing.T) {
	valid := config.InfluxV2{
		URL:    "http://localhost:8086",
		Token:  redact.New(testToken),
		Org:    "home",
		Bucket: "geiger",
	}
	tests := []struct {
		name   string
		mutate func(*config.InfluxV2)
	}{
		{"no URL", func(c *config.InfluxV2) { c.URL = "" }},
		{"no org", func(c *config.InfluxV2) { c.Org = "" }},
		{"no bucket", func(c *config.InfluxV2) { c.Bucket = "" }},
		{"no token", func(c *config.InfluxV2) { c.Token = redact.Secret{} }},
		{"wrong scheme", func(c *config.InfluxV2) { c.URL = "udp://localhost:8089" }},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfg := valid
			tc.mutate(&cfg)
			if _, err := New(cfg, discardLogger()); err == nil {
				t.Error("New accepted invalid configuration")
			}
		})
	}
}

func TestNewDoesNotRevealTokenInConfigErrors(t *testing.T) {
	_, err := New(config.InfluxV2{
		URL:    "http://%zz",
		Token:  redact.New(testToken),
		Org:    "home",
		Bucket: "geiger",
	}, discardLogger())
	if err == nil {
		t.Fatal("New accepted an unparseable URL")
	}
	if strings.Contains(err.Error(), testToken) {
		t.Errorf("token leaked into a configuration error: %v", err)
	}
}
