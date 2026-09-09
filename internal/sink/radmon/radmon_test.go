package radmon

import (
	"bytes"
	"context"
	"errors"
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

// canaryPassword is deliberately unlike any other string in this package, so a
// substring search for it in log or error text cannot match by accident.
const canaryPassword = "zzQUUX-canary-datasend-7f3a-NEVERLOG"

// compile-time proof the sink satisfies the interface the fan-out publishes to.
var _ sink.Sink = (*Sink)(nil)

// newTestSink builds a sink pointed at ts. Passing a nil server leaves the
// default endpoint in place, which is what the constructor tests want.
func newTestSink(t *testing.T, cfg config.Radmon, loc config.Location, endpoint string, log *slog.Logger) *Sink {
	t.Helper()
	s, err := New(cfg, loc, log)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if endpoint != "" {
		s.endpoint = endpoint
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func baseConfig() config.Radmon {
	return config.Radmon{
		Enabled:  true,
		User:     "testuser",
		Password: redact.New(canaryPassword),
		Timeout:  2 * time.Second,
		Retries:  0,
	}
}

func testLocation() config.Location {
	return config.Location{Latitude: 54.8097, Longitude: -2.01445, Valid: true}
}

func testReading() reading.Reading {
	return reading.Reading{
		Timestamp:            time.Unix(1700000000, 0).UTC(),
		CPM:                  137,
		AverageCPM:           140.5,
		MicroSievertsPerHour: 0.89,
		Voltage:              reading.Some(4.2),
		TemperatureC:         reading.None[float64](),
	}
}

// discardLogger keeps test output clean without hiding the handler behind a nil
// check the sink would replace with slog.Default.
func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func TestPublishSubmitsExpectedQuery(t *testing.T) {
	var got url.Values
	var method string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		method = r.Method
		got = r.URL.Query()
		_, _ = io.WriteString(w, "OK")
	}))
	defer ts.Close()

	s := newTestSink(t, baseConfig(), config.Location{}, ts.URL, discardLogger())
	if err := s.Publish(context.Background(), testReading()); err != nil {
		t.Fatalf("Publish: %v", err)
	}

	if method != http.MethodGet {
		t.Errorf("method = %q, want GET", method)
	}
	want := map[string]string{
		"function": "submit",
		"user":     "testuser",
		"password": canaryPassword,
		"value":    "137",
		"unit":     "CPM",
	}
	for k, v := range want {
		if got.Get(k) != v {
			t.Errorf("query %q = %q, want %q", k, got.Get(k), v)
		}
	}
	if len(got) != len(want) {
		t.Errorf("query has %d parameters (%v), want exactly %d", len(got), got, len(want))
	}
	for _, unwanted := range []string{"latitude", "longitude"} {
		if got.Has(unwanted) {
			t.Errorf("query unexpectedly carries %q without UseLatLng", unwanted)
		}
	}
}

func TestPublishWithLatLngIncludesCoordinates(t *testing.T) {
	var got url.Values
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.URL.Query()
		_, _ = io.WriteString(w, "OK")
	}))
	defer ts.Close()

	cfg := baseConfig()
	cfg.UseLatLng = true
	s := newTestSink(t, cfg, testLocation(), ts.URL, discardLogger())
	if err := s.Publish(context.Background(), testReading()); err != nil {
		t.Fatalf("Publish: %v", err)
	}

	want := map[string]string{
		"function":  "submitwithlatlng",
		"user":      "testuser",
		"password":  canaryPassword,
		"value":     "137",
		"unit":      "CPM",
		"latitude":  "54.8097",
		"longitude": "-2.01445",
	}
	for k, v := range want {
		if got.Get(k) != v {
			t.Errorf("query %q = %q, want %q", k, got.Get(k), v)
		}
	}
}

func TestPingUsesPingFunctionAndSendsNoReading(t *testing.T) {
	var got url.Values
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.URL.Query()
		_, _ = io.WriteString(w, "pong")
	}))
	defer ts.Close()

	s := newTestSink(t, baseConfig(), config.Location{}, ts.URL, discardLogger())
	if err := s.Ping(context.Background()); err != nil {
		t.Fatalf("Ping: %v", err)
	}

	if got.Get("function") != "ping" {
		t.Errorf("function = %q, want ping", got.Get("function"))
	}
	// The whole point of Ping is that it does not write to the public dataset.
	for _, unwanted := range []string{"value", "unit", "latitude", "longitude"} {
		if got.Has(unwanted) {
			t.Errorf("ping carried %q, which would submit data", unwanted)
		}
	}
}

