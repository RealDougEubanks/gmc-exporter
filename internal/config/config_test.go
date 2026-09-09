package config

import (
	"bytes"
	"errors"
	"log/slog"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/RealDougEubanks/gmc-exporter/internal/redact"
	"github.com/RealDougEubanks/gmc-exporter/internal/serialport"
)

// loadWithEnv runs Load against exactly the supplied settings.
//
// Load reads the real process environment, so the surrounding environment is
// cleared of every GOGMC_ variable first. Otherwise a developer with the
// exporter configured in their shell would see different test results from CI.
func loadWithEnv(t *testing.T, env map[string]string) (*Config, error) {
	t.Helper()

	for _, entry := range os.Environ() {
		key, value, _ := strings.Cut(entry, "=")
		if !strings.HasPrefix(key, EnvPrefix) {
			continue
		}
		// t.Setenv registers the restore; Unsetenv then clears it for the
		// duration of this test.
		t.Setenv(key, value)
		if err := os.Unsetenv(key); err != nil {
			t.Fatalf("clearing %s: %v", key, err)
		}
	}

	for key, value := range env {
		t.Setenv(EnvPrefix+key, value)
	}
	return Load("")
}

// minimalEnv is the smallest configuration that validates: one sink, nothing
// else.
func minimalEnv(extra map[string]string) map[string]string {
	env := map[string]string{"PROMETHEUS_ENABLED": "true"}
	for k, v := range extra {
		env[k] = v
	}
	return env
}

// problems splits an aggregated Load error back into its individual complaints.
func problems(t *testing.T, err error) []string {
	t.Helper()
	if err == nil {
		t.Fatal("expected an error, got nil")
	}
	text := err.Error()
	if !strings.HasPrefix(text, "invalid configuration:\n  - ") {
		t.Fatalf("error is not an aggregated report: %q", text)
	}
	return strings.Split(strings.TrimPrefix(text, "invalid configuration:\n  - "), "\n  - ")
}

// assertProblem finds the single complaint containing every fragment.
func assertProblem(t *testing.T, err error, fragments ...string) {
	t.Helper()
	found := 0
	for _, p := range problems(t, err) {
		matches := true
		for _, f := range fragments {
			if !strings.Contains(p, f) {
				matches = false
				break
			}
		}
		if matches {
			found++
		}
	}
	if found != 1 {
		t.Fatalf("found %d problems matching %v, want 1; got:\n%s", found, fragments, err)
	}
}

func TestLoadMinimalConfiguration(t *testing.T) {
	cfg, err := loadWithEnv(t, minimalEnv(nil))
	if err != nil {
		t.Fatalf("Load = %v, want a valid configuration", err)
	}

	if cfg.Serial.Port != "/dev/ttyUSB0" || cfg.Serial.Baud != serialport.DefaultBaud {
		t.Fatalf("serial defaults = %+v", cfg.Serial)
	}
	if cfg.Poll.Interval != time.Minute || cfg.Poll.AverageWindow != 60 {
		t.Fatalf("poll defaults = %+v", cfg.Poll)
	}
	if cfg.Log.Level != slog.LevelInfo || cfg.Log.Format != "json" {
		t.Fatalf("log defaults = %+v", cfg.Log)
	}
	if cfg.HTTP.Addr != ":9101" {
		t.Fatalf("HTTP_ADDR default = %q, want :9101", cfg.HTTP.Addr)
	}
	if cfg.Location.Valid {
		t.Fatal("location is valid despite no coordinates being set")
	}
	if !cfg.Prometheus.Enabled || cfg.Prometheus.Path != "/metrics" {
		t.Fatalf("prometheus = %+v", cfg.Prometheus)
	}
	// Every sink is off unless explicitly enabled.
	if cfg.Radmon.Enabled || cfg.InfluxV1.Enabled || cfg.InfluxV2.Enabled ||
		cfg.OTLP.Enabled || cfg.MQTT.Enabled || cfg.GMCMap.Enabled || cfg.Safecast.Enabled {
		t.Fatal("a sink other than Prometheus defaulted to enabled")
	}
	if len(cfg.resolved) == 0 {
		t.Fatal("no settings were recorded for the startup log")
	}
}

