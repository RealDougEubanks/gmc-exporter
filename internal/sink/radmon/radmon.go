// Package radmon publishes readings to radmon.org.
//
// The radmon.org API is documented only in a forum thread. As of the
// 2026-05-24 revision of that thread it is a single GET endpoint,
// https://radmon.org/radmon.php, dispatched on a function parameter. There is
// no POST form and no JSON: every parameter, credentials included, travels in
// the query string, and success is signalled by the literal body "OK" rather
// than by the status code.
//
// Two consequences shape this file. First, a 200 response is not evidence of
// success, so the body is always inspected. Second, the data-sending password
// is in the URL, which means any error escaping this package unexamined can
// write that password into a log; every error from the HTTP client therefore
// goes through redact.Error before it is wrapped, returned, or logged.
//
// The same thread records that all graph-drawing functions were disabled on
// 2026-05-24 following abuse, so nothing here depends on them.
package radmon

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
	// defaultEndpoint is the single entry point for every radmon.org API
	// function.
	defaultEndpoint = "https://radmon.org/radmon.php"

	// functionSubmit publishes a reading; functionSubmitLatLng publishes the
	// same reading with coordinates attached.
	functionSubmit       = "submit"
	functionSubmitLatLng = "submitwithlatlng"

	// functionPing answers "pong" without touching the public dataset, which
	// makes it the only safe way to test connectivity and credentials.
	functionPing = "ping"

	// unitCPM is the only unit radmon.org accepts. The thread is explicit:
	// "the unit should always be CPM".
	unitCPM = "CPM"

	// responseOK is the exact body a successful submission returns.
	responseOK = "OK"

	// responsePong is the exact body functionPing returns.
	responsePong = "pong"

	// maxBodyBytes bounds the response read. The endpoint answers with a short
	// status word, so anything larger is a captive portal or an error page, and
	// reading it whole would let a hostile or broken upstream exhaust memory.
	maxBodyBytes = 4 << 10

	// bodyExcerptRunes bounds how much of an unexpected body reaches an error
	// message, since that message is destined for a log.
	bodyExcerptRunes = 120
)

// Sink publishes readings to radmon.org.
type Sink struct {
	// endpoint is a field rather than a constant so tests can aim the sink at
	// an httptest server. Nothing outside this package can change it, so
	// production traffic can only ever go to radmon.org.
	endpoint  string
	user      string
	password  redact.Secret
	useLatLng bool
	latitude  float64
	longitude float64
	retries   int
	client    *http.Client
	log       *slog.Logger
}

// errPermanent marks a failure that retrying cannot fix. Bad credentials are
// the motivating case: the request will fail identically every time, and
// repeating it against a small volunteer-run server is abuse rather than
// resilience.
var errPermanent = errors.New("radmon: permanent failure")

