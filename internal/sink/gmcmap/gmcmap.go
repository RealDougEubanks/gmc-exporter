// Package gmcmap publishes readings to GQ Electronics' own radiation map,
// GMCMAP.com.
//
// The submission API is documented in GQ's own device user guides. It is a
// single GET endpoint, https://www.gmcmap.com/log2.asp, with every value —
// credentials included — carried in the query string. There is no POST form, no
// JSON, and no header authentication: the account ID (AID) and the geiger
// counter ID (GID) are the credentials, and together they select which station
// on the public map a reading is attributed to. There is also no timestamp
// parameter; the server timestamps on receipt, so a reading cannot be
// backfilled and a delayed submission is recorded as if it had just happened.
//
// Two consequences shape this file.
//
// First, the endpoint answers with HTTP 200 for both success and failure, and
// encodes the real outcome only as an ERRn token inside a short HTML fragment.
// The observed bodies are "OK.ERR0" on success, "Error! User not found.ERR1."
// for an unknown account, "Error! Geiger Counter not found.ERR2." for an
// unknown counter, and "Error! Data Error!(CPM)ERR4." for a malformed value.
// Success is therefore determined by finding ERR0, never by the status code and
// never by the prose, which varies and contains typos. A success can also carry
// a "please update/confirm your location" warning alongside ERR0; that is not a
// failure and is resolved on the website, not by the client.
//
// Second, the counter ID is in the URL, which means any error escaping this
// package unexamined can write that credential into a log; every error from the
// HTTP client therefore goes through redact.Error before it is wrapped,
// returned, or logged.
package gmcmap

import (
	"context"
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
	// defaultEndpoint is GQ Electronics' documented submission URL. log2.asp
	// supersedes the older log.asp, which accepted only CPM.
	defaultEndpoint = "https://www.gmcmap.com/log2.asp"

	// The parameter names are spelled exactly as GQ's URL examples write them.
	// The dose-rate parameter is the mixed-case "uSV": GQ's prose calls it
	// "uSv", but every documented example and every working client sends "uSV".
	// The endpoint is Classic ASP, whose query lookup is case-insensitive, so
	// this almost certainly does not matter — but matching the documentation
	// costs nothing and removes the question.
	paramAccountID = "AID"
	paramCounterID = "GID"
	paramCPM       = "CPM"
	paramAverage   = "ACPM"
	paramDoseRate  = "uSV"

	// paramTemperature is one of the extended parameters documented in the
	// GMC-510/520 guide, which spells the extended set in lower case unlike the
	// core five. It is sent only when the device actually supplied a reading.
	paramTemperature = "tmp"

	// errSuccess is the token that marks an accepted submission. The body is an
	// HTML fragment, so the token is searched for rather than compared against
	// the whole body.
	errSuccess = "ERR0"

	// errUserNotFound and errCounterNotFound identify credentials the server
	// does not recognise, and errDataError identifies a value it could not
	// parse. All three are faults in this exporter's configuration or payload
	// and cannot be fixed by sending the same request again.
	errUserNotFound    = "ERR1"
	errCounterNotFound = "ERR2"
	errDataError       = "ERR4"

	// userAgent identifies this exporter to the dataset operator, so a
	// misbehaving deployment can be recognised and contacted rather than
	// silently blocked.
	userAgent = "gmc-exporter (+https://github.com/RealDougEubanks/gmc-exporter)"

	// maxBodyBytes bounds the response read. A submission answers with a short
	// page, so anything larger is a captive portal or an error page, and
	// reading it whole would let a hostile or broken upstream exhaust memory.
	maxBodyBytes = 16 << 10

	// bodyExcerptRunes bounds how much of an unexpected body reaches an error
	// message, since that message is destined for a log.
	bodyExcerptRunes = 160
)

// Sink publishes readings to GMCMAP.com.
type Sink struct {
	// endpoint is a field rather than a constant so tests can aim the sink at
	// an httptest server. Nothing outside this package can change it, so
	// production traffic can only ever go to gmcmap.com.
	endpoint  string
	accountID string
	counterID redact.Secret
	latitude  float64
	longitude float64
	retries   int
	client    *http.Client
	log       *slog.Logger
}

