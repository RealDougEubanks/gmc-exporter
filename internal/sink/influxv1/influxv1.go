// Package influxv1 publishes readings to InfluxDB 1.x.
//
// The 1.x write API is one endpoint that accepts line protocol as a plain
// request body and answers with a status code, so this sink speaks to it with
// net/http directly instead of pulling in a client library. The exporter ships
// in an image measured in tens of megabytes, and a dependency that exists to
// concatenate a string and POST it does not earn its size.
//
// InfluxDB 1.x takes its credentials as u= and p= query parameters. That puts a
// plaintext password inside every request URL, and Go embeds the request URL in
// the *url.Error it returns from a transport failure. Every error leaving this
// package therefore passes through redact.Error first.
package influxv1

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net/http"
	"net/url"
	"path"
	"strconv"
	"strings"
	"time"

	"github.com/RealDougEubanks/gmc-exporter/internal/config"
	"github.com/RealDougEubanks/gmc-exporter/internal/reading"
	"github.com/RealDougEubanks/gmc-exporter/internal/redact"
)

// sinkName is the stable identifier this sink reports, which also becomes a
// metric label.
const sinkName = "influxv1"

// maxBodySnippet bounds how much of a failure response is kept. Influx explains
// a rejected write in its body, and that explanation is the difference between a
// diagnosable failure and a bare status code, but an unbounded body from a
// misdirected request could be a whole HTML page.
const maxBodySnippet = 512

// defaultBackoff is the pause before the first retry, doubling from there. It is
// deliberately short: the poll loop will come round again anyway, so a sink that
// blocks for a long time buys nothing.
const defaultBackoff = 250 * time.Millisecond

// maxBackoff caps the exponential growth so a high retry count cannot outlive
// the poll interval.
const maxBackoff = 5 * time.Second

// Sink writes readings to a single InfluxDB 1.x database.
type Sink struct {
	measurement string
	fields      FieldNames
	tagDevice   bool
	attempts    int
	backoff     time.Duration

	// writeURL carries the credentials and must never be logged. safeURL is the
	// same endpoint with every query value replaced, and is what logs and error
	// messages are allowed to name.
	writeURL string
	safeURL  string

	client *http.Client
	log    *slog.Logger
}

// New builds a sink from validated configuration.
//
// The write URL, including its credentials, is assembled once here rather than
// per publish, so the password is formatted in exactly one place.
func New(cfg config.InfluxV1, log *slog.Logger) (*Sink, error) {
	if log == nil {
		log = slog.Default()
	}

	raw := strings.TrimSpace(cfg.URL)
	if raw == "" {
		return nil, fmt.Errorf("%s: URL is required", sinkName)
	}
	u, err := url.Parse(raw)
	if err != nil {
		// The parse error quotes the input, and an unparseable URL is exactly
		// the kind that might carry credentials, so it is not repeated here.
		return nil, fmt.Errorf("%s: URL is not a valid URL", sinkName)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return nil, fmt.Errorf("%s: URL scheme must be http or https, got %q", sinkName, u.Scheme)
	}
	if strings.TrimSpace(cfg.Database) == "" {
		return nil, fmt.Errorf("%s: database is required", sinkName)
	}

	measurement := strings.TrimSpace(cfg.Measurement)
	if measurement == "" {
		measurement = "radiation"
	}

	q := url.Values{}
	q.Set("db", cfg.Database)
	// Nanosecond precision matches the timestamps this exporter emits; sending a
	// nanosecond timestamp while Influx assumes its default would misplace every
	// point by decades.
	q.Set("precision", "ns")
	// Many home installations run InfluxDB unauthenticated. Sending empty u= and
	// p= to such a server is not harmless, so the parameters are omitted rather
	// than sent blank.
	if cfg.User != "" || !cfg.Password.IsZero() {
		q.Set("u", cfg.User)
		q.Set("p", cfg.Password.Reveal())
	}

	endpoint := *u
	endpoint.Path = path.Join(u.Path, "write")
	endpoint.RawQuery = q.Encode()

	timeout := cfg.Timeout
	if timeout <= 0 {
		timeout = 15 * time.Second
	}

	fields, err := FieldNamesFor(cfg.FieldStyle)
	if err != nil {
		return nil, fmt.Errorf("influxdb 1.x: GOGMC_INFLUX1_FIELD_STYLE: %w", err)
	}

	return &Sink{
		measurement: measurement,
		fields:      fields,
		tagDevice:   cfg.TagDevice,
		// Retries counts attempts after the first, so a configured zero still
		// makes one attempt.
		attempts: cfg.Retries + 1,
		backoff:  defaultBackoff,
		writeURL: endpoint.String(),
		safeURL:  redact.URL(endpoint.String()),
		client:   &http.Client{Timeout: timeout},
		log:      log,
	}, nil
}

