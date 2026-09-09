// Package safecast publishes readings to the Safecast open radiation dataset.
//
// Safecast is the citizen-science dataset assembled after the 2011 Fukushima
// Daiichi accident, and its API is the Rails application at
// github.com/Safecast/safecastapi. The contract implemented here was read from
// that source rather than from prose documentation, because the prose is older
// than the code:
//
//   - POST https://api.safecast.org/measurements.json
//   - The API key authenticates via Devise token_authenticatable with the token
//     key renamed to api_key, so it travels as a request parameter. There is no
//     Authorization-header path in the application.
//   - The measurement body may be flat JSON; the controller wraps it into a
//     "measurement" key itself. It is sent nested here anyway, since that is the
//     shape both the controller specs and the HTML form use.
//   - value, unit, latitude and longitude are required. The unit for a dose rate
//     is the literal string "usv", which the UI labels μSv/h.
//   - A successful create answers 201 Created. Validation failures answer 422
//     with an "errors" object.
//
// The API key is in the query string, so any error escaping this package
// unexamined can write that key into a log. Every error from the HTTP client
// therefore goes through redact.Error before it is wrapped, returned, or
// logged.
//
// One Safecast-specific hazard shapes the retry logic. The measurement model
// carries a uniqueness constraint on an MD5 of value, latitude, longitude and
// captured_at, so re-sending an accepted measurement is rejected as a duplicate
// rather than silently ignored. A duplicate is therefore treated as success:
// the reading is already in the dataset, which is the outcome the caller wanted.
package safecast

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/RealDougEubanks/gmc-exporter/internal/config"
	"github.com/RealDougEubanks/gmc-exporter/internal/reading"
	"github.com/RealDougEubanks/gmc-exporter/internal/redact"
)

const (
	// defaultEndpoint is the measurement create endpoint. The .json suffix is
	// what selects the JSON response format in Rails.
	defaultEndpoint = "https://api.safecast.org/measurements.json"

	// paramAPIKey is the query parameter Devise is configured to read the
	// authentication token from.
	paramAPIKey = "api_key"

	// unitMicroSievertsPerHour is the exact string Safecast stores for a dose
	// rate. The web form labels it μSv/h but submits "usv".
	//
	// This value is not validated server-side: a wrong unit string is accepted
	// and silently corrupts the dataset rather than returning an error, which
	// is why it is a named constant matched by a test rather than an inline
	// literal.
	unitMicroSievertsPerHour = "usv"

	// capturedAtLayout is RFC3339 in UTC, which the Swagger input schema
	// declares as a date-time and which Rails parses unambiguously.
	capturedAtLayout = time.RFC3339

	// userAgent identifies this exporter to the dataset operator, so a
	// misbehaving deployment can be recognised and contacted rather than
	// silently blocked.
	userAgent = "gmc-exporter (+https://github.com/RealDougEubanks/gmc-exporter)"

	// maxBodyBytes bounds the response read. A created measurement is a small
	// JSON object, so anything larger is an error page or a captive portal, and
	// reading it whole would let a hostile or broken upstream exhaust memory.
	maxBodyBytes = 32 << 10

	// bodyExcerptRunes bounds how much of an unexpected body reaches an error
	// message, since that message is destined for a log.
	bodyExcerptRunes = 200
)

// Sink publishes readings to api.safecast.org.
type Sink struct {
	// endpoint is a field rather than a constant so tests can aim the sink at
	// an httptest server. Nothing outside this package can change it, so
	// production traffic can only ever go to api.safecast.org.
	endpoint  string
	apiKey    redact.Secret
	deviceID  int
	latitude  float64
	longitude float64
	retries   int
	client    *http.Client
	log       *slog.Logger
}

// errPermanent marks a failure that retrying cannot fix. A rejected API key and
// a validation error are the motivating cases: both will fail identically every
// time, and repeating them only adds load to a volunteer-funded service.
var errPermanent = errors.New("safecast: permanent failure")

// measurement is the request body.
//
// Fields are omitted when unset rather than sent as zero, because zero is a
// meaningful device ID and a meaningful coordinate, and Safecast has no way to
// tell a real zero from an unfilled one.
type measurement struct {
	Value      float64 `json:"value"`
	Unit       string  `json:"unit"`
	Latitude   float64 `json:"latitude"`
	Longitude  float64 `json:"longitude"`
	CapturedAt string  `json:"captured_at"`
	DeviceID   *int    `json:"device_id,omitempty"`
}

// requestBody is the nested envelope the controller specs and the web form use.
type requestBody struct {
	Measurement measurement `json:"measurement"`
}

// errorResponse is the shape Rails returns for a validation failure. The values
// are per-field message lists.
type errorResponse struct {
	Errors map[string][]string `json:"errors"`
}