func TestLoadPopulatesEverySink(t *testing.T) {
	cfg, err := loadWithEnv(t, map[string]string{
		"LATITUDE":  "35.7796",
		"LONGITUDE": "-78.6382",

		"RADMON_ENABLED":     "true",
		"RADMON_USER":        "doug",
		"RADMON_PASSWORD":    "radmon-pw",
		"RADMON_USE_LATLNG":  "true",
		"INFLUX1_ENABLED":    "true",
		"INFLUX1_URL":        "http://influx:8086",
		"INFLUX1_DATABASE":   "radiation",
		"INFLUX2_ENABLED":    "true",
		"INFLUX2_URL":        "http://influx:8086",
		"INFLUX2_ORG":        "home",
		"INFLUX2_BUCKET":     "rad",
		"INFLUX2_TOKEN":      "influx-token",
		"OTLP_ENABLED":       "true",
		"OTLP_ENDPOINT":      "otel:4317",
		"OTLP_HEADERS":       "authorization=Bearer abc",
		"MQTT_ENABLED":       "true",
		"MQTT_BROKER":        "broker.local",
		"MQTT_PASSWORD":      "mqtt-pw",
		"GMCMAP_ENABLED":     "true",
		"GMCMAP_ACCOUNT_ID":  "12345",
		"GMCMAP_COUNTER_ID":  "67890",
		"SAFECAST_ENABLED":   "true",
		"SAFECAST_API_KEY":   "safecast-key",
		"SAFECAST_DEVICE_ID": "42",
		"PROMETHEUS_ENABLED": "true",
	})
	if err != nil {
		t.Fatalf("Load = %v, want a valid configuration", err)
	}

	if !cfg.Location.Valid || cfg.Location.Latitude != 35.7796 || cfg.Location.Longitude != -78.6382 {
		t.Fatalf("location = %+v", cfg.Location)
	}
	if cfg.Radmon.Password.Reveal() != "radmon-pw" {
		t.Fatal("radmon password did not survive loading")
	}
	if cfg.InfluxV2.Token.Reveal() != "influx-token" {
		t.Fatal("influx token did not survive loading")
	}
	if cfg.GMCMap.CounterID.Reveal() != "67890" {
		t.Fatal("gmcmap counter id did not survive loading")
	}
	if cfg.Safecast.APIKey.Reveal() != "safecast-key" || cfg.Safecast.DeviceID != 42 {
		t.Fatalf("safecast = %+v", cfg.Safecast)
	}
	if cfg.OTLP.Headers["authorization"] != "Bearer abc" {
		t.Fatalf("otlp headers = %v", cfg.OTLP.Headers)
	}
	if !AnySinkEnabled(cfg) {
		t.Fatal("AnySinkEnabled = false with every sink on")
	}
}

func TestValidateRequiresLocationForPublicMaps(t *testing.T) {
	cases := []struct {
		name string
		env  map[string]string
		want string
	}{
		{
			name: "GMCMAP",
			env:  map[string]string{"GMCMAP_ENABLED": "true", "GMCMAP_ACCOUNT_ID": "1", "GMCMAP_COUNTER_ID": "2"},
			want: "GMCMAP requires a location",
		},
		{
			name: "Safecast",
			env:  map[string]string{"SAFECAST_ENABLED": "true", "SAFECAST_API_KEY": "k"},
			want: "Safecast requires a location",
		},
		{
			name: "radmon with latlng",
			env: map[string]string{
				"RADMON_ENABLED": "true", "RADMON_USER": "doug",
				"RADMON_PASSWORD": "pw", "RADMON_USE_LATLNG": "true",
			},
			want: "radmon.org submitwithlatlng requires a location",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := loadWithEnv(t, minimalEnv(tc.env))
			assertProblem(t, err, tc.want, EnvPrefix+"LATITUDE", EnvPrefix+"LONGITUDE")

			// With coordinates supplied the same configuration is valid.
			withLocation := minimalEnv(tc.env)
			withLocation["LATITUDE"] = "35.7796"
			withLocation["LONGITUDE"] = "-78.6382"
			if _, err := loadWithEnv(t, withLocation); err != nil {
				t.Fatalf("Load with coordinates = %v, want nil", err)
			}
		})
	}
}