// Name identifies the sink in logs and metrics.
func (s *Sink) Name() string { return sinkName }

// Publish writes one reading as a single line protocol point.
func (s *Sink) Publish(ctx context.Context, r reading.Reading) error {
	return s.write(ctx, lineProtocol(s.measurement, s.fields, s.tagDevice, r))
}

// Close releases pooled connections. It is safe on a sink that never published.
func (s *Sink) Close() error {
	s.client.CloseIdleConnections()
	return nil
}

// write posts the line, retrying failures that could plausibly succeed later.
func (s *Sink) write(ctx context.Context, line string) error {
	backoff := s.backoff
	var lastErr error

	for attempt := 1; attempt <= s.attempts; attempt++ {
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("%s: write to %s abandoned: %w", sinkName, s.safeURL, err)
		}

		retryable, err := s.attempt(ctx, line)
		if err == nil {
			return nil
		}
		lastErr = err

		if !retryable || attempt == s.attempts {
			break
		}

		s.log.Warn("influxdb 1.x write failed, retrying",
			"sink", sinkName, "url", s.safeURL, "attempt", attempt,
			"backoff", backoff, "error", err)

		select {
		case <-ctx.Done():
			return fmt.Errorf("%s: write to %s abandoned: %w: %w",
				sinkName, s.safeURL, ctx.Err(), lastErr)
		case <-time.After(backoff):
		}
		if backoff *= 2; backoff > maxBackoff {
			backoff = maxBackoff
		}
	}

	return fmt.Errorf("%s: write to %s failed: %w", sinkName, s.safeURL, lastErr)
}

// attempt performs one write and reports whether the failure is worth repeating.
func (s *Sink) attempt(ctx context.Context, line string) (retryable bool, err error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.writeURL, strings.NewReader(line))
	if err != nil {
		return false, redact.Error(err)
	}
	req.Header.Set("Content-Type", "text/plain; charset=utf-8")

	resp, err := s.client.Do(req)
	if err != nil {
		// This error embeds the request URL, password and all, until it has been
		// through redact.Error.
		return true, redact.Error(err)
	}
	defer func() { _ = resp.Body.Close() }()

	body := readSnippet(resp.Body)

	// Influx answers a successful write with 204 No Content, but any 2xx means
	// the point was accepted.
	if resp.StatusCode/100 == 2 {
		return false, nil
	}

	// The status alone rarely explains a rejected write; the body names the
	// offending field or the missing database.
	return retryableStatus(resp.StatusCode),
		fmt.Errorf("unexpected status %s: %s", resp.Status, body)
}

// retryableStatus reports whether repeating the request could change the answer.
//
// A 400 means the line protocol itself is malformed, and no amount of waiting
// will make the same bytes parse; the same reasoning covers an unauthorized or
// not-found response. Server errors and throttling are transient by nature.
func retryableStatus(code int) bool {
	return code == http.StatusTooManyRequests || code >= 500
}

// readSnippet reads a bounded prefix of a response body and drains the rest so
// the connection can be reused.
func readSnippet(body io.Reader) string {
	buf, err := io.ReadAll(io.LimitReader(body, maxBodySnippet+1))
	if err != nil && len(buf) == 0 {
		return "<unreadable body>"
	}
	_, _ = io.Copy(io.Discard, body)

	if len(buf) > maxBodySnippet {
		return strings.TrimSpace(string(buf[:maxBodySnippet])) + "..."
	}
	snippet := strings.TrimSpace(string(buf))
	if snippet == "" {
		return "<empty body>"
	}
	return snippet
}

