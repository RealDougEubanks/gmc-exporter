package safecast

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/RealDougEubanks/gmc-exporter/internal/config"
	"github.com/RealDougEubanks/gmc-exporter/internal/reading"
	"github.com/RealDougEubanks/gmc-exporter/internal/redact"
)

// canaryAPIKey is a distinctive value that must never appear in a log line or
// in a returned error. It is deliberately unlike any real key so a leak is
// unambiguous rather than a coincidental substring match.
const canaryAPIKey = "CANARY-APIKEY-9e107d9d372bb6826bd81d3542a419d6"

// testLocation is a valid coordinate pair. Tests that exercise the location
// requirement supply their own invalid one.
var testLocation = config.Location{Latitude: 37.4213, Longitude: 141.0328, Valid: true}

// testReading is a fixed sample, so assertions can compare against exact
// strings rather than reformatting the values under test with the code under
// test.
var testReading = reading.Reading{
	Timestamp:            time.Date(2026, 3, 14, 15, 9, 26, 0, time.UTC),
	CPM:                  42,
	AverageCPM:           39.5,
	MicroSievertsPerHour: 0.2739,
}

// newTestSink builds a sink aimed at a test server. It bypasses New only for
// the endpoint, so every other construction rule stays under test.
func newTestSink(t *testing.T, endpoint string, retries int, log *slog.Logger) *Sink {
	t.Helper()
	s, err := New(config.Safecast{
		APIKey:   redact.New(canaryAPIKey),
		DeviceID: 731,
		Timeout:  2 * time.Second,
		Retries:  retries,
	}, testLocation, log)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	s.endpoint = endpoint
	return s
}

// discardLogger keeps test output readable for cases that are not asserting on
// log content.
func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(new(bytes.Buffer), nil))
}

// created is the 201 response a real create returns. Only the status matters to
// the sink, but a realistic body guards against a success being misread as an
// error payload.
const created = `{"id":1234567,"value":0.2739,"unit":"usv","device_id":731,` +
	`"captured_at":"2026/03/14 15:09:26 +0000","latitude":37.4213,"longitude":141.0328}`

func TestPublishSendsDocumentedMeasurement(t *testing.T) {
	var gotBody []byte
	var gotQuery, gotMethod, gotContentType, gotUA string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotBody, _ = io.ReadAll(r.Body)
		gotQuery = r.URL.RawQuery
		gotMethod = r.Method
		gotContentType = r.Header.Get("Content-Type")
		gotUA = r.Header.Get("User-Agent")
		w.WriteHeader(http.StatusCreated)
		_, _ = io.WriteString(w, created)
	}))
	defer srv.Close()

	s := newTestSink(t, srv.URL, 0, discardLogger())
	defer func() { _ = s.Close() }()

	if err := s.Publish(context.Background(), testReading); err != nil {
		t.Fatalf("Publish: %v", err)
	}

	if gotMethod != http.MethodPost {
		t.Errorf("method = %s, want POST", gotMethod)
	}
	if gotContentType != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", gotContentType)
	}
	if !strings.Contains(gotUA, "gmc-exporter") {
		t.Errorf("User-Agent = %q, want it to identify the exporter", gotUA)
	}

	// The key authenticates as a query parameter, which is exactly why every
	// error in this package has to be redacted.
	if want := "api_key=" + canaryAPIKey; gotQuery != want {
		t.Errorf("query = %q, want %q", gotQuery, want)
	}

	var env struct {
		Measurement map[string]any `json:"measurement"`
	}
	if err := json.Unmarshal(gotBody, &env); err != nil {
		t.Fatalf("request body is not the documented envelope: %v (body %s)", err, gotBody)
	}
	m := env.Measurement
	if m == nil {
		t.Fatalf(`request body has no "measurement" key: %s`, gotBody)
	}

	wantValues := map[string]any{
		"value":       0.2739,
		"unit":        "usv",
		"latitude":    37.4213,
		"longitude":   141.0328,
		"captured_at": "2026-03-14T15:09:26Z",
		"device_id":   float64(731),
	}
	for k, want := range wantValues {
		if got, ok := m[k]; !ok {
			t.Errorf("measurement is missing field %q", k)
		} else if got != want {
			t.Errorf("measurement[%q] = %#v, want %#v", k, got, want)
		}
	}
	if len(m) != len(wantValues) {
		t.Errorf("measurement has %d fields (%v), want exactly %d", len(m), m, len(wantValues))
	}
}