// TestRadmonWithoutLatLngNeedsNoLocation checks the rule is scoped to the
// function that actually publishes coordinates.
func TestRadmonWithoutLatLngNeedsNoLocation(t *testing.T) {
	_, err := loadWithEnv(t, minimalEnv(map[string]string{
		"RADMON_ENABLED": "true", "RADMON_USER": "doug", "RADMON_PASSWORD": "pw",
	}))
	if err != nil {
		t.Fatalf("Load = %v, want nil", err)
	}
}

// TestLatitudeWithoutLongitudeIsRejected covers the half-configured coordinate,
// which would otherwise publish a reading attributed to the wrong place.
func TestLatitudeWithoutLongitudeIsRejected(t *testing.T) {
	for _, only := range []string{"LATITUDE", "LONGITUDE"} {
		t.Run(only, func(t *testing.T) {
			value := "35.7796"
			if only == "LONGITUDE" {
				value = "-78.6382"
			}
			_, err := loadWithEnv(t, minimalEnv(map[string]string{only: value}))
			assertProblem(t, err, EnvPrefix+"LATITUDE", EnvPrefix+"LONGITUDE", "must be set together")
		})
	}
}

func TestValidateRejectsNoSinks(t *testing.T) {
	cfg, err := loadWithEnv(t, nil)
	if cfg != nil {
		t.Fatal("Load returned a config alongside an error")
	}
	assertProblem(t, err, "no sinks are enabled", EnvPrefix+"PROMETHEUS_ENABLED")
}

// TestValidateResponseTimeoutShorterThanPollInterval prevents a configuration
// where one slow read overruns the next poll.
func TestValidateResponseTimeoutShorterThanPollInterval(t *testing.T) {
	_, err := loadWithEnv(t, minimalEnv(map[string]string{
		"POLL_INTERVAL":           "10s",
		"SERIAL_RESPONSE_TIMEOUT": "10s",
		"SERIAL_READ_TIMEOUT":     "50ms",
	}))
	assertProblem(t, err, EnvPrefix+"SERIAL_RESPONSE_TIMEOUT", EnvPrefix+"POLL_INTERVAL", "must be shorter than")

	if _, err := loadWithEnv(t, minimalEnv(map[string]string{
		"POLL_INTERVAL":           "10s",
		"SERIAL_RESPONSE_TIMEOUT": "9s",
	})); err != nil {
		t.Fatalf("Load with a response timeout under the interval = %v, want nil", err)
	}
}

// TestValidateReadTimeoutWithinResponseTimeout keeps the per-read granularity
// from exceeding the whole response budget, which would make the budget
// unenforceable.
func TestValidateReadTimeoutWithinResponseTimeout(t *testing.T) {
	_, err := loadWithEnv(t, minimalEnv(map[string]string{
		"SERIAL_RESPONSE_TIMEOUT": "1s",
		"SERIAL_READ_TIMEOUT":     "2s",
	}))
	assertProblem(t, err, EnvPrefix+"SERIAL_READ_TIMEOUT", EnvPrefix+"SERIAL_RESPONSE_TIMEOUT", "must not exceed")

	// Equal values are accepted; the rule is "must not exceed".
	if _, err := loadWithEnv(t, minimalEnv(map[string]string{
		"SERIAL_RESPONSE_TIMEOUT": "2s",
		"SERIAL_READ_TIMEOUT":     "2s",
	})); err != nil {
		t.Fatalf("Load with equal timeouts = %v, want nil", err)
	}
}