// errPermanent marks a failure that retrying cannot fix. A rejected account or
// counter ID is the motivating case: the request will fail identically every
// time, and repeating it only adds load to someone else's server.
var errPermanent = errors.New("gmcmap: permanent failure")

// New builds a sink from validated configuration.
//
// A location is mandatory. GMCMAP plots every station on a public map, and the
// coordinates the account was registered with are what the reading is pinned
// to, so an exporter that cannot say where it is has no business publishing.
func New(cfg config.GMCMap, loc config.Location, log *slog.Logger) (*Sink, error) {
	if log == nil {
		log = slog.Default()
	}
	if strings.TrimSpace(cfg.AccountID) == "" {
		return nil, errors.New("gmcmap: GOGMC_GMCMAP_ACCOUNT_ID is required")
	}
	if cfg.CounterID.IsZero() {
		return nil, errors.New("gmcmap: GOGMC_GMCMAP_COUNTER_ID is required " +
			"(this is the geiger counter ID GMCMAP issued for the device, not the account ID)")
	}
	if !loc.Valid {
		return nil, errors.New("gmcmap: a location is required; " +
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
		accountID: cfg.AccountID,
		counterID: cfg.CounterID,
		latitude:  loc.Latitude,
		longitude: loc.Longitude,
		retries:   retries,
		client:    &http.Client{Timeout: timeout},
		log:       log.With("sink", "gmcmap"),
	}, nil
}

// Name identifies the sink in logs and metrics.
func (s *Sink) Name() string { return "gmcmap" }

// Publish sends the count rate, the running average and the dose rate.
//
// All three are sent together because GMCMAP stores them as one record: CPM is
// the instantaneous count, ACPM the averaged count, and uSV the dose rate
// derived from the device's own calibration table. Sending the dose rate this
// exporter already computed is preferable to letting the map re-derive it from
// CPM with a calibration factor that may not match this tube.
//
// Temperature is attached only when the device supplied one. Omitting the
// parameter entirely is the correct way to say "not measured": the extended
// parameters use -1 as a no-reading sentinel, and -1 is a legitimate
// temperature in degrees Celsius.
func (s *Sink) Publish(ctx context.Context, r reading.Reading) error {
	params := url.Values{
		paramCPM:      {strconv.FormatUint(uint64(r.CPM), 10)},
		paramAverage:  {formatFloat(r.AverageCPM)},
		paramDoseRate: {formatFloat(r.MicroSievertsPerHour)},
	}
	if r.TemperatureC.Valid {
		params.Set(paramTemperature, formatFloat(r.TemperatureC.Value))
	}
	return s.submit(ctx, params)
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
func (s *Sink) submit(ctx context.Context, params url.Values) error {
	const baseDelay = 500 * time.Millisecond

	attempts := s.retries + 1
	var lastErr error

	for attempt := 1; attempt <= attempts; attempt++ {
		if err := ctx.Err(); err != nil {
			if lastErr != nil {
				return fmt.Errorf("gmcmap: submission abandoned after %d attempt(s): %w", attempt-1, lastErr)
			}
			return fmt.Errorf("gmcmap: submission cancelled: %w", err)
		}

		err := s.attempt(ctx, params)
		if err == nil {
			return nil
		}
		lastErr = err

		// A permanent failure is returned immediately: retrying a rejected
		// account or counter ID cannot succeed and only adds load.
		if errors.Is(err, errPermanent) {
			return err
		}
		if attempt == attempts {
			break
		}

		delay := baseDelay << (attempt - 1)
		s.log.Debug("gmcmap submission failed, retrying",
			"attempt", attempt, "of", attempts, "delay", delay, "error", err)

		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return fmt.Errorf("gmcmap: submission abandoned after %d attempt(s): %w", attempt, lastErr)
		case <-timer.C:
		}
	}

	return fmt.Errorf("gmcmap: submission failed after %d attempt(s): %w", attempts, lastErr)
}

// attempt performs a single request and validates its body.
func (s *Sink) attempt(ctx context.Context, params url.Values) error {
	// The query is assembled per attempt so the counter ID lives in a local
	// string rather than in a field that could be logged with the struct.
	q := url.Values{}
	for k, v := range params {
		q[k] = v
	}
	q.Set(paramAccountID, s.accountID)
	q.Set(paramCounterID, s.counterID.Reveal())

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.endpoint+"?"+q.Encode(), nil)
	if err != nil {
		// Even request construction failures can quote the URL.
		return redact.Error(err)
	}
	req.Header.Set("User-Agent", userAgent)

	resp, err := s.client.Do(req)
	if err != nil {
		// The single most important line in this file. http.Client returns
		// *url.Error, which carries the full URL — counter ID included — in its
		// message.
		return redact.Error(err)
	}
	defer func() {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxBodyBytes))
		_ = resp.Body.Close()
	}()

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBodyBytes))
	if err != nil {
		return fmt.Errorf("reading response: %w", redact.Error(err))
	}
	text := strings.TrimSpace(stripTags(string(body)))

	// 401 and 403 name the credentials directly; 400 and 404 mean the request
	// shape is wrong. None of them improve on a second try.
	switch resp.StatusCode {
	case http.StatusUnauthorized, http.StatusForbidden:
		return fmt.Errorf("%w: status %d, check the GMCMAP account and counter IDs: %s",
			errPermanent, resp.StatusCode, excerpt(text))
	case http.StatusBadRequest, http.StatusNotFound:
		return fmt.Errorf("%w: status %d: %s", errPermanent, resp.StatusCode, excerpt(text))
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return fmt.Errorf("status %d: %s", resp.StatusCode, excerpt(text))
	}

	// GMCMAP answers a rejected submission with 200 and an ERRn token in the
	// page body, so the status code alone proves nothing.
	return classify(text)
}