// lineProtocol renders one reading as a single point.
//
// Optional values the device did not supply are omitted rather than written as
// zero: zero volts is a dead battery and zero degrees is a cold day, so a
// defaulted value would be indistinguishable from a real measurement.
func lineProtocol(measurement string, names FieldNames, tagDevice bool, r reading.Reading) string {
	var b strings.Builder

	b.WriteString(escapeMeasurement(measurement))

	// Device identity is written as tags rather than fields because it
	// labels the series rather than varying within it. Both values are
	// single-valued for a given instrument, so this adds no cardinality.
	if tagDevice {
		appendTag(&b, "serial", r.Device.Serial)
		appendTag(&b, "version", r.Device.Version)
	}

	b.WriteByte(' ')

	// CPM is an integer count and is typed as one so Influx does not create a
	// float column that later readings cannot be compared against.
	b.WriteString(escapeTag(names.CPM))
	b.WriteByte('=')
	b.WriteString(strconv.FormatUint(uint64(r.CPM), 10))
	b.WriteByte('i')

	appendFloatField(&b, names.AverageCPM, r.AverageCPM)
	appendFloatField(&b, names.MicroSieverts, r.MicroSievertsPerHour)
	if r.Voltage.Valid {
		appendFloatField(&b, names.Voltage, r.Voltage.Value)
	}
	if r.TemperatureC.Valid {
		appendFloatField(&b, names.Temperature, r.TemperatureC.Value)
	}

	// A zero timestamp means nothing recorded when this reading was taken.
	// Omitting the timestamp lets the server stamp it on arrival, which is
	// approximately right, whereas the zero time is year 1 and would be silently
	// dropped by any retention policy.
	if !r.Timestamp.IsZero() {
		b.WriteByte(' ')
		b.WriteString(strconv.FormatInt(r.Timestamp.UnixNano(), 10))
	}

	return b.String()
}

// appendTag writes a comma-separated tag, skipping an empty value.
//
// An empty tag value is not the same as an absent tag in line protocol: it is
// a syntax error in some server versions and a distinct series in others.
func appendTag(b *strings.Builder, key, value string) {
	if value == "" {
		return
	}
	b.WriteByte(',')
	b.WriteString(escapeTag(key))
	b.WriteByte('=')
	b.WriteString(escapeTag(value))
}

// appendFloatField writes a comma-separated float field.
//
// NaN and infinity have no line protocol representation, so such a value is
// dropped instead of being written as a number that means something else.
func appendFloatField(b *strings.Builder, key string, v float64) {
	if math.IsNaN(v) || math.IsInf(v, 0) {
		return
	}
	b.WriteByte(',')
	b.WriteString(escapeTag(key))
	b.WriteByte('=')
	b.WriteString(strconv.FormatFloat(v, 'f', -1, 64))
}

// Line protocol delimits with unescaped commas, spaces and equals signs, so an
// unescaped one in a name silently reshapes the point: a comma in a measurement
// turns the remainder into a tag, and a space ends the field set early. These
// replacers are single-pass, so an already-escaped backslash is not doubled a
// second time.
//
// A raw newline terminates a point outright, which would split one write into
// two malformed ones, so it is escaped everywhere too.
var (
	// measurementEscaper covers a measurement name, which is ended by a comma or
	// a space but may contain an equals sign.
	measurementEscaper = strings.NewReplacer(
		`\`, `\\`,
		`,`, `\,`,
		` `, `\ `,
		"\n", `\n`,
	)

	// tagEscaper covers tag keys, tag values and field keys, which share one
	// escape set.
	tagEscaper = strings.NewReplacer(
		`\`, `\\`,
		`,`, `\,`,
		`=`, `\=`,
		` `, `\ `,
		"\n", `\n`,
	)

	// stringFieldEscaper covers the inside of a quoted string field value, where
	// only the closing quote and the escape character itself are special.
	stringFieldEscaper = strings.NewReplacer(
		`\`, `\\`,
		`"`, `\"`,
		"\n", `\n`,
	)
)

// escapeMeasurement escapes a measurement name.
func escapeMeasurement(s string) string { return measurementEscaper.Replace(s) }

// escapeTag escapes a tag key, tag value or field key.
func escapeTag(s string) string { return tagEscaper.Replace(s) }

// escapeStringField escapes the contents of a string field value, without the
// surrounding quotes.
func escapeStringField(s string) string { return stringFieldEscaper.Replace(s) }

// quoteStringField renders a string field value ready to place after an equals
// sign.
func quoteStringField(s string) string { return `"` + escapeStringField(s) + `"` }