func TestValidateSinkSpecificRequirements(t *testing.T) {
	cases := []struct {
		name      string
		env       map[string]string
		fragments []string
	}{
		{
			name:      "radmon without user",
			env:       map[string]string{"RADMON_ENABLED": "true", "RADMON_PASSWORD": "pw"},
			fragments: []string{"radmon.org is enabled", EnvPrefix + "RADMON_USER"},
		},
		{
			name:      "radmon without password",
			env:       map[string]string{"RADMON_ENABLED": "true", "RADMON_USER": "doug"},
			fragments: []string{"radmon.org is enabled", EnvPrefix + "RADMON_PASSWORD" + FileSuffix},
		},
		{
			name:      "influx1 without url",
			env:       map[string]string{"INFLUX1_ENABLED": "true", "INFLUX1_DATABASE": "rad"},
			fragments: []string{"InfluxDB 1.x is enabled", EnvPrefix + "INFLUX1_URL"},
		},
		{
			name: "influx2 without bucket",
			env: map[string]string{
				"INFLUX2_ENABLED": "true", "INFLUX2_URL": "http://influx:8086",
				"INFLUX2_ORG": "home", "INFLUX2_TOKEN": "t",
			},
			fragments: []string{"InfluxDB 2.x is enabled", EnvPrefix + "INFLUX2_BUCKET"},
		},
		{
			name:      "otlp without endpoint",
			env:       map[string]string{"OTLP_ENABLED": "true"},
			fragments: []string{"OTLP is enabled", EnvPrefix + "OTLP_ENDPOINT"},
		},
		{
			name:      "prometheus path without a leading slash",
			env:       map[string]string{"PROMETHEUS_PATH": "metrics"},
			fragments: []string{EnvPrefix + "PROMETHEUS_PATH", "must start with a slash"},
		},
		{
			name:      "prometheus with an empty listen address",
			env:       map[string]string{"HTTP_ADDR": ""},
			fragments: []string{"Prometheus is enabled", EnvPrefix + "HTTP_ADDR"},
		},
		{
			name:      "mqtt without a broker",
			env:       map[string]string{"MQTT_ENABLED": "true"},
			fragments: []string{"MQTT is enabled", EnvPrefix + "MQTT_BROKER"},
		},
		{
			name: "mqtt client certificate without its key",
			env: map[string]string{
				"MQTT_ENABLED": "true", "MQTT_BROKER": "broker.local",
				"MQTT_TLS": "true", "MQTT_CLIENT_CERT": "/certs/client.pem",
			},
			fragments: []string{EnvPrefix + "MQTT_CLIENT_CERT", EnvPrefix + "MQTT_CLIENT_KEY", "must be set together"},
		},
		{
			name: "mqtt certificates supplied with TLS off",
			env: map[string]string{
				"MQTT_ENABLED": "true", "MQTT_BROKER": "broker.local",
				"MQTT_CA_CERT": "/certs/ca.pem",
			},
			fragments: []string{EnvPrefix + "MQTT_TLS", "is false but TLS certificates were supplied"},
		},
		{
			name:      "gmcmap without an account id",
			env:       map[string]string{"GMCMAP_ENABLED": "true", "GMCMAP_COUNTER_ID": "2", "LATITUDE": "1", "LONGITUDE": "2"},
			fragments: []string{"GMCMAP is enabled", EnvPrefix + "GMCMAP_ACCOUNT_ID"},
		},
		{
			name:      "safecast without an api key",
			env:       map[string]string{"SAFECAST_ENABLED": "true", "LATITUDE": "1", "LONGITUDE": "2"},
			fragments: []string{"Safecast is enabled", EnvPrefix + "SAFECAST_API_KEY" + FileSuffix},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := loadWithEnv(t, minimalEnv(tc.env))
			assertProblem(t, err, tc.fragments...)
		})
	}
}

