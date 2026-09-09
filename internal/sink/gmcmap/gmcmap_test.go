package gmcmap

import (
	"bytes"
	"context"
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
)

// canaryCounterID is a distinctive value that must never appear in a log line
// or in a returned error. It is deliberately unlike any real ID so a leak is
// unambiguous rather than a coincidental substring match.
const canaryCounterID = "CANARY-GID-d41d8cd98f00b204e9800998ecf8427e"

// testLocation is a valid coordinate pair. Tests that exercise the
// location requirement supply their own invalid one.
var testLocation = config.Location{Latitude: 35.6812, Longitude: 139.7671, Valid: true}

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
	s, err := New(config.GMCMap{
		AccountID: "12345",
		CounterID: redact.New(canaryCounterID),
		Timeout:   2 * time.Second,
		Retries:   retries,
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

func TestPublishSendsDocumentedQueryParameters(t *testing.T) {
	var got url.Values
	var gotUA string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.URL.Query()
		gotUA = r.Header.Get("User-Agent")
		_, _ = w.Write([]byte("OK.ERR0"))
	}))
	defer srv.Close()

	s := newTestSink(t, srv.URL, 0, discardLogger())
	defer func() { _ = s.Close() }()

	if err := s.Publish(context.Background(), testReading); err != nil {
		t.Fatalf("Publish: %v", err)
	}

	want := map[string]string{
		"AID":  "12345",
		"GID":  canaryCounterID,
		"CPM":  "42",
		"ACPM": "39.5",
		"uSV":  "0.2739",
	}
	for k, v := range want {
		if got.Get(k) != v {
			t.Errorf("query param %s = %q, want %q", k, got.Get(k), v)
		}
	}
	if len(got) != len(want) {
		t.Errorf("sent %d query params (%v), want exactly %d", len(got), got, len(want))
	}
	if !strings.Contains(gotUA, "gmc-exporter") {
		t.Errorf("User-Agent = %q, want it to identify the exporter", gotUA)
	}
}

func TestNewRequiresValidLocation(t *testing.T) {
	_, err := New(config.GMCMap{
		AccountID: "12345",
		CounterID: redact.New(canaryCounterID),
	}, config.Location{}, discardLogger())
	if err == nil {
		t.Fatal("New succeeded without a location, want an error")
	}
	for _, want := range []string{"location", "GOGMC_LATITUDE", "GOGMC_LONGITUDE"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %q", err, want)
		}
	}
}

func TestNewRequiresCredentials(t *testing.T) {
	if _, err := New(config.GMCMap{CounterID: redact.New(canaryCounterID)},
		testLocation, discardLogger()); err == nil {
		t.Error("New succeeded without an account ID, want an error")
	}
	if _, err := New(config.GMCMap{AccountID: "12345"},
		testLocation, discardLogger()); err == nil {
		t.Error("New succeeded without a counter ID, want an error")
	}
}

func TestPublishRetriesServerErrorsThenFails(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte("upstream exploded"))
	}))
	defer srv.Close()

	s := newTestSink(t, srv.URL, 2, discardLogger())
	defer func() { _ = s.Close() }()

	err := s.Publish(context.Background(), testReading)
	if err == nil {
		t.Fatal("Publish succeeded on a 500, want an error")
	}
	if n := calls.Load(); n != 3 {
		t.Errorf("server saw %d requests, want 3 (initial attempt plus 2 retries)", n)
	}
	if !strings.Contains(err.Error(), "500") {
		t.Errorf("error %q does not report the status code", err)
	}
}

func TestPublishTreatsAuthFailureAsPermanent(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()

	s := newTestSink(t, srv.URL, 3, discardLogger())
	defer func() { _ = s.Close() }()

	if err := s.Publish(context.Background(), testReading); err == nil {
		t.Fatal("Publish succeeded on a 401, want an error")
	}
	if n := calls.Load(); n != 1 {
		t.Errorf("server saw %d requests, want 1: an auth failure must never be retried", n)
	}
}

