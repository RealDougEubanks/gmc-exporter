// Package config loads and validates the exporter's settings from the
// environment.
//
// Every setting is namespaced with GOGMC_, every secret additionally supports a
// _FILE variant so it can be delivered as a Docker or Kubernetes secret, and
// every sink is disabled by default. Validation happens once at startup and
// reports all problems together, naming the offending setting.
package config

import (
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/RealDougEubanks/gmc-exporter/internal/redact"
	"github.com/RealDougEubanks/gmc-exporter/internal/serialport"
)

// Config is the fully validated configuration.
type Config struct {
	Serial     Serial
	Poll       Poll
	Log        Log
	HTTP       HTTP
	Location   Location
	Radmon     Radmon
	InfluxV1   InfluxV1
	InfluxV2   InfluxV2
	OTLP       OTLP
	Prometheus Prometheus
	MQTT       MQTT
	GMCMap     GMCMap
	Safecast   Safecast

	// resolved holds every setting's provenance for the startup log.
	resolved []resolved
}

// Serial describes the connection to the Geiger counter.
type Serial struct {
	Port string
	Baud int
	// ResponseTimeout bounds a complete response. Measured worst-case
	// first-byte latency on a GMC-320 was 171ms.
	ResponseTimeout time.Duration
	// ReadTimeout is the per-read poll granularity, not a response budget.
	ReadTimeout time.Duration
	// ReconnectBackoff is the initial delay before reopening a port that
	// disappeared, doubling up to ReconnectBackoffMax.
	ReconnectBackoff    time.Duration
	ReconnectBackoffMax time.Duration
}

// Poll describes the sampling cadence.
type Poll struct {
	Interval time.Duration
	// AverageWindow is how many samples contribute to the running mean.
	AverageWindow int
	// CalibrationOverride replaces the table read from the device, for a unit
	// whose configuration block does not match the measured layout.
	CalibrationOverride string
}

// Log describes log output.
type Log struct {
	Level  slog.Level
	Format string
}

// HTTP describes the built-in server carrying /metrics and the health
// endpoints.
type HTTP struct {
	Addr            string
	ReadTimeout     time.Duration
	ShutdownTimeout time.Duration
	// StaleAfter is how long without a successful read before /healthz
	// reports unhealthy.
	StaleAfter time.Duration
}

// Location is the shared coordinate pair.
//
// These are published to public radiation maps. They are optional, and a sink
// that needs them refuses to start without them rather than substituting a
// default, because a default would silently publish readings attributed to the
// wrong place.
type Location struct {
	Latitude  float64
	Longitude float64
	Valid     bool
}

// Radmon publishes to radmon.org.
type Radmon struct {
	Enabled bool
	User    string
	// Password is radmon's separate data-sending password, which is not the
	// account login password.
	Password redact.Secret
	// UseLatLng selects the submitwithlatlng function, which additionally
	// publishes the configured coordinates.
	UseLatLng bool
	Timeout   time.Duration
	Retries   int
}

// InfluxV1 publishes to InfluxDB 1.x.
type InfluxV1 struct {
	Enabled     bool
	URL         string
	Database    string
	User        string
	Password    redact.Secret
	Measurement string
	Timeout     time.Duration
	Retries     int
}

// InfluxV2 publishes to InfluxDB 2.x using the token/org/bucket model.
type InfluxV2 struct {
	Enabled     bool
	URL         string
	Token       redact.Secret
	Org         string
	Bucket      string
	Measurement string
	Timeout     time.Duration
	Retries     int
}

// OTLP exports metrics over OpenTelemetry.
type OTLP struct {
	Enabled  bool
	Protocol string
	Endpoint string
	Headers  map[string]string
	Insecure bool
	Timeout  time.Duration
}

// Prometheus exposes a scrape endpoint.
//
// It needs no credentials, cannot stall the poll loop, and keeps working if
// every push-based backend changes, which makes it the most robust of the
// sinks.
type Prometheus struct {
	Enabled bool
	Path    string
}

