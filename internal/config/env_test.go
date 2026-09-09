package config

import (
	"errors"
	"os"
	"strings"
	"testing"
	"time"
)

// newTestLoader builds a loader backed by fixed maps rather than the real
// process environment, which is the reason lookupEnv and readFile are
// injectable: the tests stay parallel-safe and cannot disturb one another.
//
// Keys in env are given without the GOGMC_ prefix for brevity.
func newTestLoader(env map[string]string, files map[string]string) *loader {
	return &loader{
		lookupEnv: func(key string) (string, bool) {
			v, ok := env[strings.TrimPrefix(key, EnvPrefix)]
			return v, ok
		},
		readFile: func(path string) ([]byte, error) {
			content, ok := files[path]
			if !ok {
				return nil, errors.New("no such file or directory")
			}
			return []byte(content), nil
		},
	}
}

// errorText joins the loader's accumulated errors for substring assertions.
func errorText(l *loader) string {
	return strings.Join(errorStrings(l.errs), "\n")
}

func requireNoErrors(t *testing.T, l *loader) {
	t.Helper()
	if len(l.errs) != 0 {
		t.Fatalf("unexpected errors: %s", errorText(l))
	}
}

// requireError asserts exactly one error was recorded and that it names the
// prefixed setting, so an operator can find the variable it is talking about.
func requireError(t *testing.T, l *loader, mustContain ...string) {
	t.Helper()
	if len(l.errs) != 1 {
		t.Fatalf("error count = %d, want 1: %s", len(l.errs), errorText(l))
	}
	got := errorText(l)
	for _, want := range mustContain {
		if !strings.Contains(got, want) {
			t.Fatalf("error %q does not contain %q", got, want)
		}
	}
	if !strings.Contains(got, EnvPrefix) {
		t.Fatalf("error %q does not name the setting with its %s prefix", got, EnvPrefix)
	}
}

// lastRecorded returns the provenance entry the reader just wrote.
func lastRecorded(t *testing.T, l *loader) resolved {
	t.Helper()
	if len(l.resolved) == 0 {
		t.Fatal("no setting was recorded for the startup log")
	}
	return l.resolved[len(l.resolved)-1]
}

func TestString(t *testing.T) {
	l := newTestLoader(map[string]string{"SERIAL_PORT": "/dev/ttyACM0"}, nil)
	if got := l.String("SERIAL_PORT", "/dev/ttyUSB0"); got != "/dev/ttyACM0" {
		t.Fatalf("String = %q, want /dev/ttyACM0", got)
	}
	if got := l.String("MISSING", "fallback"); got != "fallback" {
		t.Fatalf("String with no value = %q, want the default", got)
	}
	requireNoErrors(t, l)

	if rec := lastRecorded(t, l); rec.Source != sourceDefault || rec.Key != EnvPrefix+"MISSING" {
		t.Fatalf("recorded %+v, want the default source for %sMISSING", rec, EnvPrefix)
	}
}

func TestBool(t *testing.T) {
	valid := map[string]bool{"true": true, "TRUE": true, "1": true, " false ": false, "0": false}
	for raw, want := range valid {
		l := newTestLoader(map[string]string{"X": raw}, nil)
		if got := l.Bool("X", !want); got != want {
			t.Fatalf("Bool(%q) = %v, want %v", raw, got, want)
		}
		requireNoErrors(t, l)
	}

	l := newTestLoader(map[string]string{"X": "yes please"}, nil)
	if got := l.Bool("X", true); got != true {
		t.Fatalf("Bool of an invalid value = %v, want the default", got)
	}
	requireError(t, l, "X", "is not a boolean")

	def := newTestLoader(nil, nil)
	if got := def.Bool("X", true); !got {
		t.Fatalf("Bool with no value = %v, want the default true", got)
	}
	requireNoErrors(t, def)
}