// TestErrorsAccumulate is the property that stops an operator restarting a
// container once per mistake: every problem is reported in one pass.
func TestErrorsAccumulate(t *testing.T) {
	_, err := loadWithEnv(t, map[string]string{
		"SERIAL_BAUD":    "99",       // out of range
		"LOG_LEVEL":      "verbose",  // not one of the allowed values
		"POLL_INTERVAL":  "nonsense", // not a duration
		"RADMON_ENABLED": "maybe",    // not a boolean
		// and no sink is enabled at all
	})

	got := problems(t, err)
	if len(got) != 5 {
		t.Fatalf("problem count = %d, want 5:\n%s", len(got), strings.Join(got, "\n"))
	}

	for _, want := range []string{
		EnvPrefix + "SERIAL_BAUD",
		EnvPrefix + "LOG_LEVEL",
		EnvPrefix + "POLL_INTERVAL",
		EnvPrefix + "RADMON_ENABLED",
		"no sinks are enabled",
	} {
		assertProblem(t, err, want)
	}
}

// TestEveryProblemNamesItsSetting keeps error messages actionable: a report
// that does not name a GOGMC_ variable leaves the operator guessing.
func TestEveryProblemNamesItsSetting(t *testing.T) {
	_, err := loadWithEnv(t, map[string]string{
		"SERIAL_BAUD":             "99",
		"SERIAL_RESPONSE_TIMEOUT": "1s",
		"SERIAL_READ_TIMEOUT":     "2s",
		"LATITUDE":                "35.7796",
		"MQTT_ENABLED":            "true",
		"OTLP_ENABLED":            "true",
		"PROMETHEUS_PATH":         "metrics",
		"HTTP_STALE_AFTER":        "48h",
		"OTLP_HEADERS":            "not-a-pair",
	})

	got := problems(t, err)
	if len(got) < 6 {
		t.Fatalf("expected several problems, got %d:\n%s", len(got), strings.Join(got, "\n"))
	}
	for _, p := range got {
		if !strings.Contains(p, EnvPrefix) {
			t.Fatalf("problem %q does not name a %s setting", p, EnvPrefix)
		}
	}
}

// TestBothVariantsSetIsReportedByLoad wires the _FILE conflict through the real
// entry point, since that is where an operator meets it.
func TestBothVariantsSetIsReportedByLoad(t *testing.T) {
	path := t.TempDir() + "/password"
	if err := os.WriteFile(path, []byte("from-file\n"), 0o600); err != nil {
		t.Fatalf("writing the fixture: %v", err)
	}

	_, err := loadWithEnv(t, minimalEnv(map[string]string{
		"RADMON_PASSWORD":              "from-env",
		"RADMON_PASSWORD" + FileSuffix: path,
	}))
	assertProblem(t, err, EnvPrefix+"RADMON_PASSWORD", FileSuffix, "supply exactly one")
	if strings.Contains(err.Error(), "from-env") || strings.Contains(err.Error(), "from-file") {
		t.Fatalf("the conflict report leaked a credential: %v", err)
	}
}

// TestSecretFromFileThroughLoad proves the container path works end to end.
func TestSecretFromFileThroughLoad(t *testing.T) {
	path := t.TempDir() + "/password"
	if err := os.WriteFile(path, []byte("hunter2\n"), 0o600); err != nil {
		t.Fatalf("writing the fixture: %v", err)
	}

	cfg, err := loadWithEnv(t, minimalEnv(map[string]string{
		"RADMON_ENABLED":               "true",
		"RADMON_USER":                  "doug",
		"RADMON_PASSWORD" + FileSuffix: path,
	}))
	if err != nil {
		t.Fatalf("Load = %v, want nil", err)
	}
	if got := cfg.Radmon.Password.Reveal(); got != "hunter2" {
		t.Fatalf("password = %q, want hunter2 with the trailing newline stripped", got)
	}
}