// A 200 carrying an error sentence is the failure mode this sink exists to
// catch: radmon.org reports rejections in the body, not the status code.
func TestNonOKBodyOn200IsFailure(t *testing.T) {
	var calls atomic.Int32
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		_, _ = io.WriteString(w, "Error: value out of range")
	}))
	defer ts.Close()

	s := newTestSink(t, baseConfig(), config.Location{}, ts.URL, discardLogger())
	err := s.Publish(context.Background(), testReading())
	if err == nil {
		t.Fatal("Publish succeeded on a non-OK body")
	}
	if !strings.Contains(err.Error(), "out of range") {
		t.Errorf("error does not report the server's message: %v", err)
	}
	if got := calls.Load(); got != 1 {
		t.Errorf("server called %d times, want 1", got)
	}
}

func TestAuthFailureBodyIsNotRetried(t *testing.T) {
	var calls atomic.Int32
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		_, _ = io.WriteString(w, "Incorrect login.")
	}))
	defer ts.Close()

	cfg := baseConfig()
	cfg.Retries = 3
	s := newTestSink(t, cfg, config.Location{}, ts.URL, discardLogger())

	if err := s.Publish(context.Background(), testReading()); err == nil {
		t.Fatal("Publish succeeded despite an authentication failure")
	}
	if got := calls.Load(); got != 1 {
		t.Errorf("server called %d times; bad credentials must not be retried", got)
	}
}

func TestUnauthorizedStatusIsNotRetried(t *testing.T) {
	var calls atomic.Int32
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer ts.Close()

	cfg := baseConfig()
	cfg.Retries = 3
	s := newTestSink(t, cfg, config.Location{}, ts.URL, discardLogger())

	if err := s.Publish(context.Background(), testReading()); err == nil {
		t.Fatal("Publish succeeded on 401")
	}
	if got := calls.Load(); got != 1 {
		t.Errorf("server called %d times; a 401 must not be retried", got)
	}
}

func TestServerErrorRetriesThenFails(t *testing.T) {
	var calls atomic.Int32
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		http.Error(w, "backend down", http.StatusBadGateway)
	}))
	defer ts.Close()

	cfg := baseConfig()
	cfg.Retries = 2
	s := newTestSink(t, cfg, config.Location{}, ts.URL, discardLogger())

	err := s.Publish(context.Background(), testReading())
	if err == nil {
		t.Fatal("Publish succeeded against a failing server")
	}
	if got := calls.Load(); got != 3 {
		t.Errorf("server called %d times, want 3 (initial attempt plus 2 retries)", got)
	}
	if !strings.Contains(err.Error(), "502") {
		t.Errorf("error does not name the status: %v", err)
	}
}

func TestRetrySucceedsAfterTransientFailure(t *testing.T) {
	var calls atomic.Int32
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			http.Error(w, "try again", http.StatusServiceUnavailable)
			return
		}
		_, _ = io.WriteString(w, "OK")
	}))
	defer ts.Close()

	cfg := baseConfig()
	cfg.Retries = 2
	s := newTestSink(t, cfg, config.Location{}, ts.URL, discardLogger())

	if err := s.Publish(context.Background(), testReading()); err != nil {
		t.Fatalf("Publish: %v", err)
	}
	if got := calls.Load(); got != 2 {
		t.Errorf("server called %d times, want 2", got)
	}
}

func TestConnectionRefusedIsHandled(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	addr := ts.URL
	// Closing before the request guarantees a transport failure rather than an
	// HTTP response.
	ts.Close()

	s := newTestSink(t, baseConfig(), config.Location{}, addr, discardLogger())
	err := s.Publish(context.Background(), testReading())
	if err == nil {
		t.Fatal("Publish succeeded against a closed server")
	}
	if !strings.Contains(err.Error(), "radmon") {
		t.Errorf("error is not attributed to the sink: %v", err)
	}
}

func TestContextCancellationIsHonoured(t *testing.T) {
	release := make(chan struct{})
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-release
		_, _ = io.WriteString(w, "OK")
	}))
	defer func() {
		close(release)
		ts.Close()
	}()

	cfg := baseConfig()
	cfg.Retries = 3
	s := newTestSink(t, cfg, config.Location{}, ts.URL, discardLogger())

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	start := time.Now()
	err := s.Publish(ctx, testReading())
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("Publish succeeded despite a cancelled context")
	}
	// With three retries and a doubling 500ms backoff, ignoring cancellation
	// would take multiple seconds.
	if elapsed > time.Second {
		t.Errorf("Publish took %s; cancellation was not honoured promptly", elapsed)
	}
}

func TestContextAlreadyCancelledMakesNoRequest(t *testing.T) {
	var calls atomic.Int32
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		_, _ = io.WriteString(w, "OK")
	}))
	defer ts.Close()

	s := newTestSink(t, baseConfig(), config.Location{}, ts.URL, discardLogger())

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if err := s.Publish(ctx, testReading()); err == nil {
		t.Fatal("Publish succeeded with an already-cancelled context")
	}
	if got := calls.Load(); got != 0 {
		t.Errorf("server called %d times with a cancelled context, want 0", got)
	}
}