func TestInt(t *testing.T) {
	l := newTestLoader(map[string]string{"X": " 42 "}, nil)
	if got := l.Int("X", 1, 0, 100); got != 42 {
		t.Fatalf("Int = %d, want 42", got)
	}
	requireNoErrors(t, l)

	bad := newTestLoader(map[string]string{"X": "forty-two"}, nil)
	if got := bad.Int("X", 7, 0, 100); got != 7 {
		t.Fatalf("Int of an unparseable value = %d, want the default 7", got)
	}
	requireError(t, bad, "X", "is not a whole number")

	for _, raw := range []string{"-1", "101"} {
		out := newTestLoader(map[string]string{"X": raw}, nil)
		if got := out.Int("X", 7, 0, 100); got != 7 {
			t.Fatalf("Int(%s) = %d, want the default 7", raw, got)
		}
		requireError(t, out, "X", "outside the accepted range 0..100")
	}

	// The range is inclusive at both ends.
	for _, raw := range []string{"0", "100"} {
		edge := newTestLoader(map[string]string{"X": raw}, nil)
		edge.Int("X", 7, 0, 100)
		requireNoErrors(t, edge)
	}
}

func TestDuration(t *testing.T) {
	l := newTestLoader(map[string]string{"X": " 90s "}, nil)
	if got := l.Duration("X", time.Minute, time.Second, time.Hour); got != 90*time.Second {
		t.Fatalf("Duration = %s, want 90s", got)
	}
	requireNoErrors(t, l)

	bad := newTestLoader(map[string]string{"X": "90"}, nil)
	if got := bad.Duration("X", time.Minute, time.Second, time.Hour); got != time.Minute {
		t.Fatalf("Duration of a bare number = %s, want the default", got)
	}
	requireError(t, bad, "X", "is not a duration")

	for _, raw := range []string{"500ms", "2h"} {
		out := newTestLoader(map[string]string{"X": raw}, nil)
		if got := out.Duration("X", time.Minute, time.Second, time.Hour); got != time.Minute {
			t.Fatalf("Duration(%s) = %s, want the default", raw, got)
		}
		requireError(t, out, "X", "outside the accepted range")
	}
}

func TestFloat(t *testing.T) {
	l := newTestLoader(map[string]string{"X": " 1.5 "}, nil)
	if got := l.Float("X", 0, -10, 10); got != 1.5 {
		t.Fatalf("Float = %v, want 1.5", got)
	}
	requireNoErrors(t, l)

	bad := newTestLoader(map[string]string{"X": "one point five"}, nil)
	if got := bad.Float("X", 3, -10, 10); got != 3 {
		t.Fatalf("Float of an unparseable value = %v, want the default 3", got)
	}
	requireError(t, bad, "X", "is not a number")

	out := newTestLoader(map[string]string{"X": "11"}, nil)
	if got := out.Float("X", 3, -10, 10); got != 3 {
		t.Fatalf("Float out of range = %v, want the default 3", got)
	}
	requireError(t, out, "X", "outside the accepted range")

	def := newTestLoader(nil, nil)
	if got := def.Float("X", 3, -10, 10); got != 3 {
		t.Fatalf("Float with no value = %v, want the default 3", got)
	}
	requireNoErrors(t, def)
}

// TestOptionalFloat covers the distinction that makes latitude workable: an
// unset coordinate must be reported as absent rather than as a deliberate zero,
// since zero is a real place.
func TestOptionalFloat(t *testing.T) {
	l := newTestLoader(map[string]string{"LATITUDE": "0"}, nil)
	got, ok := l.OptionalFloat("LATITUDE", -90, 90)
	if !ok || got != 0 {
		t.Fatalf("OptionalFloat(\"0\") = %v, %v; want 0, true", got, ok)
	}
	requireNoErrors(t, l)

	for _, raw := range []string{"", "   "} {
		blank := newTestLoader(map[string]string{"LATITUDE": raw}, nil)
		if _, ok := blank.OptionalFloat("LATITUDE", -90, 90); ok {
			t.Fatalf("OptionalFloat(%q) reported a value", raw)
		}
		requireNoErrors(t, blank)
	}

	unset := newTestLoader(nil, nil)
	if _, ok := unset.OptionalFloat("LATITUDE", -90, 90); ok {
		t.Fatal("OptionalFloat with no value reported a value")
	}
	requireNoErrors(t, unset)

	bad := newTestLoader(map[string]string{"LATITUDE": "north"}, nil)
	if _, ok := bad.OptionalFloat("LATITUDE", -90, 90); ok {
		t.Fatal("OptionalFloat of an unparseable value reported a value")
	}
	requireError(t, bad, "LATITUDE", "is not a number")

	out := newTestLoader(map[string]string{"LATITUDE": "91"}, nil)
	if _, ok := out.OptionalFloat("LATITUDE", -90, 90); ok {
		t.Fatal("OptionalFloat out of range reported a value")
	}
	requireError(t, out, "LATITUDE", "outside the accepted range")
}