// MQTT publishes to a broker, optionally advertising the sensors to Home
// Assistant.
type MQTT struct {
	Enabled bool
	Broker  string
	Port    int
	TLS     bool
	CACert  string
	ClientCert
	Username  string
	Password  redact.Secret
	ClientID  string
	QoS       int
	BaseTopic string
	Retain    bool

	// HADiscovery publishes Home Assistant discovery messages so the sensors
	// appear without manual configuration.
	HADiscovery bool
	HAPrefix    string
	DeviceName  string

	Timeout time.Duration
}

// ClientCert is an optional mutual-TLS client certificate pair.
type ClientCert struct {
	CertFile string
	KeyFile  string
}

// GMCMap publishes to GQ Electronics' own radiation map.
type GMCMap struct {
	Enabled   bool
	AccountID string
	CounterID redact.Secret
	Timeout   time.Duration
	Retries   int
}

// Safecast publishes to the Safecast open radiation dataset.
type Safecast struct {
	Enabled  bool
	APIKey   redact.Secret
	DeviceID int
	Timeout  time.Duration
	Retries  int
}

// Load reads and validates configuration from the environment.
//
// It returns every problem it found, not just the first, so a misconfigured
// deployment can be fixed in one pass.
func Load() (*Config, error) {
	l := newLoader()
	cfg := &Config{}

	cfg.Serial = Serial{
		Port:                l.String("SERIAL_PORT", "/dev/ttyUSB0"),
		Baud:                l.Int("SERIAL_BAUD", serialport.DefaultBaud, 1200, 115200),
		ResponseTimeout:     l.Duration("SERIAL_RESPONSE_TIMEOUT", 2*time.Second, 100*time.Millisecond, time.Minute),
		ReadTimeout:         l.Duration("SERIAL_READ_TIMEOUT", serialport.DefaultReadTimeout, 10*time.Millisecond, 5*time.Second),
		ReconnectBackoff:    l.Duration("SERIAL_RECONNECT_BACKOFF", 2*time.Second, 100*time.Millisecond, time.Minute),
		ReconnectBackoffMax: l.Duration("SERIAL_RECONNECT_BACKOFF_MAX", 60*time.Second, time.Second, 15*time.Minute),
	}

	cfg.Poll = Poll{
		Interval:            l.Duration("POLL_INTERVAL", 60*time.Second, time.Second, time.Hour),
		AverageWindow:       l.Int("AVERAGE_WINDOW", 60, 1, 10000),
		CalibrationOverride: l.String("CALIBRATION", ""),
	}

	cfg.Log = Log{
		Level:  parseLevel(l, l.Enum("LOG_LEVEL", "info", "debug", "info", "warn", "error")),
		Format: l.Enum("LOG_FORMAT", "json", "json", "text"),
	}

	cfg.HTTP = HTTP{
		Addr:            l.String("HTTP_ADDR", ":9101"),
		ReadTimeout:     l.Duration("HTTP_READ_TIMEOUT", 10*time.Second, time.Second, time.Minute),
		ShutdownTimeout: l.Duration("HTTP_SHUTDOWN_TIMEOUT", 5*time.Second, time.Second, time.Minute),
		StaleAfter:      l.Duration("HTTP_STALE_AFTER", 0, 0, 24*time.Hour),
	}

	lat, hasLat := l.OptionalFloat("LATITUDE", -90, 90)
	lon, hasLon := l.OptionalFloat("LONGITUDE", -180, 180)
	switch {
	case hasLat && hasLon:
		cfg.Location = Location{Latitude: lat, Longitude: lon, Valid: true}
	case hasLat != hasLon:
		l.errf("%sLATITUDE and %sLONGITUDE must be set together", EnvPrefix, EnvPrefix)
	}

	loadRadmon(l, cfg)
	loadInflux(l, cfg)
	loadOTLP(l, cfg)
	loadPrometheus(l, cfg)
	loadMQTT(l, cfg)
	loadMaps(l, cfg)

	validate(l, cfg)

	cfg.resolved = l.resolved
	if len(l.errs) > 0 {
		return nil, fmt.Errorf("invalid configuration:\n  - %s",
			strings.Join(errorStrings(l.errs), "\n  - "))
	}
	return cfg, nil
}