// New builds a sink from validated configuration.
//
// A location is mandatory. Safecast is a public dataset whose entire value is
// that every point is somewhere real, so an exporter that cannot say where it
// is must not contribute. Defaulting the coordinates would put a fabricated
// point on a map researchers rely on.
func New(cfg config.Safecast, loc config.Location, log *slog.Logger) (*Sink, error) {
	if log == nil {
		log = slog.Default()
	}
	if cfg.APIKey.IsZero() {
		return nil, errors.New("safecast: GOGMC_SAFECAST_API_KEY is required " +
			"(find it on your api.safecast.org profile page)")
	}
	if !loc.Valid {
		return nil, errors.New("safecast: a location is required; " +
			"set GOGMC_LATITUDE and GOGMC_LONGITUDE (note these are published publicly)")
	}

	timeout := cfg.Timeout
	if timeout <= 0 {
		timeout = 15 * time.Second
	}
	retries := cfg.Retries
	if retries < 0 {
		retries = 0
	}

	return &Sink{
		endpoint:  defaultEndpoint,
		apiKey:    cfg.APIKey,
		deviceID:  cfg.DeviceID,
		latitude:  loc.Latitude,
		longitude: loc.Longitude,
		retries:   retries,
		client:    &http.Client{Timeout: timeout},
		log:       log.With("sink", "safecast"),
	}, nil
}

// Name identifies the sink in logs and metrics.
func (s *Sink) Name() string { return "safecast" }

// Publish sends the dose rate.
//
// Safecast stores one value per measurement, so only the µSv/h figure is sent.
// The raw count rate is deliberately not submitted as a second measurement: CPM
// is meaningful only alongside the tube's calibration, and a count from an
// unregistered tube type would be an unusable point in a dataset built for
// cross-device comparison.
func (s *Sink) Publish(ctx context.Context, r reading.Reading) error {
	capturedAt := r.Timestamp
	if capturedAt.IsZero() {
		capturedAt = time.Now()
	}

	m := measurement{
		Value:      r.MicroSievertsPerHour,
		Unit:       unitMicroSievertsPerHour,
		Latitude:   s.latitude,
		Longitude:  s.longitude,
		CapturedAt: capturedAt.UTC().Format(capturedAtLayout),
	}
	// A device ID of zero means "not configured": Safecast issues positive IDs
	// for registered hardware.
	if s.deviceID > 0 {
		id := s.deviceID
		m.DeviceID = &id
	}

	body, err := json.Marshal(requestBody{Measurement: m})
	if err != nil {
		return fmt.Errorf("safecast: encoding measurement: %w", err)
	}
	return s.submit(ctx, body)
}

// Close releases resources. The sink holds only an http.Client, so there is
// nothing to release, but idle connections are dropped so a stopped exporter
// leaves no sockets behind.
func (s *Sink) Close() error {
	s.client.CloseIdleConnections()
	return nil
}

// submit performs one submission with bounded retries.
//
// Backoff doubles from a short base. The exporter polls on an interval measured
// in tens of seconds, so retrying is worth a few seconds at most; beyond that
// the next poll carries a fresher reading than the one being retried.
func (s *Sink) submit(ctx context.Context, body []byte) error {
	const baseDelay = 500 * time.Millisecond

	attempts := s.retries + 1
	var lastErr error

	for attempt := 1; attempt <= attempts; attempt++ {
		if err := ctx.Err(); err != nil {
			if lastErr != nil {
				return fmt.Errorf("safecast: submission abandoned after %d attempt(s): %w", attempt-1, lastErr)
			}
			return fmt.Errorf("safecast: submission cancelled: %w", err)
		}

		err := s.attempt(ctx, body)
		if err == nil {
			return nil
		}
		lastErr = err

		// A permanent failure is returned immediately: retrying a rejected API
		// key or an invalid measurement cannot succeed and only adds load.
		if errors.Is(err, errPermanent) {
			return err
		}
		if attempt == attempts {
			break
		}

		delay := baseDelay << (attempt - 1)
		s.log.Debug("safecast submission failed, retrying",
			"attempt", attempt, "of", attempts, "delay", delay, "error", err)

		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return fmt.Errorf("safecast: submission abandoned after %d attempt(s): %w", attempt, lastErr)
		case <-timer.C:
		}
	}

	return fmt.Errorf("safecast: submission failed after %d attempt(s): %w", attempts, lastErr)
}