func TestStringMap(t *testing.T) {
	l := newTestLoader(map[string]string{"OTLP_HEADERS": "authorization=Bearer abc, x-tenant = acme ,"}, nil)
	got := l.StringMap("OTLP_HEADERS")
	requireNoErrors(t, l)
	if len(got) != 2 {
		t.Fatalf("StringMap = %v, want two headers", got)
	}
	if got["authorization"] != "Bearer abc" || got["x-tenant"] != "acme" {
		t.Fatalf("StringMap = %v, want trimmed keys and values", got)
	}

	// Header values routinely carry authorization tokens, so only the names
	// may reach the startup log.
	rec := lastRecorded(t, l)
	if strings.Contains(rec.Display, "Bearer abc") {
		t.Fatalf("StringMap recorded %q, which contains a header value", rec.Display)
	}
	if !strings.Contains(rec.Display, "authorization") {
		t.Fatalf("StringMap recorded %q, want it to name the headers", rec.Display)
	}

	for _, raw := range []string{"", "   "} {
		blank := newTestLoader(map[string]string{"OTLP_HEADERS": raw}, nil)
		if got := blank.StringMap("OTLP_HEADERS"); got != nil {
			t.Fatalf("StringMap(%q) = %v, want nil", raw, got)
		}
		requireNoErrors(t, blank)
	}

	unset := newTestLoader(nil, nil)
	if got := unset.StringMap("OTLP_HEADERS"); got != nil {
		t.Fatalf("StringMap with no value = %v, want nil", got)
	}
	requireNoErrors(t, unset)

	noEquals := newTestLoader(map[string]string{"OTLP_HEADERS": "authorization"}, nil)
	if got := noEquals.StringMap("OTLP_HEADERS"); got != nil {
		t.Fatalf("StringMap without key=value = %v, want nil", got)
	}
	requireError(t, noEquals, "OTLP_HEADERS", "is not in key=value form")

	emptyKey := newTestLoader(map[string]string{"OTLP_HEADERS": "=value"}, nil)
	if got := emptyKey.StringMap("OTLP_HEADERS"); got != nil {
		t.Fatalf("StringMap with an empty key = %v, want nil", got)
	}
	requireError(t, emptyKey, "OTLP_HEADERS", "empty key")

	onlySeparators := newTestLoader(map[string]string{"OTLP_HEADERS": " , , "}, nil)
	if got := onlySeparators.StringMap("OTLP_HEADERS"); got != nil {
		t.Fatalf("StringMap of separators only = %v, want nil", got)
	}
	requireNoErrors(t, onlySeparators)
}

func TestEnum(t *testing.T) {
	l := newTestLoader(map[string]string{"LOG_LEVEL": " DEBUG "}, nil)
	if got := l.Enum("LOG_LEVEL", "info", "debug", "info", "warn", "error"); got != "debug" {
		t.Fatalf("Enum = %q, want the case-folded debug", got)
	}
	requireNoErrors(t, l)

	bad := newTestLoader(map[string]string{"LOG_LEVEL": "verbose"}, nil)
	if got := bad.Enum("LOG_LEVEL", "info", "debug", "info", "warn", "error"); got != "info" {
		t.Fatalf("Enum of an unknown value = %q, want the default", got)
	}
	requireError(t, bad, "LOG_LEVEL", "is not one of", "debug, info, warn, error")

	def := newTestLoader(nil, nil)
	if got := def.Enum("LOG_LEVEL", "info", "debug", "info"); got != "info" {
		t.Fatalf("Enum with no value = %q, want the default", got)
	}
	requireNoErrors(t, def)
}