func loadRadmon(l *loader, cfg *Config) {
	cfg.Radmon = Radmon{
		Enabled:   l.Bool("RADMON_ENABLED", false),
		User:      l.String("RADMON_USER", ""),
		Password:  l.Secret("RADMON_PASSWORD"),
		UseLatLng: l.Bool("RADMON_USE_LATLNG", false),
		Timeout:   l.Duration("RADMON_TIMEOUT", 15*time.Second, time.Second, 2*time.Minute),
		Retries:   l.Int("RADMON_RETRIES", 2, 0, 10),
	}
	if cfg.Radmon.Enabled {
		l.require("radmon.org", "RADMON_USER", cfg.Radmon.User)
		l.requireSecret("radmon.org", "RADMON_PASSWORD", cfg.Radmon.Password)
	}
}

func loadInflux(l *loader, cfg *Config) {
	cfg.InfluxV1 = InfluxV1{
		Enabled:     l.Bool("INFLUX1_ENABLED", false),
		URL:         l.String("INFLUX1_URL", ""),
		Database:    l.String("INFLUX1_DATABASE", ""),
		User:        l.String("INFLUX1_USER", ""),
		Password:    l.Secret("INFLUX1_PASSWORD"),
		Measurement: l.String("INFLUX1_MEASUREMENT", "radiation"),
		Timeout:     l.Duration("INFLUX1_TIMEOUT", 15*time.Second, time.Second, 2*time.Minute),
		Retries:     l.Int("INFLUX1_RETRIES", 2, 0, 10),
	}
	if cfg.InfluxV1.Enabled {
		l.require("InfluxDB 1.x", "INFLUX1_URL", cfg.InfluxV1.URL)
		l.require("InfluxDB 1.x", "INFLUX1_DATABASE", cfg.InfluxV1.Database)
	}

	cfg.InfluxV2 = InfluxV2{
		Enabled:     l.Bool("INFLUX2_ENABLED", false),
		URL:         l.String("INFLUX2_URL", ""),
		Token:       l.Secret("INFLUX2_TOKEN"),
		Org:         l.String("INFLUX2_ORG", ""),
		Bucket:      l.String("INFLUX2_BUCKET", ""),
		Measurement: l.String("INFLUX2_MEASUREMENT", "radiation"),
		Timeout:     l.Duration("INFLUX2_TIMEOUT", 15*time.Second, time.Second, 2*time.Minute),
		Retries:     l.Int("INFLUX2_RETRIES", 2, 0, 10),
	}
	if cfg.InfluxV2.Enabled {
		l.require("InfluxDB 2.x", "INFLUX2_URL", cfg.InfluxV2.URL)
		l.require("InfluxDB 2.x", "INFLUX2_ORG", cfg.InfluxV2.Org)
		l.require("InfluxDB 2.x", "INFLUX2_BUCKET", cfg.InfluxV2.Bucket)
		l.requireSecret("InfluxDB 2.x", "INFLUX2_TOKEN", cfg.InfluxV2.Token)
	}
}

func loadOTLP(l *loader, cfg *Config) {
	cfg.OTLP = OTLP{
		Enabled:  l.Bool("OTLP_ENABLED", false),
		Protocol: l.Enum("OTLP_PROTOCOL", "grpc", "grpc", "http"),
		Endpoint: l.String("OTLP_ENDPOINT", ""),
		Headers:  l.StringMap("OTLP_HEADERS"),
		Insecure: l.Bool("OTLP_INSECURE", false),
		Timeout:  l.Duration("OTLP_TIMEOUT", 15*time.Second, time.Second, 2*time.Minute),
	}
	if cfg.OTLP.Enabled {
		l.require("OTLP", "OTLP_ENDPOINT", cfg.OTLP.Endpoint)
	}
}