// TestMissingSecretFileNamesPathNotContents keeps a misconfigured mount
// diagnosable without turning the log into a credential dump.
func TestMissingSecretFileNamesPathNotContents(t *testing.T) {
	path := t.TempDir() + "/absent"
	_, err := loadWithEnv(t, minimalEnv(map[string]string{
		"RADMON_PASSWORD" + FileSuffix: path,
	}))
	assertProblem(t, err, EnvPrefix+"RADMON_PASSWORD"+FileSuffix, path)
}

func TestStaleThreshold(t *testing.T) {
	cfg := &Config{Poll: Poll{Interval: 30 * time.Second}}
	if got := cfg.StaleThreshold(); got != 90*time.Second {
		t.Fatalf("StaleThreshold = %s, want three poll intervals", got)
	}

	cfg.HTTP.StaleAfter = 5 * time.Minute
	if got := cfg.StaleThreshold(); got != 5*time.Minute {
		t.Fatalf("StaleThreshold = %s, want the explicit override", got)
	}
}

func TestAnySinkEnabled(t *testing.T) {
	if AnySinkEnabled(&Config{}) {
		t.Fatal("AnySinkEnabled = true for an empty config")
	}

	// Each sink must be enough on its own; a missing case here would let a
	// working configuration be rejected as having no sinks.
	enablers := []func(*Config){
		func(c *Config) { c.Radmon.Enabled = true },
		func(c *Config) { c.InfluxV1.Enabled = true },
		func(c *Config) { c.InfluxV2.Enabled = true },
		func(c *Config) { c.OTLP.Enabled = true },
		func(c *Config) { c.Prometheus.Enabled = true },
		func(c *Config) { c.MQTT.Enabled = true },
		func(c *Config) { c.GMCMap.Enabled = true },
		func(c *Config) { c.Safecast.Enabled = true },
	}
	for i, enable := range enablers {
		cfg := &Config{}
		enable(cfg)
		if !AnySinkEnabled(cfg) {
			t.Fatalf("AnySinkEnabled = false with sink %d enabled", i)
		}
	}
}

func TestParseLevel(t *testing.T) {
	want := map[string]slog.Level{
		"debug": slog.LevelDebug,
		"info":  slog.LevelInfo,
		"warn":  slog.LevelWarn,
		"error": slog.LevelError,
	}
	for name, level := range want {
		l := newTestLoader(nil, nil)
		if got := parseLevel(l, name); got != level {
			t.Fatalf("parseLevel(%q) = %v, want %v", name, got, level)
		}
		requireNoErrors(t, l)
	}

	// Enum should make this unreachable, but the fallback must still be safe
	// rather than silently logging at debug.
	l := newTestLoader(nil, nil)
	if got := parseLevel(l, "trace"); got != slog.LevelInfo {
		t.Fatalf("parseLevel of an unknown name = %v, want info", got)
	}
	if len(l.errs) != 1 {
		t.Fatalf("error count = %d, want 1", len(l.errs))
	}
}