// TestSecretIsNeverRecorded checks the property the whole redact package exists
// for: the value must reach the caller but never the startup log.
func TestSecretIsNeverRecorded(t *testing.T) {
	const canary = "canary-p4ssw0rd-do-not-log"

	l := newTestLoader(map[string]string{"RADMON_PASSWORD": canary}, nil)
	got := l.Secret("RADMON_PASSWORD")
	requireNoErrors(t, l)

	if got.Reveal() != canary {
		t.Fatalf("Secret.Reveal = %q, want the configured value", got.Reveal())
	}
	if got.IsZero() {
		t.Fatal("Secret reports itself zero despite holding a value")
	}
	if rec := lastRecorded(t, l); strings.Contains(rec.Display, canary) {
		t.Fatalf("secret recorded as %q, which leaks the value", rec.Display)
	}

	unset := newTestLoader(nil, nil)
	if s := unset.Secret("RADMON_PASSWORD"); !s.IsZero() {
		t.Fatal("Secret with no value is not zero")
	}
	requireNoErrors(t, unset)
	if rec := lastRecorded(t, unset); rec.Display != "(unset)" {
		t.Fatalf("unset secret recorded as %q, want (unset)", rec.Display)
	}
}

// TestFileVariantReadsFromFile covers the container case: Docker and Kubernetes
// deliver secrets as files, not as environment variables.
func TestFileVariantReadsFromFile(t *testing.T) {
	l := newTestLoader(
		map[string]string{"RADMON_PASSWORD" + FileSuffix: "/run/secrets/radmon"},
		map[string]string{"/run/secrets/radmon": "hunter2"},
	)
	if got := l.Secret("RADMON_PASSWORD").Reveal(); got != "hunter2" {
		t.Fatalf("value from file = %q, want hunter2", got)
	}
	requireNoErrors(t, l)
	if rec := lastRecorded(t, l); rec.Source != sourceFile {
		t.Fatalf("source = %q, want %q", rec.Source, sourceFile)
	}
}

// TestFileVariantStripsTrailingNewline matters because a secret file written by
// any ordinary editor or `echo` ends in one, and a newline in a password
// produces an authentication failure that looks nothing like its cause.
func TestFileVariantStripsTrailingNewline(t *testing.T) {
	cases := map[string]string{
		"unix newline":       "hunter2\n",
		"windows newline":    "hunter2\r\n",
		"repeated newlines":  "hunter2\n\n\n",
		"no trailing newlin": "hunter2",
	}
	for name, content := range cases {
		t.Run(name, func(t *testing.T) {
			l := newTestLoader(
				map[string]string{"RADMON_PASSWORD" + FileSuffix: "/secret"},
				map[string]string{"/secret": content},
			)
			if got := l.Secret("RADMON_PASSWORD").Reveal(); got != "hunter2" {
				t.Fatalf("value = %q, want hunter2", got)
			}
			requireNoErrors(t, l)
		})
	}

	// Leading whitespace is part of the value; only trailing newlines are
	// assumed to be an artefact of how the file was written.
	l := newTestLoader(
		map[string]string{"RADMON_PASSWORD" + FileSuffix: "/secret"},
		map[string]string{"/secret": " spaced \n"},
	)
	if got := l.Secret("RADMON_PASSWORD").Reveal(); got != " spaced " {
		t.Fatalf("value = %q, want %q", got, " spaced ")
	}
}

// TestFileVariantUnreadableNamesPathNotContents is the security-relevant half:
// the path is useful in a log and the contents never are.
func TestFileVariantUnreadableNamesPathNotContents(t *testing.T) {
	const canary = "canary-secret-contents"
	l := &loader{
		lookupEnv: func(key string) (string, bool) {
			if key == EnvPrefix+"RADMON_PASSWORD"+FileSuffix {
				return "/run/secrets/radmon", true
			}
			return "", false
		},
		// The error text carries the secret, imitating a reader that
		// mistakenly includes what it read in its failure message.
		readFile: func(string) ([]byte, error) {
			return []byte(canary), errors.New("permission denied")
		},
	}

	if got := l.Secret("RADMON_PASSWORD"); !got.IsZero() {
		t.Fatal("an unreadable secret file still produced a value")
	}
	requireError(t, l, "RADMON_PASSWORD"+FileSuffix, "/run/secrets/radmon", "permission denied")
	if strings.Contains(errorText(l), canary) {
		t.Fatalf("error text leaked the file contents: %s", errorText(l))
	}
}