// TestPasswordNeverReachesLogsOrErrors is the reason redact exists. The
// password rides in the query string, and http.Client reports transport
// failures as *url.Error carrying the full URL. Any regression here writes a
// plaintext credential into logs on every failure.
func TestPasswordNeverReachesLogsOrErrors(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	addr := ts.URL
	ts.Close() // Force a transport failure, the path that carries the URL.

	var logBuf bytes.Buffer
	log := slog.New(slog.NewJSONHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelDebug}))

	cfg := baseConfig()
	cfg.Retries = 2
	s := newTestSink(t, cfg, config.Location{}, addr, log)

	err := s.Publish(context.Background(), testReading())
	if err == nil {
		t.Fatal("Publish succeeded against a closed server")
	}

	if strings.Contains(err.Error(), canaryPassword) {
		t.Errorf("returned error leaked the password: %v", err)
	}
	if strings.Contains(logBuf.String(), canaryPassword) {
		t.Errorf("log output leaked the password:\n%s", logBuf.String())
	}
	// The escaped form would survive a naive check for the raw string.
	escaped := url.QueryEscape(canaryPassword)
	if strings.Contains(err.Error(), escaped) || strings.Contains(logBuf.String(), escaped) {
		t.Error("the URL-escaped password leaked into the error or log")
	}
	// Logging must still have happened; a test that passes because nothing was
	// logged proves nothing.
	if logBuf.Len() == 0 {
		t.Fatal("no log output was produced, so the redaction assertion is vacuous")
	}
}

// The same guarantee must hold for a non-transport failure, where the server's
// own body reaches the error message.
func TestPasswordNeverLeaksOnResponseFailure(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Echo the request URL back, mimicking a server that quotes what it
		// received in its error page.
		_, _ = io.WriteString(w, "Error processing "+r.URL.String())
	}))
	defer ts.Close()

	var logBuf bytes.Buffer
	log := slog.New(slog.NewJSONHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelDebug}))

	cfg := baseConfig()
	cfg.Retries = 1
	s := newTestSink(t, cfg, config.Location{}, ts.URL, log)

	err := s.Publish(context.Background(), testReading())
	if err == nil {
		t.Fatal("Publish succeeded on an error body")
	}
	// The excerpt is bounded, so the password must fall outside it.
	if strings.Contains(err.Error(), canaryPassword) {
		t.Errorf("error leaked the password from the response body: %v", err)
	}
	if strings.Contains(logBuf.String(), canaryPassword) {
		t.Errorf("log leaked the password from the response body:\n%s", logBuf.String())
	}
}

func TestNewRejectsUseLatLngWithoutLocation(t *testing.T) {
	cfg := baseConfig()
	cfg.UseLatLng = true

	_, err := New(cfg, config.Location{}, discardLogger())
	if err == nil {
		t.Fatal("New accepted UseLatLng without a location")
	}
	for _, want := range []string{"GOGMC_LATITUDE", "GOGMC_LONGITUDE"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error does not name %s: %v", want, err)
		}
	}
}

func TestNewRejectsMissingCredentials(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(*config.Radmon)
		wantEnv string
	}{
		{"no user", func(c *config.Radmon) { c.User = "" }, "GOGMC_RADMON_USER"},
		{"blank user", func(c *config.Radmon) { c.User = "   " }, "GOGMC_RADMON_USER"},
		{"no password", func(c *config.Radmon) { c.Password = redact.Secret{} }, "GOGMC_RADMON_PASSWORD"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := baseConfig()
			tt.mutate(&cfg)
			_, err := New(cfg, testLocation(), discardLogger())
			if err == nil {
				t.Fatal("New accepted an incomplete configuration")
			}
			if !strings.Contains(err.Error(), tt.wantEnv) {
				t.Errorf("error does not name %s: %v", tt.wantEnv, err)
			}
		})
	}
}

func TestNewAcceptsValidConfigAndCloseIsSafe(t *testing.T) {
	s, err := New(baseConfig(), config.Location{}, discardLogger())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if s.Name() != "radmon" {
		t.Errorf("Name = %q, want radmon", s.Name())
	}
	if s.endpoint != defaultEndpoint {
		t.Errorf("endpoint = %q, want %q", s.endpoint, defaultEndpoint)
	}
	// Close must be safe on a sink that never published.
	if err := s.Close(); err != nil {
		t.Errorf("Close: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Errorf("second Close: %v", err)
	}
}

func TestOversizedBodyIsBounded(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, strings.Repeat("x", 1<<20))
	}))
	defer ts.Close()

	s := newTestSink(t, baseConfig(), config.Location{}, ts.URL, discardLogger())
	err := s.Publish(context.Background(), testReading())
	if err == nil {
		t.Fatal("Publish succeeded on a garbage body")
	}
	if len(err.Error()) > 512 {
		t.Errorf("error message is %d bytes; the body excerpt is not bounded", len(err.Error()))
	}
}