func loadPrometheus(l *loader, cfg *Config) {
	cfg.Prometheus = Prometheus{
		Enabled: l.Bool("PROMETHEUS_ENABLED", false),
		Path:    l.String("PROMETHEUS_PATH", "/metrics"),
	}
	if cfg.Prometheus.Enabled && !strings.HasPrefix(cfg.Prometheus.Path, "/") {
		l.errf("%sPROMETHEUS_PATH must start with a slash, got %q", EnvPrefix, cfg.Prometheus.Path)
	}
}

func loadMQTT(l *loader, cfg *Config) {
	cfg.MQTT = MQTT{
		Enabled: l.Bool("MQTT_ENABLED", false),
		Broker:  l.String("MQTT_BROKER", ""),
		Port:    l.Int("MQTT_PORT", 1883, 1, 65535),
		TLS:     l.Bool("MQTT_TLS", false),
		CACert:  l.String("MQTT_CA_CERT", ""),
		ClientCert: ClientCert{
			CertFile: l.String("MQTT_CLIENT_CERT", ""),
			KeyFile:  l.String("MQTT_CLIENT_KEY", ""),
		},
		Username:    l.String("MQTT_USERNAME", ""),
		Password:    l.Secret("MQTT_PASSWORD"),
		ClientID:    l.String("MQTT_CLIENT_ID", "gmc-exporter"),
		QoS:         l.Int("MQTT_QOS", 1, 0, 2),
		BaseTopic:   l.String("MQTT_BASE_TOPIC", "gmc-exporter"),
		Retain:      l.Bool("MQTT_RETAIN", true),
		HADiscovery: l.Bool("MQTT_HA_DISCOVERY", false),
		HAPrefix:    l.String("MQTT_HA_PREFIX", "homeassistant"),
		DeviceName:  l.String("MQTT_DEVICE_NAME", "Geiger Counter"),
		Timeout:     l.Duration("MQTT_TIMEOUT", 15*time.Second, time.Second, 2*time.Minute),
	}
	if !cfg.MQTT.Enabled {
		return
	}
	l.require("MQTT", "MQTT_BROKER", cfg.MQTT.Broker)
	if (cfg.MQTT.ClientCert.CertFile == "") != (cfg.MQTT.ClientCert.KeyFile == "") {
		l.errf("%sMQTT_CLIENT_CERT and %sMQTT_CLIENT_KEY must be set together", EnvPrefix, EnvPrefix)
	}
	if !cfg.MQTT.TLS && (cfg.MQTT.CACert != "" || cfg.MQTT.ClientCert.CertFile != "") {
		l.errf("%sMQTT_TLS is false but TLS certificates were supplied", EnvPrefix)
	}
}

func loadMaps(l *loader, cfg *Config) {
	cfg.GMCMap = GMCMap{
		Enabled:   l.Bool("GMCMAP_ENABLED", false),
		AccountID: l.String("GMCMAP_ACCOUNT_ID", ""),
		CounterID: l.Secret("GMCMAP_COUNTER_ID"),
		Timeout:   l.Duration("GMCMAP_TIMEOUT", 15*time.Second, time.Second, 2*time.Minute),
		Retries:   l.Int("GMCMAP_RETRIES", 2, 0, 10),
	}
	if cfg.GMCMap.Enabled {
		l.require("GMCMAP", "GMCMAP_ACCOUNT_ID", cfg.GMCMap.AccountID)
		l.requireSecret("GMCMAP", "GMCMAP_COUNTER_ID", cfg.GMCMap.CounterID)
	}

	cfg.Safecast = Safecast{
		Enabled:  l.Bool("SAFECAST_ENABLED", false),
		APIKey:   l.Secret("SAFECAST_API_KEY"),
		DeviceID: l.Int("SAFECAST_DEVICE_ID", 0, 0, 1<<31-1),
		Timeout:  l.Duration("SAFECAST_TIMEOUT", 15*time.Second, time.Second, 2*time.Minute),
		Retries:  l.Int("SAFECAST_RETRIES", 2, 0, 10),
	}
	if cfg.Safecast.Enabled {
		l.requireSecret("Safecast", "SAFECAST_API_KEY", cfg.Safecast.APIKey)
	}
}