// attempt performs a single request and validates the response.
func (s *Sink) attempt(ctx context.Context, body []byte) error {
	// The URL is assembled per attempt so the API key lives in a local string
	// rather than in a field that could be logged with the struct.
	q := url.Values{paramAPIKey: {s.apiKey.Reveal()}}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		s.endpoint+"?"+q.Encode(), bytes.NewReader(body))
	if err != nil {
		// Even request construction failures can quote the URL.
		return redact.Error(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", userAgent)

	resp, err := s.client.Do(req)
	if err != nil {
		// The single most important line in this file. http.Client returns
		// *url.Error, which carries the full URL — API key included — in its
		// message.
		return redact.Error(err)
	}
	defer func() {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxBodyBytes))
		_ = resp.Body.Close()
	}()

	respBody, err := io.ReadAll(io.LimitReader(resp.Body, maxBodyBytes))
	if err != nil {
		return fmt.Errorf("reading response: %w", redact.Error(err))
	}
	text := strings.TrimSpace(string(respBody))

	switch {
	case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
		return fmt.Errorf("%w: status %d, check GOGMC_SAFECAST_API_KEY: %s",
			errPermanent, resp.StatusCode, excerpt(text))

	case resp.StatusCode == http.StatusUnprocessableEntity:
		// A duplicate is Safecast telling us the measurement is already stored,
		// which is the outcome Publish wanted. Reporting it as a failure would
		// turn a successful retry into a permanent error and an alert.
		if isDuplicate(text) {
			s.log.Debug("safecast rejected a duplicate measurement, treating as already published")
			return nil
		}
		return fmt.Errorf("%w: measurement rejected: %s", errPermanent, excerpt(summarizeErrors(text)))

	case resp.StatusCode == http.StatusBadRequest || resp.StatusCode == http.StatusNotFound:
		return fmt.Errorf("%w: status %d: %s", errPermanent, resp.StatusCode, excerpt(text))

	case resp.StatusCode < 200 || resp.StatusCode > 299:
		return fmt.Errorf("status %d: %s", resp.StatusCode, excerpt(text))
	}

	// A 2xx that carries an errors object is a failure whatever the status line
	// says. Rails ordinarily uses 422 for this, but a proxy or a future change
	// could put the same payload behind a 200, and silently counting that as a
	// published reading would hide data loss.
	if hasErrors(text) {
		if isDuplicate(text) {
			return nil
		}
		return fmt.Errorf("%w: status %d carried an error payload: %s",
			errPermanent, resp.StatusCode, excerpt(summarizeErrors(text)))
	}
	return nil
}

// hasErrors reports whether a response body is a Rails validation-error object.
//
// It requires the errors member to be present and non-empty, so a created
// measurement that happens to include an empty errors key is not mistaken for a
// failure.
func hasErrors(body string) bool {
	var er errorResponse
	if err := json.Unmarshal([]byte(body), &er); err != nil {
		return false
	}
	for _, msgs := range er.Errors {
		if len(msgs) > 0 {
			return true
		}
	}
	return false
}

// isDuplicate reports whether a rejection is the md5sum uniqueness constraint.
//
// Safecast derives that checksum from the value, the coordinates and
// captured_at, so a retry of an accepted submission trips it. The message text
// is matched because Rails reports uniqueness failures as a field error rather
// than with a distinguishable status code.
func isDuplicate(body string) bool {
	lower := strings.ToLower(body)
	if !strings.Contains(lower, "taken") && !strings.Contains(lower, "duplicate") {
		return false
	}
	return strings.Contains(lower, "md5") || strings.Contains(lower, "duplicate")
}

// summarizeErrors renders a validation-error payload as a compact
// field: message list.
//
// A body that is not the expected shape is returned unchanged, so an
// unrecognised error page still reaches the log rather than being swallowed.
func summarizeErrors(body string) string {
	var er errorResponse
	if err := json.Unmarshal([]byte(body), &er); err != nil || len(er.Errors) == 0 {
		return body
	}

	fields := make([]string, 0, len(er.Errors))
	for field := range er.Errors {
		fields = append(fields, field)
	}
	// Sort for stable log lines, since Go randomises map iteration order.
	for i := 1; i < len(fields); i++ {
		for j := i; j > 0 && fields[j] < fields[j-1]; j-- {
			fields[j], fields[j-1] = fields[j-1], fields[j]
		}
	}

	var b strings.Builder
	for i, field := range fields {
		if i > 0 {
			b.WriteString("; ")
		}
		b.WriteString(field)
		b.WriteString(": ")
		b.WriteString(strings.Join(er.Errors[field], ", "))
	}
	return b.String()
}

// excerpt trims a response body down to something loggable.
//
// The body is remote input that ends up in an error message and therefore in a
// log line, so it is length-bounded and quoted rather than interpolated raw.
func excerpt(body string) string {
	if body == "" {
		return `""`
	}
	runes := []rune(body)
	if len(runes) > bodyExcerptRunes {
		return strconv.Quote(string(runes[:bodyExcerptRunes]) + "...")
	}
	return strconv.Quote(string(runes))
}