// New builds a sink from validated configuration.
//
// Coordinates are required up front when UseLatLng is set rather than being
// defaulted at publish time, because a default would attribute readings to a
// place the detector is not.
func New(cfg config.Radmon, loc config.Location, log *slog.Logger) (*Sink, error) {
	if log == nil {
		log = slog.Default()
	}
	if strings.TrimSpace(cfg.User) == "" {
		return nil, errors.New("radmon: GOGMC_RADMON_USER is required")
	}
	if cfg.Password.IsZero() {
		return nil, errors.New("radmon: GOGMC_RADMON_PASSWORD is required " +
			"(this is radmon's data-sending password, not the account login password)")
	}
	if cfg.UseLatLng && !loc.Valid {
		return nil, errors.New("radmon: GOGMC_RADMON_USE_LATLNG requires a location; " +
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
		user:      cfg.User,
		password:  cfg.Password,
		useLatLng: cfg.UseLatLng,
		latitude:  loc.Latitude,
		longitude: loc.Longitude,
		retries:   retries,
		client:    &http.Client{Timeout: timeout},
		log:       log.With("sink", "radmon"),
	}, nil
}

// Name identifies the sink in logs and metrics.
func (s *Sink) Name() string { return "radmon" }

// Publish sends the reading's raw CPM count.
//
// radmon.org stores counts per minute only. The dose rate and the running
// average this exporter also computes have nowhere to go here, so they are
// deliberately dropped rather than approximated into a field that means
// something else.
func (s *Sink) Publish(ctx context.Context, r reading.Reading) error {
	fn := functionSubmit
	params := url.Values{
		"value": {strconv.FormatUint(uint64(r.CPM), 10)},
		"unit":  {unitCPM},
	}
	if s.useLatLng {
		fn = functionSubmitLatLng
		params.Set("latitude", strconv.FormatFloat(s.latitude, 'f', -1, 64))
		params.Set("longitude", strconv.FormatFloat(s.longitude, 'f', -1, 64))
	}

	return s.call(ctx, fn, params, responseOK)
}

// Ping checks connectivity and reachability without submitting a reading.
//
// It exists so startup validation and operator troubleshooting do not have to
// push a fabricated measurement into a public dataset to learn whether the
// endpoint is reachable.
func (s *Sink) Ping(ctx context.Context) error {
	return s.call(ctx, functionPing, url.Values{}, responsePong)
}

// Close releases resources. The sink holds only an http.Client, so there is
// nothing to release, but idle connections are dropped so a stopped exporter
// leaves no sockets behind.
func (s *Sink) Close() error {
	s.client.CloseIdleConnections()
	return nil
}

// call performs one API function with bounded retries, returning nil only when
// the response body matches want.
//
// Backoff doubles from a short base. The exporter polls on an interval measured
// in tens of seconds, so retrying is worth a few seconds at most; beyond that
// the next poll carries a fresher reading than the one being retried.
func (s *Sink) call(ctx context.Context, fn string, params url.Values, want string) error {
	const baseDelay = 500 * time.Millisecond

	attempts := s.retries + 1
	var lastErr error

	for attempt := 1; attempt <= attempts; attempt++ {
		if err := ctx.Err(); err != nil {
			if lastErr != nil {
				return fmt.Errorf("radmon: %s abandoned after %d attempt(s): %w", fn, attempt-1, lastErr)
			}
			return fmt.Errorf("radmon: %s cancelled: %w", fn, err)
		}

		err := s.attempt(ctx, fn, params, want)
		if err == nil {
			return nil
		}
		lastErr = err

		// A permanent failure is returned immediately: retrying bad
		// credentials cannot succeed and only adds load.
		if errors.Is(err, errPermanent) {
			return err
		}
		if attempt == attempts {
			break
		}

		delay := baseDelay << (attempt - 1)
		s.log.Debug("radmon request failed, retrying",
			"function", fn, "attempt", attempt, "of", attempts, "delay", delay, "error", err)

		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return fmt.Errorf("radmon: %s abandoned after %d attempt(s): %w", fn, attempt, lastErr)
		case <-timer.C:
		}
	}

	return fmt.Errorf("radmon: %s failed after %d attempt(s): %w", fn, attempts, lastErr)
}