// TestPublishRejectsErrorBodyWithStatus200 covers the defining quirk of this
// API: every one of these responses arrives with HTTP 200. The bodies are the
// ones the live server is recorded as returning, typo included.
func TestPublishRejectsErrorBodyWithStatus200(t *testing.T) {
	tests := []struct {
		name         string
		body         string
		wantAttempts int32
	}{
		{
			name:         "unknown account",
			body:         "\r\n<!--  sendmail.asp-->\r\n\r\nError! User not found.ERR1.",
			wantAttempts: 1,
		},
		{
			name:         "unknown counter",
			body:         "\r\n<!--  sendmail.asp-->\r\n\r\nError! Geiger Counter not found.ERR2.",
			wantAttempts: 1,
		},
		{
			name:         "malformed value",
			body:         "\r\n<!--  sendmail.asp-->\r\n\r\nError! Data Error!(CPM)ERR4.",
			wantAttempts: 1,
		},
		{
			// An unrecognised body could be a proxy or an outage page, so it is
			// transient and all attempts are used.
			name:         "unrelated page",
			body:         "<html><title>Login</title></html>",
			wantAttempts: 3,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var calls atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				_, _ = w.Write([]byte(tc.body))
			}))
			defer srv.Close()

			s := newTestSink(t, srv.URL, 2, discardLogger())
			defer func() { _ = s.Close() }()

			if err := s.Publish(context.Background(), testReading); err == nil {
				t.Fatalf("Publish accepted an error body returned with status 200: %q", tc.body)
			}
			if n := calls.Load(); n != tc.wantAttempts {
				t.Errorf("server saw %d requests, want %d", n, tc.wantAttempts)
			}
		})
	}
}

// TestPublishAcceptsDocumentedSuccessBodies checks that success is recognised
// from the ERR0 token alone. The second case is a real success that also
// carries a location-confirmation nag, which must not be read as a failure.
func TestPublishAcceptsDocumentedSuccessBodies(t *testing.T) {
	bodies := map[string]string{
		"plain": "\r\n<!--  sendmail.asp-->\r\n\r\nOK.ERR0",
		"with location nag": "\r\n<!--  sendmail.asp-->\r\n\r\n" +
			"Warrning! Please update/confirm your location.<BR>OK.ERR0",
	}
	for name, body := range bodies {
		t.Run(name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, _ = w.Write([]byte(body))
			}))
			defer srv.Close()

			s := newTestSink(t, srv.URL, 0, discardLogger())
			defer func() { _ = s.Close() }()

			if err := s.Publish(context.Background(), testReading); err != nil {
				t.Fatalf("Publish rejected a documented success body %q: %v", body, err)
			}
		})
	}
}

// TestPublishSendsTemperatureOnlyWhenMeasured guards the distinction between a
// temperature of zero and no temperature at all.
func TestPublishSendsTemperatureOnlyWhenMeasured(t *testing.T) {
	var got url.Values
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.URL.Query()
		_, _ = w.Write([]byte("OK.ERR0"))
	}))
	defer srv.Close()

	s := newTestSink(t, srv.URL, 0, discardLogger())
	defer func() { _ = s.Close() }()

	if err := s.Publish(context.Background(), testReading); err != nil {
		t.Fatalf("Publish: %v", err)
	}
	if got.Has("tmp") {
		t.Errorf("tmp was sent for a reading with no temperature: %q", got.Get("tmp"))
	}

	withTemp := testReading
	withTemp.TemperatureC = reading.Some(0.0)
	if err := s.Publish(context.Background(), withTemp); err != nil {
		t.Fatalf("Publish: %v", err)
	}
	if got.Get("tmp") != "0" {
		t.Errorf("tmp = %q, want %q for a measured zero", got.Get("tmp"), "0")
	}
}

func TestPublishReportsConnectionRefused(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	endpoint := srv.URL
	// Closing before publishing guarantees the dial fails, which is the path
	// that produces a *url.Error carrying the credential-bearing URL.
	srv.Close()

	s := newTestSink(t, endpoint, 1, discardLogger())
	defer func() { _ = s.Close() }()

	err := s.Publish(context.Background(), testReading)
	if err == nil {
		t.Fatal("Publish succeeded against a closed server, want an error")
	}
	if strings.Contains(err.Error(), canaryCounterID) {
		t.Fatalf("counter ID leaked into the error: %v", err)
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

// TestCredentialNeverReachesLogsOrErrors is the reason redact exists. A
// transport failure hands http.Client's *url.Error — which quotes the full
// request URL, counter ID included — to the retry path, which both logs it and
// returns it.
func TestCredentialNeverReachesLogsOrErrors(t *testing.T) {
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

	if strings.Contains(err.Error(), canaryCounterID) {
		t.Errorf("counter ID leaked into the returned error: %v", err)
	}
	if strings.Contains(buf.String(), canaryCounterID) {
		t.Errorf("counter ID leaked into log output: %s", buf.String())
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