// TestUnitStringIsExactlyUsv is worth its own test because Safecast does not
// validate the unit. A wrong string here is accepted with a 201 and quietly
// corrupts a public dataset, so nothing else would catch the mistake.
func TestUnitStringIsExactlyUsv(t *testing.T) {
	if unitMicroSievertsPerHour != "usv" {
		t.Errorf("unit = %q, want %q: Safecast stores the dose-rate unit lowercase and without a slash",
			unitMicroSievertsPerHour, "usv")
	}
}

func TestPublishOmitsUnconfiguredDeviceID(t *testing.T) {
	var gotBody []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotBody, _ = io.ReadAll(r.Body)
		w.WriteHeader(http.StatusCreated)
		_, _ = io.WriteString(w, created)
	}))
	defer srv.Close()

	s, err := New(config.Safecast{APIKey: redact.New(canaryAPIKey)}, testLocation, discardLogger())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	s.endpoint = srv.URL
	defer func() { _ = s.Close() }()

	if err := s.Publish(context.Background(), testReading); err != nil {
		t.Fatalf("Publish: %v", err)
	}
	if strings.Contains(string(gotBody), "device_id") {
		t.Errorf("device_id was sent although none is configured: %s", gotBody)
	}
}

func TestNewRequiresValidLocation(t *testing.T) {
	_, err := New(config.Safecast{APIKey: redact.New(canaryAPIKey)},
		config.Location{}, discardLogger())
	if err == nil {
		t.Fatal("New succeeded without a location, want an error")
	}
	for _, want := range []string{"location", "GOGMC_LATITUDE", "GOGMC_LONGITUDE"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %q", err, want)
		}
	}
}

func TestNewRequiresAPIKey(t *testing.T) {
	if _, err := New(config.Safecast{}, testLocation, discardLogger()); err == nil {
		t.Error("New succeeded without an API key, want an error")
	}
}

func TestPublishRetriesServerErrorsThenFails(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusBadGateway)
		_, _ = io.WriteString(w, "upstream exploded")
	}))
	defer srv.Close()

	s := newTestSink(t, srv.URL, 2, discardLogger())
	defer func() { _ = s.Close() }()

	err := s.Publish(context.Background(), testReading)
	if err == nil {
		t.Fatal("Publish succeeded on a 502, want an error")
	}
	if n := calls.Load(); n != 3 {
		t.Errorf("server saw %d requests, want 3 (initial attempt plus 2 retries)", n)
	}
	if !strings.Contains(err.Error(), "502") {
		t.Errorf("error %q does not report the status code", err)
	}
}

func TestPublishNeverRetriesAuthFailure(t *testing.T) {
	for _, status := range []int{http.StatusUnauthorized, http.StatusForbidden} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			var calls atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				w.WriteHeader(status)
				_, _ = io.WriteString(w, `{"error":"You need to sign in or sign up before continuing."}`)
			}))
			defer srv.Close()

			s := newTestSink(t, srv.URL, 3, discardLogger())
			defer func() { _ = s.Close() }()

			err := s.Publish(context.Background(), testReading)
			if err == nil {
				t.Fatalf("Publish succeeded on a %d, want an error", status)
			}
			if n := calls.Load(); n != 1 {
				t.Errorf("server saw %d requests, want 1: an auth failure must never be retried", n)
			}
			if !strings.Contains(err.Error(), "GOGMC_SAFECAST_API_KEY") {
				t.Errorf("error %q does not tell the operator which setting to check", err)
			}
		})
	}
}

func TestPublishTreatsValidationFailureAsPermanent(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusUnprocessableEntity)
		_, _ = io.WriteString(w, `{"errors":{"value":["can't be blank"],"unit":["can't be blank"]}}`)
	}))
	defer srv.Close()

	s := newTestSink(t, srv.URL, 3, discardLogger())
	defer func() { _ = s.Close() }()

	err := s.Publish(context.Background(), testReading)
	if err == nil {
		t.Fatal("Publish succeeded on a 422, want an error")
	}
	if n := calls.Load(); n != 1 {
		t.Errorf("server saw %d requests, want 1: a rejected measurement must not be retried", n)
	}
	// The summary must name the offending fields, and in a stable order.
	if !strings.Contains(err.Error(), "unit: can't be blank; value: can't be blank") {
		t.Errorf("error %q does not summarise the validation failures in field order", err)
	}
}