// validate applies the cross-cutting rules that span more than one setting.
func validate(l *loader, cfg *Config) {
	// Sinks that publish to a public map must not invent a location.
	needsLocation := map[string]bool{
		"GMCMAP":                      cfg.GMCMap.Enabled,
		"Safecast":                    cfg.Safecast.Enabled,
		"radmon.org submitwithlatlng": cfg.Radmon.Enabled && cfg.Radmon.UseLatLng,
	}
	for name, needs := range needsLocation {
		if needs && !cfg.Location.Valid {
			l.errf("%s requires a location; set %sLATITUDE and %sLONGITUDE "+
				"(note these are published publicly)", name, EnvPrefix, EnvPrefix)
		}
	}

	if cfg.Prometheus.Enabled && cfg.HTTP.Addr == "" {
		l.errf("Prometheus is enabled but %sHTTP_ADDR is empty", EnvPrefix)
	}

	if cfg.Serial.ResponseTimeout >= cfg.Poll.Interval {
		l.errf("%sSERIAL_RESPONSE_TIMEOUT (%s) must be shorter than %sPOLL_INTERVAL (%s)",
			EnvPrefix, cfg.Serial.ResponseTimeout, EnvPrefix, cfg.Poll.Interval)
	}

	if cfg.Serial.ReadTimeout > cfg.Serial.ResponseTimeout {
		l.errf("%sSERIAL_READ_TIMEOUT (%s) must not exceed %sSERIAL_RESPONSE_TIMEOUT (%s)",
			EnvPrefix, cfg.Serial.ReadTimeout, EnvPrefix, cfg.Serial.ResponseTimeout)
	}

	if !AnySinkEnabled(cfg) {
		l.errf("no sinks are enabled; enable at least one, for example %sPROMETHEUS_ENABLED=true",
			EnvPrefix)
	}
}

// AnySinkEnabled reports whether at least one sink is turned on.
func AnySinkEnabled(cfg *Config) bool {
	return cfg.Radmon.Enabled ||
		cfg.InfluxV1.Enabled ||
		cfg.InfluxV2.Enabled ||
		cfg.OTLP.Enabled ||
		cfg.Prometheus.Enabled ||
		cfg.MQTT.Enabled ||
		cfg.GMCMap.Enabled ||
		cfg.Safecast.Enabled
}

// StaleThreshold is how long without a successful read before the service
// reports itself unhealthy. It defaults to three poll intervals, which
// tolerates a couple of transient failures without masking a real stall.
func (c *Config) StaleThreshold() time.Duration {
	if c.HTTP.StaleAfter > 0 {
		return c.HTTP.StaleAfter
	}
	return 3 * c.Poll.Interval
}

// LogEffective writes the resolved configuration, with every secret replaced by
// a placeholder, so an operator can see what the process actually loaded and
// where each value came from.
func (c *Config) LogEffective(log *slog.Logger) {
	log.Info("effective configuration", "settings", len(c.resolved))
	for _, r := range c.resolved {
		log.Debug("setting", "key", r.Key, "value", r.Display, "source", string(r.Source))
	}
}

// parseLevel converts a validated level name to a slog.Level.
func parseLevel(l *loader, name string) slog.Level {
	switch name {
	case "debug":
		return slog.LevelDebug
	case "info":
		return slog.LevelInfo
	case "warn":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		l.errf("unknown log level %q", name)
		return slog.LevelInfo
	}
}

// errorStrings renders errors for the aggregated message.
func errorStrings(errs []error) []string {
	out := make([]string, 0, len(errs))
	for _, err := range errs {
		out = append(out, err.Error())
	}
	return out
}

// Join is a small helper for callers that need to combine validation errors.
func Join(errs ...error) error { return errors.Join(errs...) }