// classify turns a 200 response body into a result.
//
// The ERR0 test comes first and is a plain substring match, because a
// successful submission may carry a location-confirmation warning ahead of the
// token. The three known failure codes are reported as permanent: two name
// credentials the server does not recognise and the third names a value it
// could not parse, and none of them will behave differently on a second
// attempt.
func classify(body string) error {
	upper := strings.ToUpper(body)

	if strings.Contains(upper, errSuccess) {
		return nil
	}

	switch {
	case strings.Contains(upper, errUserNotFound):
		return fmt.Errorf("%w: account not recognised, check GOGMC_GMCMAP_ACCOUNT_ID: %s",
			errPermanent, excerpt(body))
	case strings.Contains(upper, errCounterNotFound):
		return fmt.Errorf("%w: counter not recognised, check GOGMC_GMCMAP_COUNTER_ID "+
			"and that it is registered to this account: %s", errPermanent, excerpt(body))
	case strings.Contains(upper, errDataError):
		// The server names the offending field in parentheses, so the excerpt
		// is the whole diagnostic. This is a bug in what this exporter sent.
		return fmt.Errorf("%w: server rejected a submitted value: %s", errPermanent, excerpt(body))
	}

	// An unrecognised body is treated as transient. GQ documents no exhaustive
	// code list, and a proxy or an outage page is far likelier here than a new
	// permanent error, so the next attempt is allowed to proceed.
	return fmt.Errorf("unexpected response body, wanted a %s token: %s", errSuccess, excerpt(body))
}

// stripTags removes HTML markup so the meaningful text of a response can be
// matched and logged.
//
// This is deliberately not a parser. The endpoint returns a handful of tags
// around a status sentence, and the only requirement is that the sentence
// survives and the angle brackets do not reach a log line.
func stripTags(body string) string {
	var b strings.Builder
	b.Grow(len(body))
	depth := 0
	for _, r := range body {
		switch {
		case r == '<':
			depth++
		case r == '>' && depth > 0:
			depth--
			b.WriteByte(' ')
		case depth == 0:
			b.WriteRune(r)
		}
	}
	return strings.Join(strings.Fields(b.String()), " ")
}

// formatFloat renders a measurement without an exponent or trailing zeros, so
// the query string carries the plainest form the server is likely to parse.
func formatFloat(v float64) string {
	return strconv.FormatFloat(v, 'f', -1, 64)
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