// TestPublishTreatsDuplicateAsSuccess covers Safecast's md5sum uniqueness
// constraint. A retry that lands after the original succeeded is rejected as a
// duplicate, and reporting that as a failure would alert on a reading that is
// in fact safely stored.
func TestPublishTreatsDuplicateAsSuccess(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnprocessableEntity)
		_, _ = io.WriteString(w, `{"errors":{"md5sum":["has already been taken"]}}`)
	}))
	defer srv.Close()

	s := newTestSink(t, srv.URL, 0, discardLogger())
	defer func() { _ = s.Close() }()

	if err := s.Publish(context.Background(), testReading); err != nil {
		t.Fatalf("Publish reported a duplicate as a failure: %v", err)
	}
}

// TestPublishRejectsErrorPayloadWithStatus200 guards against a proxy or a
// future API change returning a validation failure behind a success status.
// Counting that as published would hide silent data loss.
func TestPublishRejectsErrorPayloadWithStatus200(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{"errors":{"value":["is not a number"]}}`)
	}))
	defer srv.Close()

	s := newTestSink(t, srv.URL, 0, discardLogger())
	defer func() { _ = s.Close() }()

	err := s.Publish(context.Background(), testReading)
	if err == nil {
		t.Fatal("Publish accepted an error payload returned with status 200")
	}
	if !strings.Contains(err.Error(), "is not a number") {
		t.Errorf("error %q does not carry the server's explanation", err)
	}
}

func TestPublishAcceptsCreatedWithoutErrors(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusCreated)
		_, _ = io.WriteString(w, created)
	}))
	defer srv.Close()

	s := newTestSink(t, srv.URL, 0, discardLogger())
	defer func() { _ = s.Close() }()

	if err := s.Publish(context.Background(), testReading); err != nil {
		t.Fatalf("Publish rejected a 201 Created: %v", err)
	}
}

func TestPublishReportsConnectionRefused(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	endpoint := srv.URL
	// Closing before publishing guarantees the dial fails, which is the path
	// that produces a *url.Error carrying the key-bearing URL.
	srv.Close()

	s := newTestSink(t, endpoint, 1, discardLogger())
	defer func() { _ = s.Close() }()

	err := s.Publish(context.Background(), testReading)
	if err == nil {
		t.Fatal("Publish succeeded against a closed server, want an error")
	}
	if strings.Contains(err.Error(), canaryAPIKey) {
		t.Fatalf("API key leaked into the error: %v", err)
	}
}

func TestPublishHonoursContextCancellation(t *testing.T) {
	released := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-released
	}))
	defer srv.Close()
	defer close(released)

	s := newTestSink(t, srv.URL, 2, discardLogger())
	defer func() { _ = s.Close() }()

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()

	done := make(chan error, 1)
	go func() { done <- s.Publish(ctx, testReading) }()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("Publish succeeded despite cancellation, want an error")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Publish ignored context cancellation")
	}
}

func TestPublishAbortsOnAlreadyCancelledContext(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
	}))
	defer srv.Close()

	s := newTestSink(t, srv.URL, 2, discardLogger())
	defer func() { _ = s.Close() }()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if err := s.Publish(ctx, testReading); err == nil {
		t.Fatal("Publish succeeded with a cancelled context, want an error")
	}
	if n := calls.Load(); n != 0 {
		t.Errorf("server saw %d requests, want 0 with a cancelled context", n)
	}
}

// TestAPIKeyNeverReachesLogsOrErrors is the reason redact exists. A transport
// failure hands http.Client's *url.Error — which quotes the full request URL,
// API key included — to the retry path, which both logs it and returns it.
func TestAPIKeyNeverReachesLogsOrErrors(t *testing.T) {
	var buf bytes.Buffer
	log := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	endpoint := srv.URL
	srv.Close()

	s := newTestSink(t, endpoint, 2, log)
	defer func() { _ = s.Close() }()

	err := s.Publish(context.Background(), testReading)
	if err == nil {
		t.Fatal("Publish succeeded against a closed server, want an error")
	}

	if strings.Contains(err.Error(), canaryAPIKey) {
		t.Errorf("API key leaked into the returned error: %v", err)
	}
	if strings.Contains(buf.String(), canaryAPIKey) {
		t.Errorf("API key leaked into log output: %s", buf.String())
	}
	// The retry path must actually have logged, otherwise the assertion above
	// is vacuously satisfied by an empty buffer.
	if buf.Len() == 0 {
		t.Fatal("no log output captured, so the leak assertion proved nothing")
	}
	if !strings.Contains(buf.String(), redact.Placeholder) {
		t.Errorf("log output does not show a redacted URL, so redaction may not be running: %s", buf.String())
	}
}