// attempt performs a single request and validates its body.
func (s *Sink) attempt(ctx context.Context, fn string, params url.Values, want string) error {
	// The query is assembled per attempt so the password lives in a local
	// string rather than in a field that could be logged with the struct.
	q := url.Values{}
	for k, v := range params {
		q[k] = v
	}
	q.Set("function", fn)
	q.Set("user", s.user)
	q.Set("password", s.password.Reveal())

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.endpoint+"?"+q.Encode(), nil)
	if err != nil {
		// Even request construction failures can quote the URL.
		return redact.Error(err)
	}

	resp, err := s.client.Do(req)
	if err != nil {
		// The single most important line in this file. http.Client returns
		// *url.Error, which carries the full URL — password included — in its
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
	text := strings.TrimSpace(string(body))

	// 401 and 403 name the credentials directly; 400 and 404 mean the request
	// shape is wrong. None of them improve on a second try.
	switch resp.StatusCode {
	case http.StatusUnauthorized, http.StatusForbidden:
		return fmt.Errorf("%w: status %d, check the radmon data-sending password: %s",
			errPermanent, resp.StatusCode, s.excerpt(text))
	case http.StatusBadRequest, http.StatusNotFound:
		return fmt.Errorf("%w: status %d: %s", errPermanent, resp.StatusCode, s.excerpt(text))
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return fmt.Errorf("status %d: %s", resp.StatusCode, s.excerpt(text))
	}

	// radmon.org answers credential and validation errors with 200 and an
	// error sentence in the body, so the status code alone proves nothing.
	if !strings.EqualFold(text, want) {
		if looksLikeAuthFailure(text) {
			return fmt.Errorf("%w: rejected, check the radmon data-sending password: %s",
				errPermanent, s.excerpt(text))
		}
		if looksLikeRateLimit(text) {
			return fmt.Errorf("%w: submitted too soon after the previous reading, "+
				"this reading is skipped: %s", errPermanent, s.excerpt(text))
		}
		return fmt.Errorf("unexpected response body, wanted %q: %s", want, s.excerpt(text))
	}
	return nil
}

// looksLikeRateLimit reports whether a 200 body is radmon.org rejecting a
// submission for arriving too soon after the previous one.
//
// This is treated as permanent for the current reading, which is the opposite
// of the usual instinct. Retrying a rate limit cannot succeed — the server is
// saying "not yet", and three attempts two seconds apart is simply three
// rejections instead of one. Worse, it is rude to a free public service run for
// the community. The right response is to drop this reading and let the next
// poll submit on schedule.
//
// Observed in production against a real account when another exporter had
// submitted moments earlier: the body was "Too soon <br>".
func looksLikeRateLimit(body string) bool {
	lower := strings.ToLower(body)
	for _, phrase := range []string{
		"too soon",
		"too fast",
		"too many",
		"rate limit",
		"slow down",
	} {
		if strings.Contains(lower, phrase) {
			return true
		}
	}
	return false
}

// looksLikeAuthFailure reports whether a 200 body is radmon.org refusing the
// credentials.
//
// The forum thread documents the success strings but not the failure strings,
// so this matches on the wording radmon.org is observed to use. A miss only
// costs a few pointless retries; the check exists to stop the common case of an
// exporter hammering the endpoint with a password that will never work.
func looksLikeAuthFailure(body string) bool {
	lower := strings.ToLower(body)
	for _, phrase := range []string{
		"incorrect login",
		"incorrect password",
		"wrong password",
		"bad password",
		"invalid password",
		"invalid user",
		"unknown user",
		"no such user",
		"login failed",
		"access denied",
		"not authorised",
		"not authorized",
	} {
		if strings.Contains(lower, phrase) {
			return true
		}
	}
	return false
}

// excerpt trims a response body down to something loggable.
//
// The body is remote input that ends up in an error message and therefore in a
// log line, so it is length-bounded and quoted rather than interpolated raw.
//
// It also scrubs the password. redact.Error covers the URL that http.Client
// puts in transport errors, but a server that quotes the request back in its
// error page — which some error pages and every proxy interstitial do — would
// otherwise hand the credential straight back for logging. The credential must
// not escape by any path, not just the expected one.
func (s *Sink) excerpt(body string) string {
	if body == "" {
		return `""`
	}
	// Scrub before truncating: truncating first could cut the password in half
	// and leave a fragment that no longer matches, but still discloses most of
	// the credential.
	if secret := s.password.Reveal(); secret != "" {
		body = strings.ReplaceAll(body, secret, redact.Placeholder)
		body = strings.ReplaceAll(body, url.QueryEscape(secret), redact.Placeholder)
	}
	runes := []rune(body)
	if len(runes) > bodyExcerptRunes {
		body = string(runes[:bodyExcerptRunes]) + "..."
	}
	return strconv.Quote(body)
}