// TestRateLimitIsNotRetried covers a response observed in production.
//
// When another exporter had submitted moments earlier, radmon.org answered a
// submission with HTTP 200 and the body "Too soon <br>". The sink correctly
// treated that as a failure rather than a success, but then retried it three
// times in about two seconds.
//
// Retrying a rate limit cannot succeed: the server is saying "not yet", so
// three attempts produce three rejections instead of one, against a free public
// service. The reading is dropped and the next poll submits on schedule.
func TestRateLimitIsNotRetried(t *testing.T) {
	var attempts atomic.Int32

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		attempts.Add(1)
		w.WriteHeader(http.StatusOK)
		// The exact body observed from radmon.org.
		_, _ = io.WriteString(w, "Too soon <br>")
	}))
	defer srv.Close()

	cfg := baseConfig()
	cfg.Retries = 3
	s := newTestSink(t, cfg, testLocation(), srv.URL, slog.New(slog.NewTextHandler(io.Discard, nil)))

	err := s.Publish(context.Background(), testReading())
	if err == nil {
		t.Fatal("a rate-limited submission should be reported as a failure")
	}
	if n := attempts.Load(); n != 1 {
		t.Errorf("server was called %d times, want exactly 1: a rate limit must not be retried", n)
	}
	if !strings.Contains(err.Error(), "too soon") && !strings.Contains(strings.ToLower(err.Error()), "too soon") {
		t.Errorf("error %q should explain that the submission was too soon", err)
	}
}

// TestRateLimitStopsFurtherRequests covers the cooldown.
//
// Not retrying a single rejection is not enough on its own. Observed against
// the live service, submissions 60 and then 70 seconds apart were both refused
// as "Too soon", so an exporter that simply tries again every poll keeps
// generating requests the server has already said it does not want. After a
// refusal the sink goes quiet and makes no network call at all until the
// cooldown expires.
func TestRateLimitStopsFurtherRequests(t *testing.T) {
	var calls atomic.Int32

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		_, _ = io.WriteString(w, "Too soon <br>")
	}))
	defer srv.Close()

	s := newTestSink(t, baseConfig(), testLocation(), srv.URL,
		slog.New(slog.NewTextHandler(io.Discard, nil)))

	// The first publish reaches the server and is refused.
	if err := s.Publish(context.Background(), testReading()); err == nil {
		t.Fatal("expected the first submission to fail")
	}
	if n := calls.Load(); n != 1 {
		t.Fatalf("first publish made %d requests, want 1", n)
	}

	// Every publish during the cooldown must be skipped locally, with no
	// request at all.
	for i := 0; i < 5; i++ {
		err := s.Publish(context.Background(), testReading())
		if !errors.Is(err, sink.ErrSkipped) {
			t.Fatalf("publish %d during cooldown returned %v, want sink.ErrSkipped", i, err)
		}
	}
	if n := calls.Load(); n != 1 {
		t.Fatalf("server was called %d times; the cooldown must suppress requests entirely", n)
	}

	// A skip is not a failure: the fan-out counts it separately, so it must
	// not look like an error the operator should act on.
	if err := s.Publish(context.Background(), testReading()); !errors.Is(err, sink.ErrSkipped) {
		t.Fatalf("got %v, want sink.ErrSkipped", err)
	}
}

// TestCooldownBacksOffAndResets checks the cooldown grows while refusals
// continue and clears once a submission lands.
func TestCooldownBacksOffAndResets(t *testing.T) {
	s := newTestSink(t, baseConfig(), testLocation(), "http://127.0.0.1:1",
		slog.New(slog.NewTextHandler(io.Discard, nil)))

	s.noteRateLimited()
	first := s.cooldown
	if first != minRateLimitCooldown {
		t.Fatalf("first cooldown = %s, want %s", first, minRateLimitCooldown)
	}

	s.noteRateLimited()
	if s.cooldown != 2*first {
		t.Fatalf("second cooldown = %s, want %s", s.cooldown, 2*first)
	}

	// It must not grow without bound.
	for i := 0; i < 20; i++ {
		s.noteRateLimited()
	}
	if s.cooldown > maxRateLimitCooldown {
		t.Fatalf("cooldown grew to %s, above the %s cap", s.cooldown, maxRateLimitCooldown)
	}

	// A successful submission clears it, so one bad patch does not suppress
	// the sink forever.
	s.noteAccepted()
	if _, cooling := s.coolingDown(); cooling {
		t.Fatal("a successful submission should clear the cooldown")
	}
}