// TestLogEffectiveNeverLogsASecret is the assertion that matters most in this
// file. The startup log is written on every boot and is routinely pasted into
// tickets and shipped to aggregators, so a credential appearing here is a
// credential disclosed.
func TestLogEffectiveNeverLogsASecret(t *testing.T) {
	const (
		radmonCanary   = "canary-radmon-pw-9f3a"
		influxCanary   = "canary-influx-token-4b21"
		mqttCanary     = "canary-mqtt-pw-77de"
		gmcmapCanary   = "canary-counter-id-1b9c"
		safecastCanary = "canary-safecast-key-c0de"
		headerCanary   = "canary-bearer-token-e5f7"
	)

	cfg, err := loadWithEnv(t, map[string]string{
		"LATITUDE":  "35.7796",
		"LONGITUDE": "-78.6382",

		"RADMON_ENABLED":     "true",
		"RADMON_USER":        "doug",
		"RADMON_PASSWORD":    radmonCanary,
		"INFLUX2_ENABLED":    "true",
		"INFLUX2_URL":        "http://influx:8086",
		"INFLUX2_ORG":        "home",
		"INFLUX2_BUCKET":     "rad",
		"INFLUX2_TOKEN":      influxCanary,
		"MQTT_ENABLED":       "true",
		"MQTT_BROKER":        "broker.local",
		"MQTT_PASSWORD":      mqttCanary,
		"GMCMAP_ENABLED":     "true",
		"GMCMAP_ACCOUNT_ID":  "12345",
		"GMCMAP_COUNTER_ID":  gmcmapCanary,
		"SAFECAST_ENABLED":   "true",
		"SAFECAST_API_KEY":   safecastCanary,
		"OTLP_ENABLED":       "true",
		"OTLP_ENDPOINT":      "otel:4317",
		"OTLP_HEADERS":       "authorization=Bearer " + headerCanary,
		"PROMETHEUS_ENABLED": "true",
	})
	if err != nil {
		t.Fatalf("Load = %v, want a valid configuration", err)
	}

	// Debug is the noisiest level the exporter supports, so it is the level
	// at which a leak would actually happen.
	var buf bytes.Buffer
	log := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	cfg.LogEffective(log)

	out := buf.String()
	for _, canary := range []string{
		radmonCanary, influxCanary, mqttCanary, gmcmapCanary, safecastCanary, headerCanary,
	} {
		if strings.Contains(out, canary) {
			t.Fatalf("the startup log leaked %q:\n%s", canary, out)
		}
	}

	// The settings must still appear, with the placeholder standing in for the
	// value, or the log would be useless for confirming what was loaded.
	if !strings.Contains(out, redact.Placeholder) {
		t.Fatalf("no %s placeholder in the startup log:\n%s", redact.Placeholder, out)
	}
	for _, key := range []string{
		EnvPrefix + "RADMON_PASSWORD",
		EnvPrefix + "INFLUX2_TOKEN",
		EnvPrefix + "MQTT_PASSWORD",
		EnvPrefix + "GMCMAP_COUNTER_ID",
		EnvPrefix + "SAFECAST_API_KEY",
	} {
		if !strings.Contains(out, key) {
			t.Fatalf("the startup log omits %s entirely:\n%s", key, out)
		}
	}

	// Non-secret settings are logged in the clear; that is the point of the
	// startup log.
	if !strings.Contains(out, "doug") {
		t.Fatalf("the startup log omits the radmon user:\n%s", out)
	}
}

// TestLogEffectiveSummaryAtInfo checks the one line an operator sees without
// turning debug logging on.
func TestLogEffectiveSummaryAtInfo(t *testing.T) {
	cfg, err := loadWithEnv(t, minimalEnv(nil))
	if err != nil {
		t.Fatalf("Load = %v, want nil", err)
	}

	var buf bytes.Buffer
	log := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo}))
	cfg.LogEffective(log)

	out := buf.String()
	if !strings.Contains(out, "effective configuration") {
		t.Fatalf("no summary line in the log:\n%s", out)
	}
	if strings.Contains(out, EnvPrefix+"SERIAL_PORT") {
		t.Fatalf("per-setting detail was emitted at info level:\n%s", out)
	}
}

func TestJoin(t *testing.T) {
	if err := Join(); err != nil {
		t.Fatalf("Join() = %v, want nil", err)
	}
	a := errors.New("a")
	b := errors.New("b")
	err := Join(a, nil, b)
	if !errors.Is(err, a) || !errors.Is(err, b) {
		t.Fatalf("Join = %v, want it to carry both errors", err)
	}
}