// TestFileVariantEmptyPath catches the common compose mistake of declaring the
// variable without giving it a path.
func TestFileVariantEmptyPath(t *testing.T) {
	for _, raw := range []string{"", "   "} {
		l := newTestLoader(map[string]string{"RADMON_PASSWORD" + FileSuffix: raw}, nil)
		if got := l.Secret("RADMON_PASSWORD"); !got.IsZero() {
			t.Fatalf("empty %s path produced a value", FileSuffix)
		}
		requireError(t, l, "RADMON_PASSWORD"+FileSuffix, "is set but empty")
	}
}

// TestBothVariantsSetIsAnError proves the loader refuses to guess. Silently
// preferring one is how the wrong credential reaches production.
func TestBothVariantsSetIsAnError(t *testing.T) {
	l := newTestLoader(
		map[string]string{
			"RADMON_PASSWORD":              "from-env",
			"RADMON_PASSWORD" + FileSuffix: "/secret",
		},
		map[string]string{"/secret": "from-file"},
	)

	got := l.Secret("RADMON_PASSWORD")
	if !got.IsZero() {
		t.Fatal("a conflicting pair still produced a value; the loader picked one")
	}
	requireError(t, l, EnvPrefix+"RADMON_PASSWORD", "both set", "supply exactly one")
	if strings.Contains(errorText(l), "from-env") || strings.Contains(errorText(l), "from-file") {
		t.Fatalf("conflict error leaked a value: %s", errorText(l))
	}
}

// TestFileVariantAppliesToNonSecretsToo confirms _FILE is a property of the
// loader, not of Secret specifically.
func TestFileVariantAppliesToNonSecretsToo(t *testing.T) {
	l := newTestLoader(
		map[string]string{"POLL_INTERVAL" + FileSuffix: "/conf/interval"},
		map[string]string{"/conf/interval": "30s\n"},
	)
	if got := l.Duration("POLL_INTERVAL", time.Minute, time.Second, time.Hour); got != 30*time.Second {
		t.Fatalf("Duration from file = %s, want 30s", got)
	}
	requireNoErrors(t, l)
}

func TestRequireAndRequireSecret(t *testing.T) {
	l := newTestLoader(nil, nil)
	l.require("radmon.org", "RADMON_USER", "  ")
	requireError(t, l, "radmon.org is enabled", EnvPrefix+"RADMON_USER")

	ok := newTestLoader(nil, nil)
	ok.require("radmon.org", "RADMON_USER", "doug")
	requireNoErrors(t, ok)

	sec := newTestLoader(nil, nil)
	sec.requireSecret("Safecast", "SAFECAST_API_KEY", newTestLoader(nil, nil).Secret("SAFECAST_API_KEY"))
	requireError(t, sec, "Safecast is enabled",
		EnvPrefix+"SAFECAST_API_KEY", EnvPrefix+"SAFECAST_API_KEY"+FileSuffix)

	secOK := newTestLoader(map[string]string{"SAFECAST_API_KEY": "abc"}, nil)
	key := secOK.Secret("SAFECAST_API_KEY")
	secOK.requireSecret("Safecast", "SAFECAST_API_KEY", key)
	requireNoErrors(t, secOK)
}

// TestNewLoaderUsesTheRealEnvironment guards the wiring the injectable fields
// replace; everything else in this file bypasses it.
func TestNewLoaderUsesTheRealEnvironment(t *testing.T) {
	t.Setenv(EnvPrefix+"SERIAL_PORT", "/dev/ttyTEST")
	l := newLoader()
	if got := l.String("SERIAL_PORT", "/dev/ttyUSB0"); got != "/dev/ttyTEST" {
		t.Fatalf("String from the real environment = %q, want /dev/ttyTEST", got)
	}
	if l.readFile == nil {
		t.Fatal("readFile is nil; the _FILE variants would panic")
	}

	path := t.TempDir() + "/secret"
	if err := os.WriteFile(path, []byte("value\n"), 0o600); err != nil {
		t.Fatalf("writing the fixture: %v", err)
	}
	t.Setenv(EnvPrefix+"RADMON_PASSWORD"+FileSuffix, path)
	if got := l.Secret("RADMON_PASSWORD").Reveal(); got != "value" {
		t.Fatalf("Secret from a real file = %q, want value", got)
	}
}
