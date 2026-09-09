package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeTemp writes a config file and returns its path.
func writeTemp(t *testing.T, name, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("writing %s: %v", path, err)
	}
	return path
}

// TestLegacySchemaFromRealDeployment parses the exact config.ini schema found
// on the host this exporter replaces.
//
// Recognising it is what makes an upgrade a configuration change rather than a
// rewrite, and the InfluxDB defaults matter most: without them the exporter
// would start writing a new measurement with new field names, and every
// dashboard built on the existing data would go blank with nothing reporting
// an error.
func TestLegacySchemaFromRealDeployment(t *testing.T) {
	t.Parallel()

	// Structure copied from the deployed file; values are placeholders.
	path := writeTemp(t, "config.ini", `[radmon.org]
  user = someuser
  password = somepassword

[influxdb]
  url = http://influx.example.lan:8086
  user = influxuser
  password = influxpassword
  database = radiation

[main]
  poll = 60

[watchdog]
  interval = 300
`)

	settings, err := LoadConfigFile(path)
	if err != nil {
		t.Fatalf("LoadConfigFile: %v", err)
	}
	if !settings.legacy {
		t.Fatal("the legacy schema was not recognised")
	}

	want := map[string]string{
		"RADMON_ENABLED":      "true",
		"RADMON_USER":         "someuser",
		"RADMON_PASSWORD":     "somepassword",
		"INFLUX1_ENABLED":     "true",
		"INFLUX1_URL":         "http://influx.example.lan:8086",
		"INFLUX1_USER":        "influxuser",
		"INFLUX1_PASSWORD":    "influxpassword",
		"INFLUX1_DATABASE":    "radiation",
		"POLL_INTERVAL":       "60s",
		"INFLUX1_MEASUREMENT": "data",
		"INFLUX1_FIELD_STYLE": "legacy",
		"INFLUX1_TAG_DEVICE":  "true",
	}
	for key, expected := range want {
		if got := settings.values[key]; got != expected {
			t.Errorf("%s = %q, want %q", key, got, expected)
		}
	}

	// The bare seconds in the legacy file must become a duration, or parsing
	// rejects it later with a confusing message about the wrong setting.
	if settings.values["POLL_INTERVAL"] != "60s" {
		t.Errorf("poll interval = %q, want %q", settings.values["POLL_INTERVAL"], "60s")
	}

	// The watchdog section no longer applies and the operator should be told
	// rather than left wondering why it had no effect.
	var mentionedWatchdog bool
	for _, note := range settings.notes {
		if strings.Contains(note, "watchdog") {
			mentionedWatchdog = true
		}
	}
	if !mentionedWatchdog {
		t.Errorf("expected a note about the ignored [watchdog] section, got %v", settings.notes)
	}
}

// TestLegacyAcceptsBothInfluxUserKeys covers a documented inconsistency in the
// older exporter, which read "username" while its shipped example used "user".
func TestLegacyAcceptsBothInfluxUserKeys(t *testing.T) {
	t.Parallel()

	for _, key := range []string{"user", "username"} {
		path := writeTemp(t, "config.ini",
			"[influxdb]\nurl = http://x:8086\n"+key+" = alice\ndatabase = d\n")

		settings, err := LoadConfigFile(path)
		if err != nil {
			t.Fatalf("%s: %v", key, err)
		}
		if got := settings.values["INFLUX1_USER"]; got != "alice" {
			t.Errorf("key %q gave INFLUX1_USER=%q, want alice", key, got)
		}
	}
}

// TestNativeSchema covers a file written against this exporter's own settings.
func TestNativeSchema(t *testing.T) {
	t.Parallel()

	// Bare keys before any section are top-level. A GOGMC_-prefixed key is
	// fully qualified and keeps its meaning wherever it appears.
	path := writeTemp(t, "config.ini", `# comment
; also a comment

POLL_INTERVAL = 90s

[serial]
port = /dev/ttyUSB1
baud = 57600

[prometheus]
enabled = true

[radmon]
user = "quoted-user"
password = 'single-quoted'
GOGMC_LOG_LEVEL = debug
`)

	settings, err := LoadConfigFile(path)
	if err != nil {
		t.Fatalf("LoadConfigFile: %v", err)
	}
	if settings.legacy {
		t.Fatal("a native file was mistaken for the legacy schema")
	}

	want := map[string]string{
		"SERIAL_PORT":        "/dev/ttyUSB1",
		"SERIAL_BAUD":        "57600",
		"PROMETHEUS_ENABLED": "true",
		"RADMON_USER":        "quoted-user",
		"RADMON_PASSWORD":    "single-quoted",
		"POLL_INTERVAL":      "90s",
		"LOG_LEVEL":          "debug",
	}
	for key, expected := range want {
		if got := settings.values[key]; got != expected {
			t.Errorf("%s = %q, want %q", key, got, expected)
		}
	}
}

// TestEnvironmentBeatsFile pins the documented precedence: flags, then
// environment, then file, then defaults.
//
// A container must be able to override a mounted file without editing it,
// which is how the same file gets reused across environments.
func TestEnvironmentBeatsFile(t *testing.T) {
	t.Parallel()

	l := newLoader()
	l.fileValues = map[string]string{
		"SERIAL_PORT":   "/dev/from-file",
		"POLL_INTERVAL": "30s",
	}
	l.lookupEnv = func(key string) (string, bool) {
		if key == EnvPrefix+"SERIAL_PORT" {
			return "/dev/from-env", true
		}
		return "", false
	}

	if got := l.String("SERIAL_PORT", "/dev/default"); got != "/dev/from-env" {
		t.Errorf("SERIAL_PORT = %q, want the environment to win", got)
	}
	// A setting absent from the environment still comes from the file.
	if got := l.String("POLL_INTERVAL", "60s"); got != "30s" {
		t.Errorf("POLL_INTERVAL = %q, want the file value", got)
	}
	// A setting in neither falls back to the default.
	if got := l.String("LOG_FORMAT", "json"); got != "json" {
		t.Errorf("LOG_FORMAT = %q, want the default", got)
	}
}

// TestConfigFileSecretsAreNotLogged checks a credential read from a config
// file is redacted the same way one read from the environment is.
func TestConfigFileSecretsAreNotLogged(t *testing.T) {
	t.Parallel()

	const canary = "zzCONFIGFILE-canary-NEVERLOG"

	l := newLoader()
	l.fileValues = map[string]string{"RADMON_PASSWORD": canary}
	l.lookupEnv = func(string) (string, bool) { return "", false }

	secret := l.Secret("RADMON_PASSWORD")
	if secret.Reveal() != canary {
		t.Fatal("the secret was not read from the config file")
	}
	for _, rec := range l.resolved {
		if strings.Contains(rec.Display, canary) {
			t.Errorf("a config-file secret was recorded for logging: %+v", rec)
		}
	}
}

// TestMalformedFileIsReported checks a broken file fails loudly.
func TestMalformedFileIsReported(t *testing.T) {
	t.Parallel()

	path := writeTemp(t, "config.ini", "[main]\nthis line has no equals sign\n")
	if _, err := LoadConfigFile(path); err == nil {
		t.Fatal("a malformed config file should be rejected")
	}

	if _, err := LoadConfigFile(filepath.Join(t.TempDir(), "absent.ini")); err == nil {
		t.Fatal("a missing config file named explicitly should be an error")
	}
}
